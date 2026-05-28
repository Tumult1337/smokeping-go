# gosmokeping

> **v1+ uses ClickHouse.** InfluxDB v2/v3 support is archived on the
> `legacy/influx` branch (last released as the `v0-last-influx` tag).
> A ClickHouse server (current LTS, e.g. `25.3 LTS` at the time of writing)
> is required. Quick start:
> `docker run -d -p 9000:9000 clickhouse/clickhouse-server:lts`

A modern, single-binary replacement for [SmokePing](https://oss.oetiker.ch/smokeping/).
Keeps the classic "smoke band" latency visualisation (min–max + p5–p95 + median),
adds a JSON API, a React + uPlot UI, ClickHouse storage, MTR path discovery,
HTTP/DNS probes, and optional master/slave distributed probing from multiple
vantage points.

> **Heads up — this project is AI-coded.**
> Every line of Go, TypeScript, and CSS in this repo was written by
> Claude under human direction. It's been shaped by iterative review, used
> in anger, and the tests are real — but there is no seasoned human
> maintainer standing behind each commit. Treat it like any other
> unpaid-volunteer hobby tool: read before deploying, pin versions, don't
> put it on the critical path without your own review.

## Screenshots

**ICMP — smoke band mode, 5 probe sources, 6 h window:**

![ICMP smoke band — multi-source 6h](docs/screenshots/icmp-band.png)

**ICMP — classic SmokePing per-cycle bars mode:**

![ICMP per-cycle bars — multi-source 6h](docs/screenshots/icmp-bars.png)

Both views include a per-source legend with live percentile readouts, a stats
bar (median / p95 / min–max / loss%), the latest MTR hop table (TTL, IP,
loss%, min/avg/max, latency bar), and a per-hop loss heatmap over the same
window. Clicking any column in the heatmap or any point on the chart pins the
hop table to that cycle.

**HTTP probe — per-request RTT bars colour-coded by status class:**

![HTTP status + response time — multi-source 6h](docs/screenshots/http.png)

Source chips above each chart let you drill into a single vantage point;
the "all" view overlays every source with its own palette colour.

## Features

- **Probes:** ICMP (unprivileged UDP ping sockets, raw fallback), TCP connect,
  HTTP(S) TTFB, DNS lookup, MTR-style path discovery.
- **Storage:** ClickHouse. Four `MergeTree` tables (`probe_cycle`, `probe_rtt`,
  `probe_hop`, `probe_http`) with codec-stacked columns (Gorilla for floats,
  T64 for small ints, DoubleDelta for timestamps, ZSTD second pass). Tier-ladder
  bucketing at query time via `toStartOfInterval` — no materialised views, no
  rollup tasks. Bootstrap creates the database, tables, and per-table TTLs on
  every start (idempotent). Cluster mode rewrites to `ReplicatedMergeTree`
  when `storage.clickhouse.cluster` is set.
- **UI:** React + Vite + uPlot, embedded in the binary. Smoke band and
  classic-bars chart modes, MTR hop table, per-hop loss heatmap, drag-to-zoom,
  shareable URLs, auto-refresh.
- **Multi-source:** run extra instances with `--slave` to probe from multiple
  vantage points; the master aggregates, persists, and presents all sources
  in a single UI. Per-target slave assignment supported.
- **Alerting:** threshold conditions with sustained-cycles debounce.
  Actions: `log`, shell `exec`, generic `webhook`, and a first-class
  `discord` embed (includes MTR path for icmp/mtr targets).
- **Hot reload:** `SIGHUP` re-reads the config without dropping cycles.

## Quick start

Prerequisites: Go 1.26+, Node 20+, a running ClickHouse instance (current LTS).

### 1. Start ClickHouse and grab credentials

```bash
docker run -d --name clickhouse -p 9000:9000 \
  clickhouse/clickhouse-server:lts
```

gosmokeping creates the `probe_cycle`, `probe_rtt`, `probe_hop`, and `probe_http`
tables with automatic rollup policies on first start.

### 2. Configure

```bash
cp config.example.json config.json
cp .env.example .env
```

Edit `.env`:

```bash
CH_ADDR=127.0.0.1:9000
CH_DATABASE=gosmokeping
CH_USERNAME=default
CH_PASSWORD=
```

`config.example.json` already interpolates these as `${CH_ADDR}`, etc.
Edit `config.json` to list the hosts you want to probe — the example ships
with `1.1.1.1` and `8.8.8.8` so the first run is immediately useful.

### 3. Build

```bash
make ui     # vite build → internal/ui/dist/
make build  # go build → ./gosmokeping
```

Or with Docker:

```bash
docker build -t gosmokeping .
```

### 4. Grant raw-socket capability

ICMP and MTR need raw sockets. Either run as root, or:

```bash
make setcap   # sudo setcap cap_net_raw+ep ./gosmokeping
```

The Docker image and the bundled systemd unit handle this automatically.

### 5. Run

```bash
./gosmokeping -config config.json
```

Open <http://localhost:8080>. Cycles start landing within one probe
interval (default 5 min; `config.example.json` overrides to 30 s).

## Config

See [`config.example.json`](config.example.json). Environment variables
(`${NAME}`) are expanded at load time, so tokens stay in env vars. `SIGHUP`
re-reads the file live.

`.env` is auto-loaded from the directory holding `--config` first, then
from the current working directory (important under systemd where cwd is `/`).
Real shell env always wins over `.env`; a missing `.env` is silently ignored.

### Storage backend

ClickHouse is the only storage backend. The configured user must have permissions
to `CREATE`, `INSERT`, `SELECT`, and `ALTER` on the target database. The default
database is `smokeping`; override with `storage.clickhouse.database` in the config.

### Managing table retention and size

Per-table TTL is set at bootstrap from `storage.clickhouse.retention`. Defaults
are generous; tighten them on busy installs where `probe_hop` and `probe_rtt`
dominate disk usage.

```json
"storage": {
  "clickhouse": {
    "retention": {
      "cycle_days": 365,
      "rtt_days":   14,
      "hop_days":   90,
      "http_days":  14
    }
  }
}
```

| Table | Default | What it costs you to shorten |
|-------|---------|------------------------------|
| `probe_cycle` | 365d | Cycle-level history (min/avg/max/p5..p95, loss). Drop below 30d only if you don't need long-window trend charts. |
| `probe_rtt` | 14d | Per-ping raw RTT. Used to draw "smoke" bands at short zooms; aggregates in `probe_cycle` retain p5..p95. |
| `probe_hop` | 90d | Per-cycle hop stats (MTR + opportunistic ICMP traces). Heaviest table on multi-target fleets. |
| `probe_http` | 14d | Per-request HTTP samples. Aggregates survive in `probe_cycle`. |

TTL changes take effect on next process start (bootstrap re-emits
`ALTER TABLE … MODIFY TTL` on every start). Existing rows older than the new
window are pruned by the next CH merge cycle, not immediately.

### Alert conditions

Each alert's `condition` is a single predicate `<field> <op> <value>`. No
AND/OR — to combine criteria, attach multiple alerts to the target.

| Field | Unit | Notes |
|-------|------|-------|
| `loss_pct` | percent | Target-level loss. MTR mirrors the final hop (target) or reports full loss when unreachable; intermediate hop drops are ignored. |
| `rtt_min`, `rtt_max`, `rtt_mean`, `rtt_median`, `rtt_stddev` | ms | Per-cycle summary across the cycle's RTT samples. |
| `rtt_p5`, `rtt_p95` | ms | Other percentiles (`p10`..`p90`) are computed and stored, but not currently accepted as alert fields. |

Operators: `>` `>=` `<` `<=` `==` `!=`. Avoid `==`/`!=` on `rtt_*` —
those are float milliseconds. `rtt_*` values accept Go duration syntax
(`50ms`, `1.5s`); a bare number is interpreted as milliseconds.

```json
"alerts": {
  "high-loss":    { "condition": "loss_pct > 5",      "sustained": 3, "actions": ["log"] },
  "high-latency": { "condition": "rtt_median > 50ms", "sustained": 5, "actions": ["log"] }
}
```

**Sustained / debounce.** `sustained: N` requires `N` consecutive bad
cycles before the alert transitions to `firing` (with a `pending` event
on the first bad cycle if `N > 1`). `0` and `1` are equivalent — first
bad cycle fires immediately. A single good cycle resets the counter and
transitions back to `ok`; there is no hysteresis, so a metric oscillating
across the threshold will produce a transition per cycle.

**Full-loss cycles.** When every probe in a cycle fails, all `rtt_*`
fields read as `0`, so a condition like `rtt_median > 50` will *not*
fire on an outage — pair it with a `loss_pct` alert if you want both
"slow" and "down" coverage.

**State lifetime.** Alert state lives in memory only. Restarts return
every alert to `ok`; if a target is still bad, the next cycle re-walks
`ok → pending/firing` and re-emits the event. In cluster mode state is
keyed by `(target, alert, source)`, so a slave firing does not affect
the master's counter for the same target.

### Alert actions

| Type | Shape |
|------|-------|
| `log` | Structured log line. |
| `webhook` | Generic JSON POST; `template` overrides the default body. |
| `discord` | Rich embed with RTT, loss, sustained-cycle count, and MTR path. |
| `exec` | Shell command with alert payload in env vars. |

```json
"actions": {
  "slack":   { "type": "webhook", "url": "https://hooks.slack.com/..." },
  "discord": { "type": "discord", "url": "${DISCORD_WEBHOOK_URL}" },
  "page":    { "type": "exec",    "command": "/usr/local/bin/pager" }
}
```

## HTTP API

| Method | Path | Purpose |
|--------|------|---------|
| GET | `/api/v1/health` | Health + uptime |
| GET | `/api/v1/sources` | List source labels seen in storage (master + slave names) |
| GET | `/api/v1/targets` | List all targets |
| GET | `/api/v1/targets/{group}/{name}/status` | Latest cycle for the target |
| GET | `/api/v1/targets/{group}/{name}/cycles?from&to` | Aggregated latency, optionally bucketed |
| GET | `/api/v1/targets/{group}/{name}/rtts?from&to` | Raw per-ping samples |
| GET | `/api/v1/targets/{group}/{name}/http?from&to` | Raw HTTP samples |
| GET | `/api/v1/targets/{group}/{name}/hops?at=<unix>` | Latest MTR path, or the one nearest `at` |
| GET | `/api/v1/targets/{group}/{name}/hops/timeline?from&to` | Per-hop loss history |

`from` / `to` accept RFC3339, unix seconds, or relative durations like `-24h`.
The bucket width on `/cycles` and `/hops/timeline` is picked server-side from
the window. Cycles ladder: ≤2h raw, ≤24h 2m, ≤7d 15m, ≤30d 1h, ≤180d 6h,
>180d 1d. Hops ladder: ≤2h raw, ≤24h 5m, >24h 15m (timeline capped at 7d).
Cycles also accepts a back-compat `step=raw|1h|1d` override.

All endpoints accept `source=<name>` to filter by probe origin in master/slave
deployments. `/cycles` and `/hops/timeline` echo the resolved `from`/`to` in
the response so the UI can pin its x-axis exactly to what the server returned.

## Deployment

**Docker compose on a host that already runs ClickHouse natively.** The repo
ships a `docker-compose.yml` for this shape — single service, `network_mode:
host` so the binary reaches CH on `127.0.0.1:9000` with no bridge NAT and ICMP
probes see real interfaces. After cloning, copy `config.example.json` and
`.env.example` to `config.json` / `.env`, fill them in, then:

```bash
git pull && make deploy
```

`make deploy` checks the two files exist, runs `docker compose build` and
`docker compose up -d`, and reports `docker compose ps`. Use this on the
deploy VM as the steady-state update flow.

**Docker compose with CH in the stack** (CH not on the host). See
[`docs/docker-master.md`](docs/docker-master.md) — adds a `clickhouse` service
alongside, walks through CH user setup, master/slave layout, and reverse-proxy
recommendations.

**Docker, ad-hoc.** The image runs as an unprivileged user and grants
`CAP_NET_RAW` to the binary via `setcap`:

```bash
docker run -d --name gosmokeping -p 8080:8080 \
  -v $(pwd)/config.json:/opt/smokeping/config.json:ro \
  -v $(pwd)/.env:/opt/smokeping/.env:ro \
  gosmokeping
```

**systemd** — run `sudo ./deploy/install.sh` after `make build`. Creates a
`gosmokeping` system user, installs the binary to `/opt/smokeping/gosmokeping`,
stages `/opt/smokeping/` for config + `.env`, and installs the unit file. The
unit grants `CAP_NET_RAW` via systemd — no `setcap` needed. Re-run to update;
it's idempotent.

**Reverse proxy** — terminate TLS and authenticate at the proxy (Nginx/Caddy).
The binary has no built-in auth.

## Master / slave

Run extra `--slave` instances to probe every target from multiple vantage
points simultaneously. The master aggregates all cycles, persists to ClickHouse,
and shows each source as a separately filterable overlay in the UI.

**Master** `config.json`:

```json
"cluster": {
  "token": "${CLUSTER_TOKEN}",
  "source": "master"
}
```

By default every target is probed by the master and all registered slaves.
To restrict a target to specific slaves only (master skips it locally):

```json
{ "name": "Berlin POP", "host": "10.0.1.1", "slaves": ["berlin-1"] }
```

**Slave** — copy [`config.slave.example.json`](config.slave.example.json),
set `master_url`, `token`, and a unique `name`, then:

```bash
./gosmokeping --slave -config config.slave.json
```

The slave registers with the master, pulls the target list, probes locally,
and pushes cycle batches back every few seconds. A 600-cycle ring buffer
absorbs short master outages. Slaves never touch ClickHouse or the UI.

## Development

```bash
make dev               # go run with debug logging
make ui-dev            # vite dev server on :5173 (proxies /api to :8080)
make test              # unit tests
make test-integration  # requires CLICKHOUSE_ADDR / CLICKHOUSE_PASSWORD
make lint              # go vet
```

See [`CLAUDE.md`](CLAUDE.md) for architecture notes covering the
scheduler-as-hub pipeline, config hot-reload contract, storage tiering,
ICMP socket quirks, MTR trace semantics, rollup task versioning, and the
UI time-axis contract.

## Layout

```
cmd/gosmokeping/    # entrypoint (main, run_node, run_slave, logger)
internal/
  alert/            # threshold evaluator + dispatchers (log/webhook/discord/exec)
  api/              # chi router + handlers
  cluster/          # master+slave HTTP protocol and runners
  config/           # JSON loader + hot-reload store
  probe/            # ICMP/TCP/HTTP/DNS/MTR + shared TTL-walk trace
  scheduler/        # per-target probe scheduler + sink fanout
  stats/            # RTT aggregation (min/max/mean/median/p5–p95/stddev)
  storage/          # ClickHouse writer, reader, bootstrap
  ui/               # embed.FS wrapper for the built SPA
ui/                 # React + Vite + uPlot SPA source
deploy/             # systemd unit + install script
docs/screenshots/   # README screenshots
```

## Upgrading from the InfluxDB-era release

If you were running gosmokeping before the ClickHouse migration (any commit
on the `legacy/influx` branch, or the `v0-last-influx` tag), see
[`docs/migrate-from-influx.md`](docs/migrate-from-influx.md) — there is no
schema-level data migration, but the doc covers the orderly cutover (export
what you want to keep, stand up CH, swap the config, leave the legacy stack
running side-by-side if you need historical access).

## Migrating from SmokePing

`smokeping2gosmokeping` reads a SmokePing `Config::Grammar` config and emits
an equivalent gosmokeping JSON config. It follows `@include` directives and
translates the common probe/alert shapes.

```bash
smokeping2gosmokeping -in /etc/smokeping/config -out config.json
# writes config.json and config.json.notes.txt
```

Storage credentials are emitted as `${CH_ADDR}` / `${CH_PASSWORD}` placeholders
matching `.env.example`. Add `-strict` to exit 2 on any untranslatable
construct, useful in CI-driven config generation.

## License

MIT. Fork, break, improve.
