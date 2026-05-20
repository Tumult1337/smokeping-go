# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

gosmokeping is a single-binary Go replacement for SmokePing. It probes network
targets (ICMP/TCP/HTTP/DNS), writes results to InfluxDB v2, serves a React+uPlot
UI plus a JSON API, and can fire threshold alerts.

## Commands

```bash
make ui                 # vite build → internal/ui/dist/ (required before `make build`)
make build              # builds UI first, then `go build`
make build-nui          # Go-only build (UI must already be built or dist empty)
make dev                # go run with -log-level debug
make ui-dev             # vite dev server on :5173, proxies /api to :8080

make test               # unit tests
make test-integration   # needs INFLUX_URL/INFLUX_TOKEN/INFLUX_ORG
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

- **Storage tiering (v2 vs v3):** the `Resolution` abstraction is shared
  but the two backends realise it differently:
  - **influxv2** (default): four buckets (`smokeping_raw`, `smokeping_5m`,
    `smokeping_1h`, `smokeping_1d`) populated by Flux tasks that
    `influxv2.Bootstrap` installs at startup. The Writer only writes the
    raw bucket; rollups are InfluxDB's job. Per-ping samples
    (`probe_rtt` measurement) and MTR hops (`probe_hop`) only live in
    the raw bucket — rollups cover `probe_cycle` only.
  - **influxv3**: a single database. `influxv3.Reader` translates the
    requested `Resolution` into a SQL `date_bin()` width at query time —
    v3 has no Flux task equivalent and its columnar Parquet storage
    makes wide aggregations cheap, so query-time bucketing is the right
    trade. Pre-baked rollups via the Processing Engine downsampler plugin
    are an operator-side option (not shipped from Go).
  Either way `storage.PickResolution` is the single decision point — only
  the realisation downstream changes. Tier breakpoints are picked so each
  range button keeps its point count near the chart canvas width (~666
  px): ≤6h → raw, ≤24h → 5m, ≤180d → 1h, >180d → 1d. The 5m tier is
  optional in `InfluxV2.Bucket5m`; when unset the v2 reader's
  `fallbackChain` walks down to raw automatically (slower, but correct).

- **Hop write policy:** `storage.hop_policy.mode` ∈ `always`|`on_loss`|
  `sampled` gates the `probe_hop` write loop in both v2 and v3 writers.
  `always` (default) keeps the legacy behaviour; `on_loss` writes hops
  only when the last hop in the trace reports `Lost > 0`; `sampled`
  writes one cycle per `sample_every` window per `(target, source)` —
  a loss cycle consumes the slot, otherwise a baseline snapshot does
  (loss wins, baseline fills the gap). The gate lives in `internal/storage/hoppolicy.go`
  and is consulted from both backend writers — adding a new writer means
  threading the same `*storage.HopPolicy` through its constructor. The
  policy is constructed once at startup in `cmd/gosmokeping/storage.go`
  (`buildHopPolicy`) and not hot-reloaded, so a config change requires a
  process restart to bind.

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

- **Rollup task versioning:** `storage/bootstrap.go` names tasks with a
  `-vN` suffix. Changing the aggregation Flux (new percentile fields, etc.)
  requires bumping the suffix AND adding all prior names to
  `deleteObsoleteTasks` so upgrades replace rather than duplicate. InfluxDB
  doesn't diff task bodies — same name = skip.

- **UI time-axis contract:** `/api/v1/targets/{id}/cycles` echoes the `from`
  and `to` it resolved. The charts pin `scales.x.range` to those unix
  timestamps so a wide window with sparse data still renders at the full
  requested span. Don't recompute the window client-side from the range
  string — use the server's echo.

- **Cluster mode (master/slave):** `--slave` flips the binary into a
  runner that registers with a master, pulls the target list over HTTP,
  probes locally, and pushes cycle batches back. Slaves never touch
  InfluxDB or the UI. The master's cluster endpoints live under
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

`storage.backend` selects the persistence layer: `"influxv2"` (default;
three-bucket rollup tasks, Flux queries) or `"influxv3"` (single database,
SQL/Flight queries, query-time `date_bin` aggregation). The UI and alert
evaluator are backend-agnostic — they only see `storage.Reader` and
`scheduler.Sink`. Pick v3 when slave fan-out is producing more write load
than v2's TSM ingestion can keep up with; v2 is the right default for
single-node and small-cluster deployments.

The v3 backend's bootstrap creates the configured database via
`POST /api/v3/configure/database` if missing, which **requires a token
with admin scope**. A write-only token will fail bootstrap with a 401/403;
either grant admin to the configured token or pre-create the database
externally (e.g. via `influxdb3 create database`). The v3 writer wraps
the official `influxdb3/batching.Batcher` with a 1s ticker so writes
flush either at 1000 points or every second — sync per-cycle writes
would defeat the point of picking v3 for write throughput.

## Integration tests

Behind the `integration` build tag. Set `INFLUX_URL`, `INFLUX_TOKEN`, and
`INFLUX_ORG`; the tests use dedicated `gosmokeping_test_*` buckets so they
don't collide with production data.
