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

- **Storage backend:** single ClickHouse backend in `internal/storage/clickhouse/`.
  Four `MergeTree` tables (`probe_cycle`, `probe_rtt`, `probe_hop`, `probe_http`)
  with codec-stacked columns (Gorilla for floats, T64 for small ints,
  DoubleDelta for timestamps, ZSTD as second pass). The reader buckets at
  query time via `toStartOfInterval` — no materialised views, no rollup
  tasks. `QueryFilter.Step` carries the bucket width; `pickCycleStep` and
  `pickHopStep` in the CH reader are the single decision points. Tier ladders:
  - cycles: ≤24h raw, ≤180d 1h, >180d 1d
  - hops:   ≤24h raw, >24h 15m (API caps timeline at 7d)
  Bucketed cycle percentiles are computed with `quantilesExactWeighted`
  over the per-cycle percentile columns weighted by `sent` — information-
  preserving relative to NULL. Bucketed hop queries keep `hop_addr` in
  the `GROUP BY` so a path flap returns one row per distinct address
  seen in the bucket.

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
  shared TTL-walk helper in `internal/probe/trace.go`. The `MTR` probe uses
  its return (`hops`, `reached`, err) directly; the `ICMP` probe calls it
  after its echo batch so every icmp target also gets a hops view for free.
  Trace needs `CAP_NET_RAW` — callers distinguish the permission error with
  `errors.Is(err, errRawUnavailable)` and skip gracefully. When the target
  never replies within `maxTTL`, `reached=false` and MTR reports full loss
  instead of mirroring the final intermediate hop.

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
  and idempotent. Column additions require `ALTER TABLE … ADD COLUMN` — the
  bootstrap does not currently do this automatically. TTL changes are applied
  on every start via `ALTER TABLE … MODIFY TTL`, so they take effect on the
  next restart.

- **UI time-axis contract:** `/api/v1/targets/{id}/cycles` echoes the `from`
  and `to` it resolved. The charts pin `scales.x.range` to those unix
  timestamps so a wide window with sparse data still renders at the full
  requested span. Don't recompute the window client-side from the range
  string — use the server's echo.

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
