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
  `cmd/gosmokeping/main.go`.

- **Config hot-reload:** `config.Store` uses `atomic.Pointer[Config]`.
  Consumers (API, alert evaluator) call `store.Current()` on every request
  rather than caching — this is intentional so SIGHUP and `POST /api/v1/config/reload`
  take effect immediately without pointer-update races. When adding a
  consumer, do **not** cache the `*Config`.

- **Storage tiering:** three InfluxDB buckets (`smokeping_raw`, `smokeping_1h`,
  `smokeping_1d`) populated by Flux tasks that `storage.Bootstrap` installs on
  startup. The Writer only writes the raw bucket; rollups are InfluxDB's job.
  `storage.PickResolution` maps a requested time span to a bucket so the UI
  can query cheaply at wide zoom levels. Per-ping samples (`probe_rtt`
  measurement) only live in the raw bucket — rollups keep aggregates only.

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

## Config

`config.example.json` is the canonical reference. Env expansion happens on the
raw bytes before JSON parse (`${NAME}` form), so tokens can live in env vars.

## Integration tests

Behind the `integration` build tag. Set `INFLUX_URL`, `INFLUX_TOKEN`, and
`INFLUX_ORG`; the tests use dedicated `gosmokeping_test_*` buckets so they
don't collide with production data.
