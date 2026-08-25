# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

gosmokeping is a single-binary Go replacement for SmokePing. It probes network
targets (ICMP/TCP/HTTP/DNS), writes results to ClickHouse, serves a React+uPlot
UI plus a JSON API, and can fire threshold alerts.

## Commands

```bash
make ui                 # vite build → internal/ui/dist/ (required before `make build`)
make build              # builds UI first, then `go build`
make build-nui          # Go-only build (UI must already be built or dist empty)
make dev                # go run with -log-level debug
make ui-dev             # vite dev server on :5173, proxies /api to :8080

make test               # unit tests
make test-integration   # needs CLICKHOUSE_ADDR (see Integration tests section)
make lint               # go vet
make tidy               # go mod tidy
go test ./internal/api -run TestHealth  # single test

./gosmokeping -config config.json               # run master/standalone
./gosmokeping --slave -config config.slave.json # run as cluster slave
```

ICMP needs raw sockets; `make setcap` grants `CAP_NET_RAW` to the local binary.

## Architecture

The binary is composed of packages that feed data through a single pipeline:

```
config.Load → probe.Registry → scheduler.Scheduler → Fanout(sinks) → {LogSink, storage.Writer, alert.Evaluator}
                                                                                      ↑
                                                   api.Server ← storage.Reader        ↑
                                                                                 alert.Dispatcher
```

Key points a reader can't derive from a single file:

- **Scheduler-as-hub:** `scheduler.Scheduler` is the only thing that drives
  probes. Everything downstream (storage writes, alert evaluation) plugs in as
  a `scheduler.Sink` via `scheduler.Fanout`. To add a new consumer of probe
  results, implement `Sink.OnCycle` and append it to the `sinks` slice in
  `cmd/gosmokeping/run_node.go`. Slave-inbound cycles from the cluster ingest
  handler are written to the **same** fanout, so any new sink automatically
  sees both local and remote cycles.

- **Config hot-reload:** `config.Store` uses `atomic.Pointer[Config]`.
  Consumers (API, alert evaluator) call `store.Current()` on every request
  rather than caching — this is intentional so `SIGHUP` takes effect
  immediately without pointer-update races. When adding a consumer, do
  **not** cache the `*Config`.

- **Target identity is (group, name), never name alone.** All four tables
  carry `target_id` (the name) *and* `target_group`, and every read scopes
  by both. Name alone is not unique: config dedupes on `group/name`, so
  `core-backbone/frankfurt` and `retn/frankfurt` are legal and were served
  identical merged rows from two unrelated networks until the group
  predicate was added. It is also a security boundary — slave-health
  targets live in `_cluster` and are named after the slave, and the API's
  hop-address redaction keys on group, so a user target sharing a slave's
  name could read unredacted health hops. `target_id` is in every
  `ORDER BY`, which is why the group is a separate column rather than a
  composite id: ClickHouse refuses `ALTER … UPDATE` on a key column, so
  re-keying would mean rebuilding every table. Detail rows written before
  `target_group` existed read as `""` and are deliberately unreachable
  rather than matched by a `target_group = ''` fallback — their real group
  was never recorded, so that fallback would re-merge them *and* reinstate
  the health-hop disclosure. `probe_cycle` is unaffected: it has always
  written the group, so its full retention stays visible.

  `internal/storage/clickhouse/reader_args_test.go` guards the class this
  change nearly shipped: query text and its arg slice are built in
  separate places, so it injects a fake `driver.Conn` and asserts
  placeholder count equals arg count across filter permutations. Without
  it an added `?` with no argument only fails against a live server.

- **Storage backend:** single ClickHouse backend in `internal/storage/clickhouse/`.
  Four `MergeTree` tables (`probe_cycle`, `probe_rtt`, `probe_hop`, `probe_http`)
  with codec-stacked columns (Gorilla for floats, T64 for small ints,
  DoubleDelta for timestamps, ZSTD as second pass). The reader buckets at
  query time via `toStartOfInterval` — no materialised views, no rollup
  tasks. `QueryFilter.Step` carries the bucket width; `storage.PickCycleStep`
  and `storage.PickHopStep` (in `internal/storage/backend.go`) are the
  single decision points, called from the API layer. Tier ladders:
  - cycles: ≤2h raw, ≤24h 2m, ≤7d 15m, ≤30d 1h, ≤180d 6h, >180d 1d
  - hops:   ≤2h raw, ≤24h 5m, >24h 15m

  **Write buffers.** Each table's channel is sized by
  `writerChanCap(table, pings)` (base 4096 slots × a rows-per-cycle factor,
  clamped to [4096, 131072]) so all four absorb a comparable ClickHouse
  stall at ordinary rates — a flat 4096 gave the deployed 122-target/20s
  install 11 minutes of cycle buffer against 96 seconds of hop buffer, and
  hops died first. The hop factor is clamp-limited by design: 4096×32 is
  the ceiling exactly, so a larger factor is inert and worst-case hop cycles
  (90 rows for icmp's 3×30 walk, 300 for MTR's 10×30) still overflow first
  — drop-oldest and the counters are the bound there, not the buffer. Those
  per-table counters are served as `writer_drops` on `/api/v1/health`.

  **Window caps.** The binary ships no auth, so on every endpoint that
  returns unbucketed rows the window cap is the only bound on an
  anonymous request's scan: `/rtts` 24h (`api.maxRTTWindow`), `/http`
  and `/hops/timeline` 7d, and `?step=raw` on `/cycles` bounded by
  `storage.PickCycleStep(span) == 0` rather than a second copy of the
  2h threshold, so widening the raw tier widens the override with it.
  `/rtts` is tighter than its 7d siblings because `probe_rtt` stores a
  row per ping, not per cycle. Measured against a 122-target, 5-source
  install at a 15s interval: `/rtts?from=-30d` returned 206 MB and
  `/cycles?from=-30d&step=raw` 199 MB, both still streaming when cut at
  45s, from single unauthenticated GETs. Bucketed `/cycles` needs no cap
  — the ladder holds it to ~500–1000 points at any width.

  **Query admission.** Window caps bound one request's scan; they don't
  bound how many run at once. A `CachingReader` miss detaches its inner
  query via `context.WithoutCancel` and keeps running up to
  `queryMaxDuration` (5m) after the caller disconnects, and every cache-key
  field is request-controlled, so `storage.maxInflightLeaders` caps
  concurrently-detached queries at 32 per cache (32 cycles + 32 hops).
  Only *leaders* are admitted against it — waiters join a leader already
  paid for, and refusing them would turn a stampede on one hot key into
  errors. Past the cap the reader returns `storage.ErrOverloaded`, which
  the API maps to 503 + `Retry-After` rather than the 502 every other
  reader error gets: it is backpressure, not a broken upstream.
  The cycle ladder targets ~500–1000 buckets per window so point density
  stays roughly constant as the user zooms out — no >7× cliff at any
  boundary. Smoothing happens server-side via the weighted percentile
  aggregation; client-side smoothing is intentionally avoided so the
  p95/max band keeps showing real outliers rather than averaging them
  away.
  Bucketed cycle percentiles are computed with `quantilesExactWeighted`
  over the per-cycle percentile columns weighted by `sent` — information-
  preserving relative to NULL. Both bucketed queries keep `source` in
  the `GROUP BY` so each (bucket, source) tuple stays a distinct row —
  without it, master/slave data would be mixed together with `any(source)`
  picking an arbitrary label and the UI would lose per-source bands.
  Bucketed hop queries additionally keep `hop_addr` in the `GROUP BY` so
  a path flap returns one row per distinct address seen in the bucket.

- **Retention:** per-table TTL set at bootstrap from
  `storage.clickhouse.retention.{cycle,rtt,hop,http}_days` (defaults
  365/14/90/14). `Bootstrap` re-emits `ALTER TABLE … MODIFY TTL` on every
  start so a config change takes effect on the next process restart. The
  writer/reader bind the rest of the config once at startup.

- **CH cluster mode:** `storage.clickhouse.cluster` empty = single-node
  `MergeTree`. Non-empty = `ON CLUSTER <name>` + `ReplicatedMergeTree`
  with conventional `{shard}/{replica}` paths. Independent of
  gosmokeping's master/slave cluster.

- **UI embed:** `internal/ui/ui.go` uses `//go:embed all:dist` against
  `internal/ui/dist/`. That directory must exist at build time, so the
  repo keeps a `.gitkeep` in it. `FS()` returns nil when dist is empty,
  letting the API run headless (useful for dev / container builds that
  don't need the UI).

- **Icon:** `npm run icon` in `ui/` regenerates `ui/public/favicon.svg`
  (16 bars, for a tab) and `ui/public/icon.svg` (64 bars, for anywhere it
  is shown large); Vite copies `public/` into the embedded dist. The bar
  pattern is deterministic and synthetic on purpose — the icon ships to
  every user of this tool, so it must not encode one operator's probe
  list. Colours are three entries of `ui/src/palette.ts` read as a latency
  ramp, keeping the icon inside the same CVD-gated set as the charts.

- **Alert state is in-memory only:** `alert.Evaluator` stores per-target
  state in a map. After a restart all alerts return to `StateOK` — no
  persistence in v1. This avoids replaying cycles from storage, at the cost
  of missing the "still firing" state across restarts.

- **ICMP sockets:** `probe.listen` prefers unprivileged UDP ping sockets
  (`udp4`/`udp6`) before falling back to raw ICMP. When using UDP sockets,
  the kernel rewrites the ICMP ID to the source port, so `sendOne` matches
  replies by **sequence number only**, not ID. Don't "fix" this — it's
  correct for both socket types.

- **Path discovery (MTR + opportunistic trace):** `probe.traceHops` is the
  shared TTL-walk helper in `internal/probe/trace.go`; the round loop itself
  is `walkRounds`, driven through an injected `step` so tests exercise the
  production loop rather than a copy. It returns `roundStats` — the rounds that
  actually sent a probe and the subset the target echoed in — and `MTR` reports
  `Sent`/`LossCount` straight from those two counters, never from the hop rows:
  a round that walks past the target's old TTL folds its loss onto the marked
  row there, so summing marked rows counts one round once per TTL the target
  ever answered at, and a lengthening route reads as loss it never suffered.
  The `ICMP` probe calls the walk concurrently with its echo batch so every
  icmp target also gets a hops view for free, and discards the counters.
  Trace needs `CAP_NET_RAW` — callers distinguish the *permission* error with
  `errors.Is(err, errRawUnavailable)` and skip gracefully, while every other
  raw-socket failure stays loud. Concurrent because both share the cycle
  deadline — run sequentially, a loss-saturated echo batch left the walk zero
  budget and it returned no hops at all.

  **Each round runs to its own terminal**, an echo reply *or* an ICMP
  unreachable, with no cross-round TTL clamp: a route that lengthens
  mid-cycle gets its new hops probed and one that shortens keeps the rows the
  longer path already measured. The unreachable's reporting gateway is the
  last hop, annotated with a closed-set `Unreach` label (`unreachLabel`,
  RFC 792 / RFC 4443 codes normalized across families); walking past it
  re-elicited that same gateway at every deeper TTL and fabricated a clean
  30-hop path at zero loss out of one router. An unreachable never counts a
  round as reached, so MTR still reports full target loss.

  Rows are per `(ttl, responder address)` in first-seen order, so ECMP
  siblings each carry their own samples; a TTL's losses have no responder to
  blame and fold onto its first-seen row, which keeps single-responder
  numbers identical to the pre-split shape, and a TTL nothing answered emits
  one `IP: ""` row. Rows the target itself answered carry `TargetReply` —
  the target's row is no longer guaranteed to be the deepest, so `/hops`
  redaction, MTR's RTT mirror, and the UI's end-to-end loss all key on that
  marker instead of on position.

  **An echo reply counts as the target's only if its peer is the resolved
  destination.** `matchDatagram` is the read path's whole trust boundary —
  bytes and source address both come off the wire — and it sits apart from
  `sendTTL`'s read loop precisely so hostile input can drive it without a
  socket. Type, id and seq are all visible to any router on the path, so one
  could answer from its own address; the round stopped there, the row was
  marked `TargetReply`, and MTR reported a target that never replied. That
  marker is persisted and drives `/hops` redaction, so it is a disclosure
  input, not only a display one. `TimeExceeded` and unreachable are exempt —
  they legitimately come from routers along the path. Addresses compare as
  unmapped `netip.Addr` via `peerAddr`, which is what reconciles the socket
  asymmetry: an unprivileged ping socket reports `*net.UDPAddr` (and the
  kernel has rewritten the ICMP id), a raw one `*net.IPAddr`. Any other shape
  resolves to the invalid zero `Addr` and matches nothing — without that,
  `Addr.String()` would write the literal `"invalid IP"` into `probe_hop` as
  an error's hop address.

- **ICMP cycle budget:** the echo batch and the TTL walk run concurrently
  and share one context whose deadline is `cfg.Interval`. Each echo's
  deadline is `min(probe timeout, (remaining cycle − spacing still owed) /
  pings still to send)`, recomputed per ping so the batch self-levels into
  the interval: a ping that answers fast returns its unused share to the
  ones after it, and the configured timeout survives on every cycle the
  schedule can afford it. `Sent` counts pings **attempted**, so it usually
  equals `cfg.Pings` now that the batch cannot overrun — but still
  under-reports when the context is cancelled, when the cycle expires during
  spacing, or when DNS and socket setup already spent the deadline. Do not
  "fix" that by pre-setting `Sent`.
  `config.ICMPPingBudget` refuses a schedule whose full-loss derived budget
  `(interval − (pings−1)×config.ICMPPingSpacing) / pings` falls below
  `config.MinPingBudget` (50ms). It lives in `config` because `config.Validate`
  calls it: a schedule the icmp probe cannot serve must never be stored, or
  the master serves it to every slave and the whole fleet fails at its next
  restart while the operator sees green. `probe.Build` calls the same function
  as defence in depth — a slave builds from a master-supplied config it never
  validated. Both gate on the config defining an icmp probe; neither binds a
  config with none. `pings` is bounded twice before that multiplication: at
  `interval/200ms + 1`, which the product would otherwise overflow into a
  passing budget, and at `config.MaxPingsPerCycle`, the echo sequence space
  left above the TTL walk's window, which a long enough interval would
  otherwise admit into `echoBaseSeq`'s degenerate branch (`probe` pins that
  ceiling to the walk's own bounds with a compile-time assertion). Reload is
  fail-closed at both layers: `Store.Reload` keeps the last-good config, and
  `scheduler.RunLifecycle` keeps the previous targets when `Build` errors.

  **A cycle that sent nothing is not a healthy cycle.** `Sent == 0` means no
  measurement — neither 0% nor 100% loss — so `alert.Evaluator.OnCycle`
  returns before touching any state and the writer omits the `probe_cycle`
  row entirely, leaving a gap rather than a fabricated point. Hop rows still
  write: the TTL walk carries its own per-hop `sent`/`lost`, which are real
  measurements even when the echo batch got no budget. Returning before
  `lastSeen` is written is deliberate — the source then ages out of the
  quorum denominator instead of voting healthy on no data. The cost is that
  a target which only ever sends nothing is silent rather than alerting,
  since there is no `no_data` condition; `clickhouse.writer.no_measurement`
  is the only signal. A validated config cannot reach that state
  persistently, because the per-ping budget floor guarantees every ping at
  least `minPingBudget`.

- **Address-family pinning:** `Target.Family` is `""` / `"v4"` / `"v6"` and
  every probe routes it through the shared `familyNetwork(base, family)`
  helper in `internal/probe/probe.go`. Interpretation is per-probe and
  intentional, not accidental: ICMP/MTR via `net.ResolveIPAddr("ip"|"ip4"|"ip6")`
  (shared `traceHops` takes family as a parameter); TCP via dialer network
  `tcp`/`tcp4`/`tcp6`; HTTP by cloning `http.DefaultTransport` with a
  family-pinned `DialContext` (`HTTP.clientFor` — connection pool is
  per-family, acceptable because `maxHTTPRequests==2`); DNS by pinning the
  **record type** via `LookupIP("ip"|"ip4"|"ip6", host)`, **not** the
  dialer network — the probe measures lookup latency, so restricting A vs
  AAAA is the semantically correct reading; how the resolver reaches the
  upstream is left to the OS.

- **Schema versioning:** `internal/storage/clickhouse/bootstrap.go` issues
  `CREATE TABLE IF NOT EXISTS` at startup, so adding new tables is safe
  and idempotent. Column additions ride `addColumnStatements` in
  `internal/storage/clickhouse/bootstrap.go` — `ALTER TABLE … ADD COLUMN IF
  NOT EXISTS` (with `ON CLUSTER` handling) runs on every start, as it did for
  `target_group`, `unreach`, and `target_reply`. The flush inserts name their
  columns explicitly, so a binary older than the newest column keeps writing
  after a rollback from the *next* migration onward; rolling back across the
  `unreach`/`target_reply` migration itself still breaks hop inserts, because
  pre-migration binaries used positional inserts. TTL changes are applied
  on every start via `ALTER TABLE … MODIFY TTL`, so they take effect on the
  next restart.

- **UI time-axis contract:** `/api/v1/targets/{id}/cycles` echoes the `from`
  and `to` it resolved. The charts pin `scales.x.range` to those unix
  timestamps so a wide window with sparse data still renders at the full
  requested span. Don't recompute the window client-side from the range
  string — use the server's echo.

  `/hops/timeline` echoes `step_sec` for the same reason: the heatmap draws
  one column per bucket, and bucket width is not recoverable from the rows,
  because a window holding a single bucket has no row gap to measure. Sizing
  a column from row count instead painted that one bucket across the entire
  window, reading as hours of history that had not been collected. `step_sec`
  is 0 on the raw tier, where the median inter-cycle gap is the right estimate
  since raw cycles are not grid-aligned.

- **Cluster mode (master/slave):** `--slave` flips the binary into a
  runner that registers with a master, pulls the target list over HTTP,
  probes locally, and pushes cycle batches back. Slaves never touch
  ClickHouse or the UI. The master's cluster endpoints live under
  `/api/v1/cluster/{register,config,cycles}` behind a shared bearer
  token; `master.Server` plugs into the existing API listener via
  `api.Options.ClusterHandler` so one listener serves both UI and
  ingest. **Per-target assignment via `target.slaves`:** when the list
  is empty (the default) the master and every registered slave probe
  that target; when it's non-empty, only named slaves probe it and the
  master skips it locally (`master.LocalTargets` filters the scheduler
  view; the stored cfg stays authoritative for UI + ingest).
  `BuildClusterConfig` filters per-slave via `X-Slave-Name` and strips
  both `Alerts` (evaluated master-side) and `Slaves` (peer assignments
  are none of a given slave's business) from the wire.

- **Cycle source stamping:** every cycle carries a `Source` string (the
  slave's `Cluster.Name`, or `cfg.Cluster.Source`/`"master"` for local
  probes). The Writer tags the `source` label on storage writes; the
  alert evaluator keys its state map by `(targetID, source)` so two
  slaves probing the same host transition independently. UI filtering
  uses the `source=` query param on cycles/rtts/hops endpoints.

  **A pushed source must already be in the registry.** `/cycles` takes
  the identity from `X-Slave-Name`, falling back to `batch.Source` for
  slaves predating that header, and refuses it with 403 unless
  `Registry.Has` already holds the name — checked *before* `Touch`,
  which would otherwise create the entry it is being asked about. Every
  minted label costs a permanent ClickHouse `LowCardinality` dictionary
  entry, a `QueryLatestHops` row forever, and a source on the
  unauthenticated API naming no real node. `Registry.Touch` returns
  whether it accepted, and refuses *new* names past
  `master.maxRegisteredSlaves` (512, per master process, entries leaving
  only via the hourly 24h `Sweep`) — the registry is the list of legal
  labels, so its size is the cardinality bound. Refusing at the ceiling
  never evicts a registered slave, and `/register` answers 503 there.
  `Touch` also refuses a version or advertise longer than
  `maxSlaveFieldLen` (256): both are free strings arriving as headers,
  bounded only by net/http's 1 MiB cap, and both are retained per entry —
  advertise inside the log-dedup key even when `ParseAdvertise` rejects
  it.
  This is a cardinality and data-integrity bound, **not** authentication:
  the cluster token is shared, so any registered slave can still claim
  any other registered slave's name. That is accepted, not overlooked.
  A slave that gets 403 re-registers and requeues the batch rather than
  dropping it — registration is otherwise attempted only at boot, so
  under `cluster.pull_every` `"0"` a master restart would otherwise
  refuse that slave for the life of its process.

- **Ingest bounds.** `cluster.CycleBatch.Validate` is the trust boundary
  for everything a slave puts in a batch; before it the only limit on a
  POST `/cycles` was the 100 MiB body cap. It refuses the **whole**
  batch, never part of it: a legitimate slave never trips a bound, so a
  violation is a protocol disagreement, and half-ingesting one leaves
  the two peers disagreeing about what was stored. The counts live in
  `internal/cluster/protocol.go` (`MaxCyclesPerBatch` 1024 —
  `slave.Runner` drains 100 per push; `MaxHopsPerCycle` 256 — the walk
  runs 30 TTLs with one row per distinct responder; `MaxRTTsPerHop` 128
  — one per round, mtr runs 10; `MaxHTTPSamplesPerCycle` 64 —
  `maxHTTPRequests` is 2), RTTs per cycle reuse `config.MaxPingsPerCycle`
  so no schedule `config.Validate` accepts can be refused at ingest, and
  the counter ceilings are the storage columns' own (`probe_hop.ttl` is
  `UInt8`, every sent/lost is `UInt16` — a negative wraps rather than
  failing, so it is refused here). The deployed 122-target / 6-source /
  20s install sits at or below 10% of every one.

  `config.MaxFutureSkew` (5m) and `config.MaxCycleAge` (7d) live in
  `config`, not `cluster`, because the reader needs the same window: a
  future-dated row pins itself as its source's newest in
  `QueryLatestHops` for as long as the lie lasts *and* outlives
  `probe_hop`'s TTL, which derives from the row timestamp, so only a
  manual ClickHouse delete clears it. Ingest stops new ones;
  `QueryLatestHops`' CTE carries `timestamp <= now() + MaxFutureSkew` so
  rows already in the table stop being served. `MaxCycleAge` is 7d
  because `PushSink` is a 600-cycle drop-oldest ring — even a multi-day
  outage delivers cycles far younger — while a row past the shortest
  default retention (14d) is written already expired.

  `clickhouse.maxHopRows` (300k) is on every hop read. `hop_addr` is
  slave-supplied text that widens `queryHopsBucketed`'s `GROUP BY` per
  distinct value, and each read buffers its whole result into a
  `[]storage.HopPoint` for an unauthenticated GET. The widest legitimate
  read — 7d timeline, 15m buckets, 672 × 30 TTLs × 6 sources × 2
  addresses through a flap — is ~242k rows, so the cap truncates only
  under the abuse it exists to bound. It is appended from one
  `hopRowLimit` var so a new hop read cannot inherit the unbounded shape
  by omission.

- **Slave push buffer + auth:** `slave.PushSink` is a fixed 600-cycle
  ring with drop-oldest on overflow; a failed push `Requeue`s on 5xx /
  network errors and drops on 404 (master lost state; next /register
  re-establishes us). A 401 on any endpoint cancels the runner's
  context with cause = `ErrAuth` so the process exits non-zero and the
  operator must rotate the token. Target-set fingerprint changes (group
  + name + probe + host + url + interval + pings) trigger a scheduler
  rebuild without tearing down the push loop.

- **Slave config refresh cadence:** `cluster.pull_every` on the slave
  controls how often `refreshLoop` re-pulls `/config` from the master
  (default 60s). The special value `"0"` / `"0s"` means one-shot: the
  initial pre-`Run` pull stays authoritative for the process lifetime
  and no refresh goroutine is started — operators rely on restart to
  pick up target-list changes. Any positive duration is used verbatim;
  unparseable / negative values log a warning and fall back to 60s.

- **Slave health mesh:** every node ICMP-probes every registered slave.
  The master synthesizes one target per slave into the reserved
  `_cluster` group / `_slave_health` probe (`internal/slavehealth`,
  group and probe names rejected in user config so a real target can't
  shadow one), injected at scheduler-build time and never written to
  `config.Store`. Addresses come only from the slave's own explicit
  `cluster.advertise` — never auto-detected, because a bridge-networked
  container reports `172.17.0.2` and no range check distinguishes that
  from a real WireGuard mesh address; empty opts a slave out entirely.
  `cluster.slave_addrs` optionally pins a slave to one address — a
  mismatch is refused a health entry, but unpinned slaves are accepted
  so the feature works zero-config. `cluster.health_hops` (default true)
  drops traceroute-hop collection for health targets at the probe, for
  meshes where N slaves would otherwise write N² hop streams (the master
  probes all N, each slave probes the other N−1).

  `cluster.health_alerts` names alerts stamped onto every synthesized
  health target — the only way to alert on a slave going down, since a
  user cannot write a `_cluster` target. The names ship nowhere near a
  slave (`sanitizeTarget` strips them, health group included) and the
  alert evaluator scrubs `Host`/`URL`/`Family`/`Cycle.Hops` off every
  Event for a health target before dispatch, because action templates
  render over the raw `Event` and would otherwise publish the address.

  The master's own scheduler builds its probe registry from
  `master.LocalTargets`' returned config, not from `cfg.Probes` — the
  synthetic `_slave_health` probe exists only in that clone
  (`localView` in `cmd/gosmokeping/run_node.go` keeps the two joined).

  The address never reaches the API: `slavehealth.Set` exposes `Probe()`
  (real hosts — scheduler and `BuildClusterConfig` only) and `Public()`
  (stripped — the API's `HealthLister` only). Hop redaction differs by
  endpoint. `/hops` blanks the union, per `(source, timestamp)`, of
  every `target_reply` marker row, the positional max-index row, and
  every row sharing an address with those two sets: a per-round walk can
  put the target's echo below a deeper all-silent round, so position
  alone no longer finds it, the positional arm keeps rows written before
  the marker existed covered, and the address arm catches a
  `TimeExceeded` quoting the target's own address. Every comparison is
  against the served rows themselves, never a configured address, so
  the Probe/Public split still holds. Those three arms are all set
  membership, so they fail *open* alone: a slave holding the shared
  token can write a trace whose deepest row is silent and whose only
  address sits on an unmarked intermediate, and no arm names a terminal.
  A `(source, timestamp)` group that yields no terminal address
  therefore has every address blanked — the whole path, not one row.
  Address comparison is on parsed `netip.Addr` values (unmapped,
  zone-stripped), because `hop_addr` is slave-supplied text and
  `::ffff:10.0.0.1` / `2001:0db8::0001` / `fe80::1%eth0` would otherwise
  each split a row from its own mate; text the parser rejects keeps its
  exact bytes as its identity. `/hops/timeline` buckets across
  `(bucket_ts, source, ttl, hop_addr)`, so no rule identifies a terminal
  row and every non-empty `IP` there is replaced with the sentinel
  `"redacted"` instead — not `""`, because the heatmap reads `IP` as a
  did-this-hop-reply flag; its DTO does not carry `target_reply` at all,
  since a field with no consumer on an unauthenticated endpoint is pure
  disclosure surface. That redaction only works where the marker exists, so
  a slave withholds a health target's hops entirely from a master whose
  `/config` omits `hop_markers`: an old master blanks the deepest row alone
  and would serve the slave's address whenever a round echoed above a
  deeper silent one. `PushSink` decides per cycle from the last
  advertisement — fail-closed until the first pull answers — and ordinary
  targets keep their hops either way. A redacted row loses `Unreach` with its
  address on both endpoints — an unreachable reason that outlives its
  address describes the slave's transit. `TargetReply` instead survives on
  `/hops` and is cleared on the timeline: `/hops` keeps every intermediate
  address and every row's RTTs, so the answering TTL is already readable and
  the marker is what `MtrSection` takes end-to-end loss from — clearing it
  dropped the UI back to the deepest-TTL fallback and rendered a slave
  reached at TTL 2 as 100% loss. On the timeline every address is the same
  sentinel, so nothing else would name that TTL.

  Because health targets live outside the stored config,
  `scheduler.LifecycleOptions.ExtraFingerprint` carries mesh membership
  into the rebuild decision (`Fingerprint(cfg)` alone can't see it), and
  registry changes share the debounced SIGHUP signal path so a fleet
  restart costs one rebuild, not one per registration.

- **Alert quorum:** `alerts.<name>.quorum` accepts `"majority"`
  (strictly more than half the live sources) or a positive integer
  (absolute minimum), gating dispatch only — per-source `sustained`
  counters stay independent. Sources stale beyond 3× the probe interval
  are pruned from the live count so a dead slave can't suppress a real
  alert. A quorum alert also has a warm-up: it won't dispatch FIRING
  until either 2 distinct sources have reported for that target+alert or
  the 3×-interval window has elapsed, so a master restart doesn't page
  and immediately resolve on partial data. Webhook/log templates get
  `{{.Firing}}` and `{{.Live}}` (both 0 for non-quorum alerts).

  `Event.FiringSources` names the sources firing at dispatch time
  (sorted; stale and unnamed sources excluded) and is populated on both
  the quorum and per-source paths. The non-quorum path collects it
  **read-only** via `firingSources` rather than reusing `tally`:
  tally deletes stale entries, which on that path would drop a silent
  source's `StateFiring` and swallow its eventual resolve. It reaches
  operators as `sources` in the webhook payload, the Discord
  `Sources (N)` field, and `ALERT_SOURCES` for `exec`; `source` /
  `ALERT_SOURCE` still carry only the triggering cycle's source.

## Config

`config.example.json` is the canonical reference. Env expansion happens on the
raw bytes before JSON parse (`${NAME}` form), so tokens can live in env vars.
`main.go` loads `.env` from `filepath.Dir(--config)` first, then from cwd —
this is load-bearing under systemd where cwd is `/`. Real shell env always
wins over `.env` (godotenv default); a missing `.env` is a silent no-op.

The `storage.clickhouse` block configures the single storage backend. The UI
and alert evaluator are backend-agnostic — they only see `storage.Reader` and
`scheduler.Sink`. Key fields: `addr` (required, e.g. `"127.0.0.1:9000"`),
`database`, `username`, `password`, `cluster` (empty = single-node MergeTree;
non-empty = ReplicatedMergeTree on that cluster name), and `retention` (see
the Retention bullet in Architecture). Bootstrap runs at every startup and is
idempotent — it creates missing tables and re-applies TTL settings.

## Integration tests

Behind the `integration` build tag. Set `CLICKHOUSE_ADDR` (required;
e.g. `127.0.0.1:9000`); optionally `CLICKHOUSE_USERNAME` and
`CLICKHOUSE_PASSWORD`. The tests create and drop a temporary database so
they don't collide with production data.
