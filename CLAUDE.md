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
  - hops:   ≤2h 11s, ≤24h 5m, >24h 15m, never finer than the probe interval

  **Known limitation — `/overview`'s bucket origin.** Every timestamp bound
  in every reader travels as epoch milliseconds through `dtMilli`, with one
  exception: `QueryOverview`'s `intDiv(toUInt32(timestamp) - ?, ?)` subtracts a
  signed `from.Unix()` from a `UInt32` cast, so its bucket index is wrong
  outside the 1970–2106 range that cast spans. Unreachable today because the
  endpoint only queries near now. `TestReaderBindsEveryTimestampAsMilliseconds`
  exempts it **by name** rather than by weakening the rule, so fixing the
  expression means deleting that exemption in the same edit.

  **Write buffers.** Each table's channel is sized by
  `writerChanCap(table, pings)` (base 4096 slots × a rows-per-cycle
  factor, clamped to [4096, 131072]) so all four absorb a comparable
  ClickHouse stall at ordinary rates — a flat 4096 gave the deployed
  122-target/20s install 11 minutes of cycle buffer against 96 seconds of
  hop buffer, and hops died first. The hop factor is clamp-limited by
  design: 4096×32 is the ceiling exactly, so a larger factor is inert and
  worst-case hop cycles (90 rows for icmp's 3×30 walk, 300 for MTR's
  10×30) still overflow first — drop-oldest and the counters are the bound
  there, not the buffer. Those per-table counters are served as
  `writer_drops` on `/api/v1/health`, alongside `cache` — the read cache's
  hit/miss counters, which are what distinguish a 503 under real load from
  one a cache minting a key per request caused. `Writer.offer` is
  drop-oldest, evicting one row before enqueuing rather than discarding
  the incoming one: a full channel means ClickHouse is stalling, and
  dropping the newest grows a hole up to the present for as long as the
  stall lasts — the window an operator is actually looking at. Overflow
  takes no lock, so two producers overlapping can invert one pair, and can
  also refill the freed slot before the offered row reaches it, losing both
  and counting both drops — the incoming row is preferred over the oldest,
  not guaranteed a place; serialising the fast path costs more than the one
  row of recency it would buy. `offer` retries the non-blocking send once
  before evicting, which takes a slot a flusher freed in between rather
  than spending a queued row and counting a drop that did not happen; the
  window itself cannot close without that lock.
  **`runTable` stops draining while a flush is failing** (a nil `src` in
  the select), because `pending`'s cap is a flat
  `maxRows × flushRetainFactor` for all four tables: draining through it
  discarded the per-table sizing at exactly the moment it exists for, and
  at the deployed shape left `probe_hop` with ~33 seconds of backlog
  against `probe_cycle`'s ~11 minutes — worse than the imbalance the
  sizing was added to remove. The channel cap only ever governed the case
  where ClickHouse accepts the connection and hangs; now it governs a
  refused one too. `flushRetainFactor` retains a failed batch for the
  next tick and caps `pending` at `maxRows × 4`; since the loop stops
  draining while a flush is failing, `pending` cannot exceed `maxRows`
  during an outage and that cap binds only on the shutdown drain, which
  appends the whole channel at once — every row it still holds there is
  counted as a drop, not just the overflow past the cap;
  `slave.PushSink`'s ring is the third.

  **Window caps.** The binary ships no auth, so on every endpoint that
  returns unbucketed rows the window cap is the only bound on an
  anonymous request's scan: `/rtts` 24h (`api.maxRTTWindow`), `/http`
  and `/hops/timeline` 7d (plus one probe origin per request — see the
  hop row caps below), and every `?step=` override on `/cycles` bounded by the ladder's own
  tier for the window — `raw` by `storage.PickCycleStep(span) == 0`,
  `1h`/`1d` by `derived <= override` — rather than second copies of the
  thresholds, so widening a tier widens the override with it, and an
  override finer than the tier is a 400 rather than served. `/status`
  scans only `api.statusRecentCycles` (50) × the live interval instead of
  an unbucketed 24h — 50 intervals is 50 cycles *per source*, so the trim
  is per source too (`trimPerSource`); across sources it described a
  different quantity from the window and showed 8 cycles each on a
  6-source install. It echoes `from`/`to` for the reason `/cycles` does:
  a target silent longer than the window comes back empty, which is the
  honest answer but is otherwise indistinguishable from a target that
  never existed.
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
  Hop timeline queries are the exception: `queryHopsGrid` groups by
  `(slot, source, ttl)` only, so a path flap folds into one row carrying
  the address of the slot's worst-loss cycle — the row the heatmap
  already picked out of the per-responder set and drew. Per-responder
  rows survive on `/hops?at=`, which pins one cycle and needs no grid.
  `worst_time` is that cycle's own timestamp, and it is what a heatmap
  cell clicks through to whether or not the slot lost: `/hops?at=`
  resolves the nearest cycle within ±15m, so a slot start is unreachable
  from every cycle in it once the probe interval floors the step past 30m.

- **Retention:** per-table TTL set at bootstrap from
  `storage.clickhouse.retention.{cycle,rtt,hop,http}_days` (defaults
  365/14/90/14; 0 defaults, anything else must be inside `[1,
  config.MaxRetentionDays]`). A negative value used to pass straight
  into the `MODIFY TTL` Bootstrap re-emits on every start — a TTL
  already in the past, which expires the whole table. `MaxRetentionDays`
  is a sanity bound and **not** a derived maximum: the TTL is evaluated
  against each row's own timestamp, so what is representable depends on
  when the row was written and no compile-time constant knows it.
  `config.RetentionWithinDateTime` holds the real check, against a clock
  and `toDateTime`'s 2106 ceiling, and it lives in `config` for the
  reason `ICMPPingBudget` does: **`Validate` calls it**, so a retention
  the process cannot boot with is never stored. Guarding only at
  bootstrap left ~7,482 of the values the fixed ceiling admits validating
  green on load and on SIGHUP and then refusing to start at the next
  restart — invisible until a redeploy, since `Bootstrap` runs only at
  startup. `clickhouse.ttlWithinDateTime` delegates to the same function
  as defence in depth, and runs **before** `PerTableDDL`: that DDL embeds
  the retention in `CREATE TABLE` too, so a guard placed only over the
  `ALTER` loop let a fresh install create its tables carrying an
  unrepresentable TTL and abort afterwards, with every later start
  failing at the guard before reaching the `ALTER` that would repair
  them. `Bootstrap` re-emits `ALTER TABLE … MODIFY TTL` on every
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
  replies by **sequence number only**, not ID — don't "fix" that, it is
  correct for both socket types — plus the same
  peer-is-the-resolved-destination check `matchDatagram` applies on the
  walk (`matchEchoReply`, the echo read path's trust boundary). Type, id
  and seq are all visible to any router on the path, so one could answer
  from its own address and a fully-down target read 0% loss with
  plausible RTTs.

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

  Rows are per `(ttl, responder address, echo-vs-error)` in first-seen
  order, so ECMP siblings each carry their own samples and a responder
  that both echoes and rejects — a rate-limiting firewall answering
  admin-prohibited from the target's own address — yields two rows;
  mixed onto one, the unreachable's error-generation time rode the
  `TargetReply` marker into MTR's RTT mirror and became the target's
  percentiles, with `len(RTTs)` exceeding `Sent−LossCount`. One
  responder per round per TTL still holds, so `MaxHopRowsPerCycle`'s
  rounds × TTLs derivation is unchanged; a TTL's losses have no responder to
  blame and fold onto its first-seen row, which keeps single-responder
  numbers identical to the pre-split shape, and a TTL nothing answered emits
  one `IP: ""` row. Rows the target itself answered carry `TargetReply` —
  the target's row is no longer guaranteed to be the deepest, so `/hops`
  redaction and MTR's RTT mirror key on that marker instead of on position.
  End-to-end loss does **not**: it comes from the cycle's own round
  counters, served as `target_loss` alongside `hops`, because summing
  marked rows counts one round once per TTL the target ever answered at.

- **`/hops` target loss:** `QueryLatestHops` and `QueryHopsAt` return
  `storage.HopsResult{Hops, Cycles}` — the `probe_cycle` counters read at
  exactly the `(source, timestamp)` pairs the hop rows pinned, so they are
  cached, admitted and refused with the hops rather than as an uncached
  point query on every cache hit. The wire field is `target_loss`, a
  sibling array of `hops` named for the quantity rather than the row so it
  cannot be read as a hop field. `/hops/timeline` carries none: a grid
  slot spans whatever cycles fell in it. Absent key or missing source renders as unknown,
  never 0% — an old server pairs with a new UI during a rolling upgrade.

  **An echo reply counts as the target's only if its peer is the resolved
  destination.** `matchDatagram` is the read path's whole trust boundary —
  bytes and source address both come off the wire — and it sits apart from
  `sendTTL`'s read loop precisely so hostile input can drive it without a
  socket. Type, id and seq are all visible to any router on the path, so
  one could answer from its own address; the round stopped there, the row
  was marked `TargetReply`, and MTR reported a target that never replied.
  That marker is persisted and drives `/hops` redaction, so it is a
  disclosure input, not only a display one. `TimeExceeded` and unreachable
  are exempt — they legitimately come from routers along the path.
  Addresses compare as unmapped `netip.Addr` via `peerAddr`, which is what
  reconciles the socket asymmetry: an unprivileged ping socket reports
  `*net.UDPAddr` (and the kernel has rewritten the ICMP id), a raw one
  `*net.IPAddr`. Any other shape resolves to the invalid zero `Addr` and
  matches nothing — without that, `Addr.String()` would write the literal
  `"invalid IP"` into `probe_hop` as an error's hop address. The
  responder's identity stays a `netip.Addr` end to end inside the walk:
  `ttlReply.addr` and the aggregation rows hold the parsed address, and
  the textual `Hop.IP` is produced once at row emission, so the wire and
  storage form are unchanged and `""` still means nothing answered at that
  TTL.

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
  validated. `config.Validate` gates on the config defining an icmp probe *or* on a
  cluster master (a cluster block with a token), because the health mesh
  injects `slavehealth.ProbeDef`'s icmp probe at scheduler-build time —
  gating on the stored map alone let a validated master config become
  unbuildable fleet-wide at the first slave registration, and a slave
  booting into a >=2-peer mesh exit non-zero on every restart.
  `probe.Build` gates on the map it actually receives, which on both
  master and slave is the post-injection one. A standalone config with
  no icmp probe is still unbound by the budget, but
  `config.ValidatePingCount` bounds `pings` for **every** schedule at
  `config.MaxPingsPerCycle`: the scheduler stamps `Sent = cfg.Pings` on
  a probe error taken while the cycle context was still live, so an
  unbounded count produced cycles cluster ingest
  refuses (`sent` past its UInt16 column) and each refusal dropped the
  slave's whole drained batch. That ceiling is the icmp sequence space
  even for a config defining no icmp probe, which over-restricts a
  standalone non-icmp schedule by 92 pings — accepted, because
  conditioning it is what made the gate above wrong, and no real
  schedule sends 65,444 pings a cycle. `pings` is bounded twice before that multiplication: at
  `interval/200ms + 1`, which the product would otherwise overflow into a
  passing budget, and at `config.MaxPingsPerCycle`, the echo sequence space
  left above the TTL walk's window, which a long enough interval would
  otherwise admit into `echoBaseSeq`'s degenerate branch (`probe` pins that
  ceiling to the walk's own bounds with a compile-time assertion). Reload is
  fail-closed at both layers: `Store.Reload` keeps the last-good config, and
  `scheduler.RunLifecycle` keeps the previous targets when `Build` errors.

  **A cycle that sent nothing is not a healthy cycle.** A probe that returns
  no result at all is stamped `Sent = cfg.Pings` — a target that did not
  answer — **unless the cycle's own context ended first**, which is a
  measurement never taken rather than a failed one. `probe.resolveIPAddr`
  honors that context, so every SIGHUP and every debounced registry change
  cancels the in-flight resolves; without the distinction a config reload
  wrote a fleet-wide outage into `probe_cycle` and fired `sustained: 1`
  alerts off cycles that never sent a packet.

  `Sent == 0` means no
  measurement — neither 0% nor 100% loss — so `alert.Evaluator.OnCycle`
  returns before touching any state and the writer omits the `probe_cycle`
  row entirely, leaving a gap rather than a fabricated point. Hop rows still
  write: the TTL walk carries its own per-hop `sent`/`lost`, which are real
  measurements even when the echo batch got no budget. Returning before
  `lastSeen` is written is deliberate — the source then ages out of the
  quorum denominator instead of voting healthy on no data. The cost is that
  a target which only ever sends nothing is silent rather than alerting,
  since there is no `no_data` condition; `clickhouse.writer.no_measurement`
  is the only signal. The per-ping budget floor does not close that gap:
  `HasICMPProbe` gates it on a config defining an `icmp` probe, so an
  MTR-only schedule is never checked against it, and MTR's `Sent` is the
  rounds that actually sent — zero when `traceHops`' resolve spends the
  cycle deadline before the first probe goes out. Resolution goes
  through `probe.resolveIPAddr`, which honors the context, so it now
  fails *at* the deadline instead of overrunning it: the trace goroutine
  is joined by an unconditional defer on every `ICMP.Probe` return path,
  so a resolver call that ignored a cancelled cycle blocked shutdown
  (`Scheduler.Run`'s `wg.Wait`, `RunLifecycle`'s `<-schedDone`) and
  every SIGHUP rebuild behind it.

- **Address-family pinning:** `Target.Family` is `""` / `"v4"` / `"v6"` and
  every probe routes it through the shared `familyNetwork(base, family)`
  helper in `internal/probe/probe.go`. Interpretation is per-probe and
  intentional, not accidental: ICMP/MTR via `probe.resolveIPAddr("ip"|"ip4"|"ip6")` —
  `net.ResolveIPAddr`'s selection with the context honored, shared
  `traceHops` taking family as a parameter. It queries **only the pinned
  family's record**, as the real resolver does: filtering a dual lookup
  instead made an unrelated blackholed AAAA path fail every v4-pinned
  cycle, reading as total loss on a target that was answering. A literal
  never reaches the resolver at all, which is what carries a link-local
  zone through to the socket; TCP via dialer network
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
  is always positive: the hop ladder has no raw tier, because a slot per cycle
  puts the producer's cycle rate — the one factor nothing bounds — back into
  the row cap below.

  A bucket's click-through carries its `WorstTime` at full precision, all the
  way from the row to `?t=` in the page URL. `/hops?at=` resolves the cycle
  *nearest* the stamp, so rounding one to a whole second can leave the
  neighbouring bucket's cycle nearer than the cycle the column was drawn for —
  at a sub-2s cadence the click then opens a cycle outside the bucket clicked.
  RFC3339 is the only `at` form that survives the trip, since the unix
  form parses as an integer. That precision is why the pinned read's
  cache key is milliseconds — and why only the *relative* form is
  coalesced: `?at=-1h` names no instant, so resolving it against a fresh
  clock minted a key per request against a 16-entry LRU shared with the
  timeline's entries. `parseTimeParam` reports which branch parsed and
  `getHops` applies `storage.CoalesceHopsAt` to that one alone, rather
  than re-deriving the grammar at the call site, where the
  `-1`-versus-`-1h` distinction is exactly what a second reading gets
  wrong.

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
  unauthenticated API naming no real node. `Registry.Touch` returns an error naming why it refused —
  `errRegistryFull` at the ceiling (503 on `/register`, retryable once
  `Sweep` frees a name) and `errSlaveFieldTooLong` for a version or
  advertise past `maxSlaveFieldLen` (400: the request's own bytes can
  never succeed), so an operator is no longer told "slave registry full"
  for an oversized advertise. It refuses *new* names past
  `master.maxRegisteredSlaves` (512, per master process, entries leaving
  only via the hourly 24h `Sweep`) — the registry is the list of legal
  labels, so its size is the cardinality bound. Refusing at the ceiling
  never evicts a registered slave, and `/register` answers 503 there.
  `Touch` also refuses a version or advertise longer than
  `maxSlaveFieldLen` (256): both are free strings arriving as headers,
  bounded only by net/http's 1 MiB cap, and both are retained per entry —
  advertise inside the log-dedup key even when `ParseAdvertise` rejects
  it. The refusal is **per field and never a refusal of liveness**: a
  registered slave whose cycles are being ingested still advances
  `LastSeen` (returning first let `Sweep` drop it after 24h, dedup window
  included, and its next push 403s), and an over-length version still lets
  the advertise beside it re-resolve — treating their union as one
  condition froze that slave's mesh address against every later
  `cluster.advertise` edit and every pin added to evict a squatter. A name
  the registry has never seen is still refused outright: the entry is what
  makes a name a legal ingest label.
  This is a cardinality and data-integrity bound, **not** authentication:
  the cluster token is shared, so any registered slave can still claim
  any other registered slave's name — including one that spells a peer's
  advertised address. That is an **accepted risk, decided deliberately**,
  not an oversight: holding the token means already having compromised a
  node the operator runs, and nothing reaches that from outside the fleet.
  Reviews keep re-raising it; the answer is settled. Binding the name to a
  per-slave credential is the only real fix and is out of scope until the
  threat model changes.
  A slave **running this binary or newer** gets 403, re-registers and
  requeues the batch rather than dropping it — unless that re-registration
  is itself refused with `ErrMasterRefused`, which exits non-zero like
  `registerForever` and `pullConfigInitial` do at boot: the master answers
  those bytes identically forever, so retrying leaves the ring
  head-of-line blocked while drop-oldest eats the live cycles behind the
  batch, under a process reporting healthy. **`ErrMasterRefused`, not
  `ErrRejected`:** the latter is every 4xx bar 401/403/404 and the
  retryable set, and any intermediary can produce one — a
  `client_max_body_size`, a header-buffer limit or a routing change
  answering 413/431/405 would crash-loop the whole fleet under systemd,
  the failure `client.go` already records for 403. Only 400, the status
  the master's own handlers emit, is fatal; registration is otherwise
  attempted only at boot, so under `cluster.pull_every` `"0"` — where no
  `/config` heartbeat exists either — that path is the only thing that
  recovers a slave after a master restart, and
  `slave.TestSlaveRecoversFromMasterRestartWithoutConfigPull` drives that
  exact combination end to end. A slave predating it never recovers on its
  own and must be restarted, which is why
  `docs/migrate-cluster-ingest-bounds.md` makes the slave upgrade
  mandatory rather than engineering a compatibility path for it.

- **Ingest bounds.** `cluster.CycleBatch.Validate` is the trust boundary
  for everything a slave puts in a batch; before it the only limit on a
  POST `/cycles` was the 100 MiB body cap. It refuses the **whole**
  batch, never part of it: a legitimate slave never trips a bound, so a
  violation is a protocol disagreement, and half-ingesting one leaves
  the two peers disagreeing about what was stored. The counts live in
  `internal/cluster/protocol.go` (`MaxCyclesPerBatch` 1024 —
  `slave.Runner` drains 100 per push; `MaxHopsPerCycle` = 2 ×
  `config.MaxHopRowsPerCycle`; `MaxRTTsPerHop` 128 — one per round, mtr
  runs 10; `MaxHTTPSamplesPerCycle` 64 — `maxHTTPRequests` is 2), RTTs
  per cycle reuse `config.MaxPingsPerCycle`
  so no schedule `config.Validate` accepts can be refused at ingest, and
  the counter ceilings are the storage columns' own (`probe_hop.ttl` is
  `UInt8`, every sent/lost is `UInt16` — a negative wraps rather than
  failing, so it is refused here). The deployed 122-target / 6-source /
  20s install sits at or below 10% of every one.

  **Leaf values, not only collection lengths.** Bounding `len(Hops)` left
  every value inside unbounded: 256 hop rows whose `ip` strings summed to
  the 100 MiB body cap all landed in the `LowCardinality(String)`
  `hop_addr` column, which an unauthenticated `/hops` then served back. So
  `validate` also refuses: a hop `ip` that is neither empty nor a
  `netip.ParseAddr`-parseable address (empty is the producer's "nothing
  answered at this TTL"); `lost > sent` on a cycle or a hop, which no
  probe can produce and which drives `loss_pct` and every alert condition;
  any latency — cycle RTT, hop RTT, http RTT, every `stats.Summary` field,
  walked through `stats.PercentileSet` so a new percentile is covered the
  day it is added — outside `[0, config.MaxSampleRTT]`, which is exactly
  `durUS`'s saturation point (`MaxUint32` µs, 71m34.967295s) so an accepted
  value is stored as itself; an `HTTPSample.Time` outside the batch window,
  the same `[now−MaxCycleAge, now+MaxFutureSkew]` the cycle is checked
  against, which is what keeps a sample from evading `probe_http`'s TTL; a
  `Status` outside `[0, 999]`, which wraps in the `UInt16` column; and any of
  group / name / probe / source past `config.MaxLabelLen` (256).

  **A free-text leaf is truncated at the producer, never refused at the
  boundary.** `HTTPSample.Err` carries a `url.Error` embedding the whole
  configured URL, and config bounds no URL's length, so no ceiling derived
  from what a probe can emit exists — a 4096-byte wire bound turned a
  legitimate long URL plus a connection failure into a 400, and `ErrRejected`
  then dropped the entire batch including up to 99 unrelated cycles.
  `probe.TruncateHTTPErr` (`probe.MaxHTTPErrLen`, 4096, cutting on a rune
  boundary so `probe_http.error` never holds half a code point) runs in the
  http probe before the sample exists, and again in `ToCycle` so a slave
  predating it costs a truncated string rather than a dropped batch.

  **Every config `Config.Validate` accepts is ingestable**, which is a
  property to preserve rather than assume: it held for `MaxLabelLen` and not
  for latency, because a cycle runs under a context whose deadline is the
  interval and config bounded no interval. `config.MaxProbeInterval` is now
  refused above, and it is `MaxSampleRTT` *exactly* rather than a round number
  under it: a ceiling picked below the real one refuses working schedules,
  which is the same defect one layer up. The only schedules it refuses are the
  ones whose own latencies `durUS` could not store as themselves. Both
  constants live in `config` for the same reason `MaxFutureSkew` does — the
  producer is a schedule this package validates and the consumer is cluster's
  ingest bound. Residual: a probe that overshoots its own context deadline by
  more than zero at the very top of the range yields an RTT one tick past the
  bound, which ingest refuses. Nothing measurable protects against that
  without reintroducing a picked number, and the value is unstorable either
  way.
  Two values are **overridden** rather than bounded, because the master
  already holds the authoritative one: `ingestBatch` stamps `Source` from
  the authenticated identity and `ProbeName` from the resolved target's
  own `Probe`. Both are `LowCardinality` columns, so free text there is a
  permanent dictionary entry per distinct value.
  `ToCycle` stores the *parsed* address's canonical form, not the wire
  bytes: `2001:0DB8::0001` and `2001:db8::1` are one address and must not
  be two dictionary entries, and it is the form `/hops` redaction already
  compares on. Text that fails to parse becomes `""` there as well —
  fail-closed if a future caller reaches `ToCycle` without `Validate`.

  **Parsing is not bounding.** `netip.ParseAddr` accepts a zone of any
  length, so `fe80::1%<90 MiB of unique text>` was a hop `ip` that parsed,
  canonicalized and landed in `hop_addr` — the leaf attack the paragraph
  above closed, reconstructed through the one field that looked already
  validated. `parseHopAddr` is now the single reading both `validate` and
  `ToCycle` take: `MaxHopAddrLen` (76) bounds the whole encoded value
  *before* `ParseAddr` sees it, and a zone must be shaped like the interface
  name or decimal index Go fills one from. `MaxHopZoneLen` is twice
  `maxInterfaceNameLen` (15 — `IFNAMSIZ`−1 on Linux, macOS and the BSDs, the
  only platforms this binary ships for, against 10 digits for an `int32`
  index), the same headroom `MaxHopsPerCycle` takes over its own producer.
  It is not wider **because `hop_addr`'s width is the other half of
  `clickhouse.maxHopRows`' byte ceiling on an unauthenticated `/hops`**:
  at 30 the worst case is 485,280 × 76 B ≈ 36.9 MB, where a zone of 256
  would have made it ≈146 MB. The character
  class refuses ASCII control bytes and `/` for what those bytes do
  downstream, **not** as a survey of what a kernel accepts: `%` is permitted,
  because refusing a character some platform can name an interface with costs
  the producer its whole batch. Linux itself refuses a literal `%` —
  `register_netdevice` sends any name holding one through `dev_alloc_name`'s
  `%d` expansion, verified against 7.1.8 — but the BSD rename path applies no
  such expansion, and the refusal bought nothing: a zone's danger is its
  length, which `MaxHopZoneLen` bounds.
  `MaxHopAddrLen` (76) is the zone ceiling plus RFC 4291 §2.2 form 3, the
  longest textual address `ParseAddr` accepts (45 bytes). `fe80::1%eth0`,
  `fe80::1%3` and `fe80::1%uplink%blue` all pass, which is the point: a
  refused hop address is a refused batch.

  **The hop bound is derived from the producer, not picked.**
  `config.MaxHopRowsPerCycle` = `MaxTraceRounds` (10) × `MaxTraceTTL` (30)
  = 300 is `walkRounds`' exact ceiling — one row per (ttl, distinct
  responder), and a round contributes at most one responder per TTL — and
  `cluster.MaxHopsPerCycle` is twice it. A hand-picked 256 sat *below* the
  producer's own maximum, so a deep ECMP path was a legitimate batch the
  master refused. The two constants mirror `probe`'s unexported `maxRounds`
  and `MTR.maxTTL`, which cannot assert against them at compile time the
  way `icmpTraceSeqReserve` does (probe already imports config, and the
  values are unexported), so `internal/config/tracebounds_test.go` parses
  `probe/mtr.go` and fails naming the value to update. Replace it with a
  compile-time assertion in `probe` the next time that package is open.

  `config.MaxFutureSkew` (5m) and `config.MaxCycleAge` (7d) live in
  `config`, not `cluster`, because the reader needs the same window: a
  future-dated row pins itself as its source's newest in `QueryLatestHops`
  for as long as the lie lasts *and* outlives `probe_hop`'s TTL, which
  derives from the row timestamp, so only a manual ClickHouse delete
  clears it. Ingest stops new ones; both pinned reads' CTEs carry
  `timestamp <= now() + MaxFutureSkew` so rows already in the table stop
  being served — `QueryHopsAt` needed it as much as `QueryLatestHops`,
  since `storage.ValidQueryTime` admits an `at` far enough ahead to reach
  them. `MaxCycleAge` is 7d because `PushSink` is a 600-cycle drop-oldest
  ring — even a multi-day outage delivers cycles far younger — while a row
  past the shortest default retention (14d) is written already expired.

  **Every hop read carries a row cap, and each is derived from real
  limits rather than sized against a typical read** — twice now a cap
  picked that way sat under output the probe produces by itself, and
  once a cap re-derived from the wrong limit was cut below reads that
  already worked. Where no product bounds the read, the cap says so and
  is guarded as a byte ceiling instead.
  Each read buffers its whole result into a `[]storage.HopPoint` for an
  unauthenticated GET, so the ceiling is what stands between one GET and
  the process's memory.

  `clickhouse.maxHopRows` (485,280) covers the two pinned reads,
  `QueryLatestHops` and `QueryHopsAt`. It is the one cap here that is a
  **memory ceiling rather than a product of producer limits**, and
  deliberately so: both reads group by `source` over `probe_hop` itself,
  so the group count is every label the table still holds within its
  TTL, which operator churn — renames, re-provisioned nodes, restarts —
  raises past the live names `maxRegisteredSlaves` admits at once.
  Deriving it from the registry encoded a bound the query does not have
  and cut the cap below reads it used to serve.
  `TestMaxHopRowsClearsEverySourcesPinnedCycle` guards it from both
  sides instead: at or above the rows a full live fleet's pinned read
  holds (`maxHopSources × cluster.MaxHopsPerCycle`), and under the byte
  ceiling `maxHopRows × cluster.MaxHopAddrLen` names. Extreme churn can
  still reach it, and reaching it is `ErrHopsTruncated` → 400 with the
  remedy, never a silent prefix.

  `clickhouse.maxHopTimelineRows` (172,288) covers `/hops/timeline`, and
  is `maxHopTimelineBuckets × maxHopTTLs`: the endpoint caps its window
  at 7d and `PickHopStep` never buckets finer than 15m past 24h, so the
  grid is at most 673 slots wide, and `probe_hop.ttl` is `UInt8` with
  ingest refusing an index outside `[0, 255]`, so it is at most 256 rows
  deep. **Both other factors are removed rather than estimated**:
  responders fold into one row per `(slot, ttl)` (see the storage
  bullet), and the source count — the one factor neither the config nor
  the schema bounds, 512 registered slaves × their history — is admitted
  instead, one probe origin per request. `source` is therefore *required*
  on `/hops/timeline`; an empty value is the untagged pre-cluster origin,
  and a missing one is a 400. The heatmap already fetches and draws one
  canvas per source, so the UI never sends the request that is refused.
  **Every tier buckets**, so the cap is the grid's own product rather than a
  guess: `MaxHopGridSlots` × `maxHopTTLs`, which the widest schema-legal
  result reaches *exactly* — a 7d window starting off the 15m grid spans 673
  slots, `ttl` is a `UInt8` whose whole domain ingest admits, and the refusal
  is `> cap`, so that result is served rather than refused
  (`TestIntegrationTimelineReachesItsCapExactly`). Nothing schema-legal
  exceeds it, which is the point — the refusal is an assertion about the data,
  not a policy about the query. The ≤2h tier was raw and justified separately, on
  `probe` walking one TTL per 50ms; a round ends at the target's own reply
  *before* it pays that spacing and config bounds no interval from below, so
  a one-hop MTR target at a 30ms interval wrote 240,000 rows into the window
  that bound said held 144,000 — the fourth cap in a row set under its own
  producer. `storage.MinHopStep` (11s) is `ceil(2h / (MaxHopGridSlots-1))`,
  so the finest tier needs no more slots than the 7d tier the cap is derived
  from, and no step goes below the probe interval: an empty slot renders as
  history that was never collected. `docs/migrate-hops-timeline-contract.md`
  carries the operator-facing form.

  Reaching either cap is reported, never trimmed. Hop reads order
  oldest-first, so the prefix a silent truncation left was missing the
  newest history and rendered as a probe that had stopped — served as 200
  on the endpoint an operator opens during an incident. Every hop query
  asks for `cap + 1` via `hopRowLimit(cap)` so a new hop read cannot
  inherit the unbounded shape by omission, and `hopRowsWithinCap` refuses
  the whole result with `storage.ErrHopsTruncated`, which the API maps to
  400 with the remedy rather than the 502 that blames the backend or the
  503 that invites an identical retry. A flag on a 200 was rejected:
  `CachingReader` would cache and re-serve the partial set, and an error
  is the fail-closed default for a consumer that has not heard of the
  flag. `CachingReader` also refuses to answer it from an expired entry: a
  refusal is deterministic, only a success bumps an entry's expiry, and
  serving stale on one turned the refusal into a 200 that never ended
  (`isRefusal`, the one place a new semantic sentinel is declared).
  `maxCycleCounterKeys` (2 × `maxHopSources`) bounds the same read's
  counters lookup, and is the one bound here that **trims rather than
  reports**: it governs that query alone, a missing counter already
  renders as unknown loss by contract, and letting it refuse failed a
  whole path view whose hop rows were present and correct.

- **Ingest admits each measurement once per window; local probing needs no
  guard.** Cluster delivery
  is at-least-once — `PushSink.Requeue` resends any batch whose ack was lost
  to a 5xx or a network error — and all four tables are plain `MergeTree`
  (`ReplicatedMergeTree` in CH-cluster mode), neither of which deduplicates.
  A redelivered batch therefore doubled `sum(sent)`, `sum(lost)`, the
  `quantilesExactWeighted` weights and every hop counter, so a blip between
  slave and master silently inflated the loss percentages an operator reads.
  `master.cycleDedup` (`internal/cluster/master/dedup.go`) sits in
  `ingestBatch`, **upstream of the fanout**, so the writer and the alert
  evaluator are covered by one check rather than one each. Scoping it to the
  cluster path is deliberate: a locally probed cycle reaches the fanout once
  with no retry path behind it, so a guard there would add a window with
  nothing to catch.

  **What is guarded is admission to the fanout, not storage.** The window slot
  is *reserved* before delivery, because that is what collapses two copies
  arriving at once into one delivery, and `cycleDedup.forget` *releases* it
  again if `Sink.OnCycle` never returns — an identity remembered for a cycle no
  sink took would refuse the very redelivery that repairs it. A released
  identity keeps its ring slot until the ring wraps past it, and the slot
  carries the insertion position that made it, so evicting it deletes nothing
  a retry re-established under the same identity; the cost is that a window
  which released *k* of its last 1024 insertions recognises 1024−*k*. It
  cannot reach
  further than that: `OnCycle` returns nothing, so a row the writer then drops
  on a full channel is indistinguishable here from one it queued, and the
  redelivery that would have refilled it is still classified as a copy. Closing
  that needs an acceptance signal the `Sink` interface does not have; the
  `writer_drops` counter on `/api/v1/health` is what surfaces it meanwhile.

  **Identity is `(source, group, name, timestamp)`.** `source` is the map
  level above, and it is the authenticated `X-Slave-Name` `ingestBatch`
  already pins — so two slaves probing one host keep both rows, and no slave
  can spend another's window. `group` is in it for the same reason it is in
  every query: `core-backbone/frankfurt` and `retn/frankfurt` are two
  targets. The reserved `_cluster` health group needs no special case — the
  target there is the *probed* slave and the source is the *probing* one, so
  a full mesh is N distinct identities per timestamp. Dedup runs **after**
  target resolution, so a stale slave's unresolvable targets cannot spend the
  window a live one needs.

  **A window, never a high-water mark.** A backlog delivered after an outage
  carries timestamps older than rows already stored and every one of them is a
  real measurement, so the guard is set membership over recent identities.
  Ordering is the alert evaluator's `lastCycle` floor, which is why that guard
  stays.

  **The bound is one batch, because one batch is what gets redelivered.**
  `Requeue` puts a failed batch back at the ring's *head* and `Drain` reads
  head-first, so the redelivery is the same cycles, ahead of anything newer;
  the largest one this master accepts is `cluster.MaxCyclesPerBatch` (1024),
  which is therefore `dedupWindowPerSource` exactly. `dedupMaxSources` is
  `maxRegisteredSlaves` (512), because ingest refuses a name the registry
  does not hold, so the registry's ceiling *is* the number of live windows.
  Measured resident cost at 512 × 1024 entries: 53.7 MB against one target,
  109.5 MB against 1024 distinct ordinary targets, 381.5 MB in the
  pathological case of 1024 targets whose group and name are both
  `config.MaxLabelLen`; the deployed 6-source shape is 0.6 MB. Target keys
  are interned per window — bounded by `dedupWindowPerSource`, since an
  N-entry window references at most N distinct keys — which is what makes
  that cost scale with distinct targets rather than with cycle rate; without
  it the one-target case measured 341.4 MB.

  **A window's lifecycle is its registry entry's.** `Registry.Sweep` releases
  the name and, through `SetOnRemove`, the window with it: ingest refuses a
  name the registry does not hold, so the window has no reader left and only
  holds a slot. Eviction reads the same coupling in the other direction —
  `evictLRU` spends a window whose source the registry no longer holds before
  any live one's, because last-ingest order alone made a source that was
  merely between pushes a legal victim while a swept, permanently dead one
  kept its slot. With no dead candidate it falls back to plain LRU; it never
  refuses the newcomer. `admit` calls `Registry.Has` under the dedup lock and
  `Sweep` calls `forgetSource` with the registry lock already released, which
  is the order that keeps the two from meeting in the middle. That release is
  also a gap a re-registration fits through, so `forgetSource` re-reads
  membership under the dedup lock rather than trusting the name `Sweep`
  captured: the removal is stale exactly when the slave came back, and
  applying it would erase a window that is already refusing redeliveries.

  **Both eviction paths fail open.** Past the window, and past
  `dedupMaxSources` where the least recently used window is dropped, the
  affected source degrades to the pre-guard double-write — never to silence.
  A guard that muted a peer would be worse than the defect it closes.

  **Residual: a batch in flight across a master restart still double-writes.**
  The window is in-memory, like the registry and the alert state, so a restart
  empties it and one redelivery per source lands twice. That is bounded — at
  most `MaxCyclesPerBatch` cycles per source per restart, against the status
  quo of every lost ack forever — and closing it means persisting state the
  master otherwise keeps none of. Two smaller residuals ride with it: a row
  the writer drops on channel overflow (counted as `writer_drops`) is no
  longer refilled by a later redelivery, and neither is one lost to a sink
  panic the fanout recovers. Neither was ever a designed property.

  **The ClickHouse-native options were evaluated and rejected.**
  `insert_deduplication_token` with `non_replicated_deduplication_window`
  deduplicates *insert blocks*, and the writer's four per-table flushers batch
  rows from many cycles and many sources on their own `max_rows`/interval
  cadence — a redelivered batch's rows land in a differently composed block,
  so no token can be 1:1 with a pushed batch. `ReplacingMergeTree` collapses
  on the sorting key, and two of the four sorting keys are not unique per row:
  `probe_cycle` orders by `(target_id, source, timestamp)` with no
  `target_group`, so it would merge the two `frankfurt` targets the group
  predicate exists to separate, and `probe_hop` orders by
  `(..., timestamp, ttl)` while rows are per `(ttl, responder)`, so it would
  collapse ECMP siblings. Fixing that means re-keying, which ClickHouse
  refuses on a key column, so it means rebuilding all four tables — and reads
  before a merge still see duplicates without `FINAL` on every query. An
  application guard is both cheaper and correct earlier.

  The ack now carries `{"accepted": n, "duplicate": d}` and the master logs
  `cluster ingest skipped redelivered cycles` when `d > 0`, at most one line
  per push: sustained duplicates mean acks are being lost between the peers,
  which is a link fault rather than a slave fault.

- **Slave push buffer + auth:** `slave.PushSink` is a fixed 600-cycle
  ring with drop-oldest on overflow; a failed push `Requeue`s on 5xx /
  network errors and drops on 404 (master lost state; next /register
  re-establishes us) and on `ErrRejected` — every 4xx except 401, 403, 404
  and the transient `retryable4xx` set. A 4xx the master
  will answer identically forever (a batch outside the ingest bounds, or
  one whose oldest cycle aged past `MaxCycleAge` during an outage) must not
  requeue: it head-of-line blocks the ring, so every later flush re-sends
  the same doomed batch while drop-oldest discards the live cycles behind
  it. The drop logs at Error with the master's own message, because a
  master, WAF or proxy answering 4xx to everything is now silent data loss.

  **`retryable4xx` is derived from each status's own specification**, not
  from the master's behaviour, because any intermediary on the path can
  answer: 407 (RFC 9110 §15.5.8 — the *proxy* wants credentials, which says
  nothing about the batch, and the master never emits it), 408 (§15.5.9,
  "the client MAY repeat the request without modifications"), 421 (§15.5.20,
  "the client MAY retry the request over a different connection" — a reverse
  proxy's routing condition), 425 (RFC 8470 §5.2) and 429 (RFC 6585 §4).
  409 is deliberately *not* in the set: its "resubmit" is conditioned on the
  user resolving the conflict, not on time passing. 421 additionally calls
  `CloseIdleConnections`, because the RFC's remedy is a *different* connection
  and retrying through the same pool reproduces the misroute on every flush.
  407 is retried knowing it cannot succeed until an operator supplies proxy
  credentials: a stuck retry loses the oldest cycles to drop-oldest, while
  dropping loses the whole batch immediately, for a verdict the master never
  issued.
  `TestRetryable4xxMatchesTheRFC` sweeps 400–499 so a status added without a
  citation reddens, and asserts none of 401/403/404 appear — those are
  classified before `retryable4xx` is consulted, so listing one there would
  make its own handling unreachable. The cost of a wrong entry is the
  head-of-line block above; the `push failed, requeueing` Warn on every
  flush is the signal. A 401 on any endpoint cancels the runner's context with cause =
  `ErrAuth` so the process exits non-zero and the operator must rotate
  the token. `ErrRejected` is likewise fatal at boot: `registerForever`
  and `pullConfigInitial` exit non-zero carrying the master's own
  message rather than retrying a verdict the master answers identically
  forever — an invalid `cluster.name` or an oversized advertise
  otherwise left a slave "running" while probing nothing. Mid-flight,
  `refreshLoop` still keeps the last-good config on any pull error,
  matching `Store.Reload`. Target-set fingerprint changes (group
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
  from a legitimate private peer address; empty opts a slave out entirely.
  `cluster.slave_addrs` optionally pins a slave to one address — a
  mismatch is refused a health entry, unpinned slaves are accepted so
  the feature works zero-config, and the pins follow the config
  hot-reload contract: the registry reads them through a live closure
  over `store.Current()` (`Registry.SetPinsFn`), re-checked both at
  `Touch` time and in `Peers()`, so a SIGHUP-edited pin drops a
  mismatched peer on the next scheduler signal without waiting for that
  slave's next heartbeat. A pin also **beats an unpinned claim**: adding
  one is the documented remedy for a squatter, so `Touch` releases an
  address whose current owner is unpinned when the heartbeating slave is
  pinned to it. Releasing only from an owner whose *own* pin excluded the
  address left an unpinned squatter holding it permanently — it keeps
  heartbeating, so `Sweep` never frees it, and the SIGHUP the operator
  was told to apply changed nothing. `cluster.health_hops` (default true)
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
  Exec failures log only the action name and a fixed `execFailureCategory`
  (timeout / exit code / start failed), matching the webhook and discord
  `httpFailureCategory` siblings: `a.Command` is env-expanded from the raw
  config bytes and can embed a resolved secret, `exec.Error` quotes
  argv[0], and the command's stdout+stderr is unbounded operator-script
  output. The `exec` action runs `cmd.Run()` with nil `Stdout`/`Stderr`
  and `cmd.WaitDelay = actionTimeout`: `CombinedOutput` builds a pipe, and
  `CommandContext` kills only the direct child, so any orphan holding the
  inherited descriptor — `notify.sh &`, `systemd-run`, a helper daemon —
  kept `Wait` blocked long past the deadline and wedged that shard's
  delivery worker for as long as it lived.

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
  exact bytes as its identity. `/hops/timeline` returns one row per
  `(slot, ttl)` whose address is the slot's worst-loss cycle's responder,
  so no rule identifies a terminal row — a slot's deepest row can be a
  silent hop with the slave's own address sitting above it — and every
  non-empty `IP` there is replaced with the sentinel
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
  `/hops`: it is the only thing left naming which row the trace ended at
  once the address is gone, and it discloses nothing that response does not
  already carry — intermediates keep their real addresses and a blanked row
  that answered keeps its RTTs and sub-100% loss, so the answering TTL is
  readable from the rows regardless. End-to-end loss is **not** read from
  it — that is `target_loss`, the cycle's own counters, which no hop row can
  reconstruct. The timeline neither selects nor serves the marker: every
  address there is the same sentinel, so it would be the one thing naming
  that TTL. `redactAllHopAddresses` clears it anyway, on a field
  `queryHopsGrid` already leaves false, because the cost of a redundant
  clear on a disclosure path is nothing and the cost of a later select
  adding it back is the leak.

  Because health targets live outside the stored config,
  `scheduler.LifecycleOptions.ExtraFingerprint` carries mesh membership
  into the rebuild decision (`Fingerprint(cfg)` alone can't see it), and
  registry changes share the debounced SIGHUP signal path so a fleet
  restart costs one rebuild, not one per registration.

- **Alert quorum:** `alerts.<name>.quorum` accepts `"majority"`
  (strictly more than half the live sources) or a positive integer
  (absolute minimum), gating dispatch only — per-source `sustained`
  counters stay independent. Sources stale beyond the liveness window — `livenessWindow`, which is
  `alertFreshness` = max(3× interval, `config.MaxFutureSkew`) — are
  pruned from the live count so a dead slave can't suppress a real
  alert. It is the freshness window rather than a bare 3× interval
  because cycles arrive in pushed batches on the slave's own
  `cluster.push_every` cadence, which config does not bound: any cadence
  whose cycles the freshness gate still evaluates must also keep its
  source live, or a bursty-but-healthy slave is pruned between pushes
  and a `"majority"` quorum collapses to whichever source delivers
  continuously, where `Threshold(1) == 1` fires on one. A cadence past
  the window is already losing cycles to the freshness gate and is
  warned about as `alert.source_excluded`. The quorum warm-up window
  stays 3× interval.

  **Every clock the evaluator keys on is the master's receive clock**
  (`Evaluator.nowFn`, injected, `time.Now` in production; read once per
  `OnCycle` pass). `alertState.lastSeen`, `tally`, `firingSources` and
  `aggWarmup.firstSeen` all use it. Keying them on `Cycle.Time` made the
  liveness clock slave-supplied: ingest accepts `config.MaxFutureSkew` (5m)
  of forward skew and the staleness window at a 20s interval is 60s, so one
  hostile slave dating a cycle forward pruned every honest source and became
  a majority of itself — in either direction, manufacturing a page or
  silencing a real outage. The same skew backwards satisfied warm-up's
  window on a slave's very first cycle. The cycle's own timestamp still
  reaches storage and still stamps `Event.Time`; it is only not a clock.

  A cycle older than `alertFreshness` = max(3× interval,
  `config.MaxFutureSkew`) is **not evaluated at all** — returned on like a
  cycle that sent nothing, so the source ages out rather than voting on
  replayed data. Alerting is a statement about now, and ingest accepts a
  cycle up to `config.MaxCycleAge` (7d) old, so without this a slave could
  replay a week of history into sustained-state transitions. The window is
  never tighter than the liveness window it feeds, or a slow-interval
  deployment could not keep a source live, and never tighter than the skew
  ingest already tolerates, or an honest slave at the accepted limit is
  silently excluded. The cost: a slave whose clock lags the master by more
  than that window stops contributing to alerts (its data is still stored),
  and a backlog delivered after an outage longer than the window is stored
  but not alerted on. Both are reported at **Warn** as
  `alert.source_excluded`, not Debug: a source contributing nothing to
  alerting while its data keeps arriving is the failure alerting exists to
  prevent. A stably skewed source would emit one line per target per interval,
  so each `(source, reason)` is logged once per freshness window and carries
  the count it suppressed; the same window evicts the entry, and source names
  are already bounded by the master's registry. `reason` is `clock_skew` for
  the freshness refusal and `duplicate_cycle` for the guard below. The line
  carries `example_target`, never a bare `target`: the record is keyed by
  source, so `suppressed` spans every target that source reports on and a
  single target name would misdirect the investigation. There is no
  health-endpoint field: an API field needs a read site to be worth declaring,
  and the log is what an operator's alerting-on-alerting consumes.

  **A cycle is evaluated once while its identity is still held, in
  order.** `alertState.seenCycle` /
  `lastCycle` hold whether and when a cycle was last accepted per source —
  two fields rather than one because `lastCycle`'s zero value would
  otherwise mean *admit*, and any producer stamping nothing would disable
  the guard for its source forever. The timestamp is the
  `(target, source, timestamp)` tuple that *identifies* one measurement, the
  same tuple `master.cycleDedup` keys on. That one does not make this one
  redundant: it is a window of identities and this is an ordering guard, and
  they disagree exactly where it matters — an unseen *older* cycle is a real measurement ingest must store
  and alerting must not apply, so ingest admits it and this skips it. This is
  also the only guard on the local probe path, which never reaches ingest, and
  the one that still holds past `dedupWindowPerSource`.
  A non-increasing cycle is skipped **before** any state mutation and
  before `lastSeen`. `PushSink.Requeue` resends a batch on any 5xx or network
  error, so a lost ack redelivers the same measurement: applied twice it
  incremented `consecHits` twice and fired a `sustained: 2` alert off one bad
  cycle, and an older healthy batch delivered late cleared a newer firing
  state. Skipping before `lastSeen` is what makes it fail closed — a source
  that only replays ages out of the quorum denominator rather than
  voting healthy from its ring. `tally`'s staleness prune drops only
  quorum participation — `state` and `consecHits` reset to what a
  recreated entry would hold — never the replay identity: deleting the
  whole `alertState` recreated it with `seenCycle` false, which admits
  anything, so a lost-ack redelivery of a pruned source's cycle was
  applied a second time whenever its stamp was still alert-fresh,
  resolving a live alert or refiring a sustained one off replayed data.
  The identity is deleted only once `now − lastCycle` exceeds
  `alertFreshness`, past which every stamp it holds is refused by the
  freshness gate upstream and retention buys nothing. A *time-based*
  global sweep was tried and reverted: retention is load-bearing,
  because a silent source's `StateFiring` is what dispatches its
  eventual resolve. Two bounded sweeps replace it. `pruneQuietSources` runs
  on the **non-quorum** path, which `tally` never reaches, so an alert
  without quorum no longer keeps an `alertState` — `ahead` slice included —
  for every source name that ever pushed to it, scanned by `firingSources`
  under `e.mu` on every transition. Its rule is `tally`'s, with one
  exemption: a source in `StateFiring` is never reaped whatever its age,
  because a per-source alert dispatches its resolve from exactly that
  state and a recreated entry starts at `StateOK`, which makes the
  recovery a non-transition and leaves the page open. `pruneDepartedTargets`
  sweeps on `Refresh`, dropping a `(target, alert)` **pair** the reloaded
  config no longer names — keyed on the pair, because an alert that still
  exists for other targets but is detached from this one can produce no
  further cycle for this key either, and `evaluate` only ever iterates the
  target's own `Alerts` list. The health group is exempt because it lives
  outside the stored config, and its group is recovered with the **first**
  `/` — `TargetRef.ID()` joins on that one, so `path.Dir`'s last-slash
  reading turned a slave named `eu/fra1` into group `_cluster/eu`, missed
  the exemption, and deleted the state a firing slave-down alert needs to
  resolve. The cost is that a producer whose clock steps
  backwards contributes nothing until its clock passes the newest timestamp
  accepted from it, which is why that skip is warned about rather than counted
  silently.

  **A cycle ahead of the master's clock is ordered separately, never
  unordered.** `cy.Time` is an identity here, never a clock — the same rule
  `lastSeen` follows — and ingest accepts one `config.MaxFutureSkew` ahead, so
  one floor over both kinds of stamp cannot serve: raising it to a
  forward-dated cycle skipped every genuine cycle behind it *before*
  `lastSeen` and let `tally` evict the source, a per-name mute that empties
  the quorum denominator until `live` reaches 0 and every alert resolves.
  `alertState` therefore carries **two** rising marks and one exact set, all
  in `admits`/`accept`: `lastCycle` over every accepted stamp, `pastCycle`
  over only those that were not ahead of the clock, and `ahead`, the stamps
  accepted while they were. A cycle ahead of the clock must beat `lastCycle`;
  one behind it must beat `pastCycle` **and** miss `ahead`. Neither of the two
  simpler repairs works: clamping the stored floor to `now` reopens the replay
  for any slave whose clock runs a millisecond fast, which is most of them
  (`TestDuplicateIsCaughtWhenTheSlaveClockRunsAhead` pins it), and simply
  disarming the floor while it sits in the future re-admitted the
  forward-dated cycle itself — directly, and again once genuine cycles had
  moved the floor behind it, which 1024 identities of ingest dedup then let
  arrive as a redelivery. `ahead` is why the exact stamp is still recognised
  after the clock passes it, when no ordering mark separates it from a genuine
  cycle of the same age; `pastCycle` is why an ordinary replay never needs the
  set at all, so the slice stays nil for every source whose clock is not
  ahead. It is bounded by `aheadCap` — the skew plus one `alertFreshness`
  window, over which an honest producer emits one cycle per interval per
  target, so 11 entries at the 1m interval and 31 at 20s — capped at
  `min(derived, aheadCeiling)`, because config bounds no interval from below
  and the derivation alone asks for ~6e11 int64 at a 1ns schedule, which
  `slices.Contains` and `slices.DeleteFunc` then walk per cycle.
  `aheadCeiling` is `cluster.MaxCyclesPerBatch` (1024, 8 KiB of int64 per
  target+source): an entry exists to recognise a redelivery, the redelivery
  unit is one batch, and `master.cycleDedup` holds that many identities per
  source *across every target it reports* — so past this depth the window
  upstream has rolled too and a longer slice catches nothing it would not
  have caught first.

  **Two residuals, both deliberate.** Eviction is oldest-first and fails open
  to the pre-guard double-apply rather than to silence, like both of
  `cycleDedup`'s own eviction paths — so a stamp evicted from `ahead` while
  still alert-fresh, and also rolled out of the 1024 identities of ingest
  dedup, is applied a second time when the master's clock passes it. Reaching
  that takes a token holder posting more forward-dated cycles than either
  window holds; closing it means remembering identities without a bound,
  which is the defect and not the fix. And ordering the ahead arm against
  `lastCycle` skips every *genuine* cycle stamped between the master's clock
  and an accepted forward-dated one, for as long as those stamps are
  themselves still ahead — not merely one landing on the same nanosecond.

  A quorum alert also has a warm-up: it won't dispatch FIRING until either
  2 distinct sources have reported for that target+alert or the
  3×-interval window has elapsed, so a master restart doesn't page and
  immediately resolve on partial data. Warm-up state is swept on `Refresh`
  by the same rule as the aggregate: a quorum alert that never dispatched
  has a warmup entry and no agg entry, so sweeping only agg's keys leaked
  entries for alerts that left the config and kept a stale `firstSeen`
  across a disable/re-enable — the elapsed-window arm then paged on the
  first partial-data evaluation, the exact flap warm-up exists to prevent.
  Webhook/log templates get `{{.Firing}}` and `{{.Live}}` (both 0 for
  non-quorum alerts).

  Dispatch does not run on the caller's goroutine. The transition is
  committed before it and the path is change-gated with no renotify, so a
  caller's spent deadline silently dropped the only FIRING notification an
  alert would ever send while the state read as delivered — the first
  payload the endpoint saw was the resolve for a page never sent.
  Detaching the context alone was not enough: `Dispatch` runs an event's
  actions in sequence under `alert.actionTimeout` each, and ingest
  delivers a batch's cycles in sequence, so bounding one transition left
  their sum unbounded and one push against an endpoint that accepts but
  never answers pinned the ingest handler for hours. `Evaluator` therefore
  queues committed transitions to `dispatchShards` (8) delivery workers,
  each with its own FIFO, a `(target, alert)` pair hashed onto exactly one
  of them so a firing still precedes its own resolve. Sharding is what
  bounds the blast radius: `Dispatch` bounds itself at `actionTimeout` per
  action, so a dead endpoint does not park a worker forever — it caps that
  worker's delivery *rate*, and on one worker that rate was the whole
  fleet's, every target's page queued behind an endpoint that was not
  theirs. `dispatchQueueDepth` is
  `cluster.MaxCyclesPerBatch`, the burst it absorbs rather than the
  producer's maximum: `evaluate` emits one Event per alert a target names
  and config bounds that count nowhere, so a batch produces a multiple of
  it. It does not need to be the maximum, because **a refused transition
  is reverted, not lost** — the enqueue happens under `e.mu` at the moment
  of commit, so a full queue undoes the state change and the next cycle
  re-detects and re-dispatches it, *for as long as the condition holds*.
  A condition that clears while the queue is still full is never paged:
  dispatch is change-gated with no renotify, and closing that would mean
  a ledger of owed pages rather than a state machine. `consecHits` is not
  reverted with the state — it counts cycles the condition held, which is
  a fact about measurements, not deliveries, and reverting it changes no
  outcome (verified against the transient case). Keeping it would be a page that never
  happens: dispatch is change-gated with no renotify, so a
  committed-but-undelivered FIRING leaves the endpoint's first payload the
  resolve for it. Refusals are logged at Error and served as
  `alert_dispatch_refusals` on `/api/v1/health`, named for what they are
  rather than as drops: the transition is retried, so a rising count is a
  delivery backlog and not missing pages. It is split across the shards as
  `dispatchShardDepth`, so the queued total is unchanged.

  The depth bounds the count, not the bytes: an Event retains its whole
  `Cycle`, and a worst-case one carries `config.MaxPingsPerCycle` RTTs
  beside `config.MaxHopRowsPerCycle` hop rows of `cluster.MaxRTTsPerHop`
  each — ~854 KB, so the depth alone admitted 874 MB of queued
  notifications — and the largest an Event ingest accepts is bigger still,
  `config.MaxPingsPerCycle` RTTs beside `cluster.MaxHopsPerCycle` hop rows
  of `cluster.MaxRTTsPerHop` each, ~1.18 MB. `dispatchQueueBytes` (64 MiB)
  is the second ceiling, split as `dispatchShardBytes` and reserved under
  `e.mu` against that shard's own counter. **Per shard, for the same reason
  the depth is:** one global counter meant a single stalled shard's backlog
  refused transitions on all eight, putting the fleet-wide coupling back
  through the memory bound. It is inert at any shape a probe emits — a
  10×30 MTR walk is ~47 KB, the deployed 20-ping cycle a few KB, so the
  depth is reached first — and a refusal on it is the same
  revert-and-retry, logged with `reason=bytes` against the depth's
  `reason=depth` because the two call for different remedies. Refusals are
  collected and logged **after** `e.mu` is released, one line per pass:
  `slog` writes synchronously, so a line under the evaluator's only lock
  stalled `OnCycle` for every other target on the one path that runs when
  delivery is already backed up.

  A transition the `Dispatcher` would discard never enters the queue at
  all. `DispatchFilter` is the optional interface it declares that with —
  `ActionDispatcher.Wants` is the same enters-or-leaves-firing gate
  `Dispatch` opens with — because which transitions notify anyone is the
  dispatcher's policy while the queue is the evaluator's bounded resource,
  so the policy is asked rather than duplicated. Without it a flapping
  `sustained: 3` alert filled a stalled shard with `OK→PENDING` and
  `PENDING→OK` events that can page nobody, and the real `OK→FIRING`
  behind them was refused and delayed. A `Dispatcher` that does not
  implement it still receives every transition. Each worker carries its own
  `recover()` — dispatch ran inside `scheduler.Fanout`'s while it was
  inline, and that perimeter has to move with the work or a `Dispatcher`
  panic takes the process down. `Close` signals the workers and returns
  without waiting — one may be inside a delivery that never answers, which
  is the wait every other goroutine is being spared.

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
