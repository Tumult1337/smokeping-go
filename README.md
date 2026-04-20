# gosmokeping

A modern, single-binary replacement for [SmokePing](https://oss.oetiker.ch/smokeping/).
Keeps the classic "smoke band" latency visualization (min–max + p5–p95 + median)
and adds a JSON API, a React + uPlot UI, InfluxDB v2 storage with tiered
rollups, MTR path discovery, and optional master/slave distributed probing.

> **Heads up — this project is AI-coded.**
> Every line of Go, TypeScript, CSS, and Flux in this repo was written by
> Claude under human direction. It's been shaped by iterative review, used
> in anger, and the tests are real — but there is no seasoned human
> maintainer standing behind each commit. Treat it like any other
> unpaid-volunteer hobby tool: read before deploying, pin versions, don't
> put it on the critical path without your own review.

## Screenshots

**24h latency, smoke band:**

![Smoke band — 24h gateway view](docs/screenshots/gateway-24h-band.png)

**24h latency, per-cycle bars (classic SmokePing style):**

![Per-cycle bars — 24h gateway view](docs/screenshots/gateway-24h-bars.png)

Below each chart the UI shows the latest MTR path (hops, per-hop loss, min/avg/max)
and a per-hop loss heatmap over the same window.

## Features

- **Probes:** ICMP (unprivileged ping sockets with raw fallback), TCP connect,
  HTTP(S) TTFB, DNS lookup, MTR-style path discovery.
- **Storage:** InfluxDB v2 with tiered rollups (raw samples 7d, 1h 180d,
  1d 2y) created automatically on startup via Flux tasks.
- **UI:** React + Vite + uPlot, embedded in the binary. Smoke band and
  classic-bars chart modes, MTR table, per-hop loss heatmap.
- **Alerting:** threshold conditions with sustained-cycles debounce.
  Actions: `log`, shell `exec`, generic `webhook`, and a first-class
  `discord` embed (includes MTR path when the probe is icmp/mtr).
- **Distributed probing:** run extra instances with `--slave` to probe
  from multiple vantage points; the master aggregates and persists.
- **Hot reload:** `SIGHUP`.

## Quick start

Prerequisites: Go 1.22+, Node 20+, a running InfluxDB v2 instance.

### 1. Start InfluxDB and grab credentials

The fastest path is Docker:

```bash
docker run -d --name influxdb -p 8086:8086 \
  -e DOCKER_INFLUXDB_INIT_MODE=setup \
  -e DOCKER_INFLUXDB_INIT_USERNAME=admin \
  -e DOCKER_INFLUXDB_INIT_PASSWORD=changeme123 \
  -e DOCKER_INFLUXDB_INIT_ORG=smokeping \
  -e DOCKER_INFLUXDB_INIT_BUCKET=smokeping_raw \
  -e DOCKER_INFLUXDB_INIT_ADMIN_TOKEN=supersecret-replace-me \
  influxdb:2
```

gosmokeping creates the `smokeping_1h` and `smokeping_1d` rollup buckets
and their Flux tasks automatically on first start — you only need to
provide a valid token and org.

### 2. Configure

```bash
cp config.example.json config.json
cp .env.example .env
```

Edit `.env` with your InfluxDB token/org (matching step 1):

```bash
INFLUX_URL=http://localhost:8086
INFLUX_ORG=smokeping
INFLUX_TOKEN=supersecret-replace-me
```

Edit `config.json` to list the hosts you actually want to probe. The
example ships with `1.1.1.1` and `8.8.8.8` so the first run is useful
out of the box.

### 3. Build

```bash
make ui     # vite build → internal/ui/dist/
make build  # go build → ./gosmokeping
```

Or build a container:

```bash
docker build -t gosmokeping .
```

### 4. Grant raw-socket capability

ICMP and MTR need raw sockets. Either run as root, or:

```bash
make setcap   # sudo setcap cap_net_raw+ep ./gosmokeping
```

The Docker image and the bundled systemd unit handle this for you.

### 5. Run

```bash
./gosmokeping -config config.json
```

Open <http://localhost:8080>. Cycles start landing within one probe
interval (30s by default).

## Config

See [`config.example.json`](config.example.json). Environment variables
of the form `${NAME}` are expanded at load time, so tokens live in env
vars. `SIGHUP` re-reads the file.

`.env` is auto-loaded from the directory holding `--config` first, then
from the current working directory (this matters under systemd, where
cwd is `/`). Real shell env always wins over `.env`; a missing `.env`
is a silent no-op.

### Alert actions

Every alert references one or more actions by name. The action types:

| Type      | Shape |
|-----------|-------|
| `log`     | Writes a structured log line. |
| `webhook` | Generic JSON POST; `template` overrides the default body. |
| `discord` | Rich embed with RTT, loss, sustained-cycle count, and MTR path. |
| `exec`    | Runs a shell command with the alert payload in env vars. |

```json
"actions": {
  "slack":   { "type": "webhook", "url": "https://hooks.slack.com/..." },
  "discord": { "type": "discord", "url": "${DISCORD_WEBHOOK_URL}" },
  "page":    { "type": "exec",    "command": "/usr/local/bin/pager" }
}
```

For icmp/mtr targets the Discord embed appends an MTR-style path block
so you can see where a cycle broke without opening the UI.

## HTTP API

| Method | Path                                                | Purpose |
|--------|-----------------------------------------------------|---------|
| GET    | `/api/v1/health`                                    | Health + uptime |
| GET    | `/api/v1/targets`                                   | List all targets |
| GET    | `/api/v1/targets/{group}/{name}/cycles?from&to&resolution` | Aggregates |
| GET    | `/api/v1/targets/{group}/{name}/rtts?from&to`       | Raw per-ping samples |
| GET    | `/api/v1/targets/{group}/{name}/status`             | Last 50 cycles |
| GET    | `/api/v1/targets/{group}/{name}/hops`               | Latest MTR path |
| GET    | `/api/v1/targets/{group}/{name}/hops/timeline?from&to` | Per-hop history |

`from` / `to` accept RFC3339, unix seconds, or durations like `-24h`.
`resolution` is `auto` (default), `raw`, `1h`, or `1d`. All endpoints
accept a `source=<name>` query parameter to filter by probe origin
when running in master/slave mode.

## Deployment

- **systemd:** run `sudo ./deploy/install.sh` after `make build`. The
  script creates a `gosmokeping` system user, installs the binary to
  `/usr/local/bin`, the unit to `/etc/systemd/system`, and stages
  `/etc/gosmokeping/` for your config + `.env`. The unit
  ([`deploy/gosmokeping.service`](deploy/gosmokeping.service)) grants
  `CAP_NET_RAW` via systemd so you don't need `setcap`. Re-run the
  script to update the binary or unit — it's idempotent.
- **Docker:** the image runs as an unprivileged user and grants
  `CAP_NET_RAW` to the binary via `setcap`. Mount your config and `.env`:

  ```bash
  docker run -d --name gosmokeping -p 8080:8080 \
    -v $(pwd)/config.json:/etc/gosmokeping/config.json:ro \
    -v $(pwd)/.env:/etc/gosmokeping/.env:ro \
    gosmokeping
  ```
- **Reverse proxy:** terminate TLS and authenticate at the proxy
  (Nginx/Caddy). The binary has no built-in auth.

## Master / slave

gosmokeping can run as a master that aggregates cycles from one or
more remote slaves so every target is probed from multiple vantage
points at once.

On the **master**, add a cluster block to `config.json`:

```json
"cluster": {
  "token": "${CLUSTER_TOKEN}",
  "source": "master"
}
```

By default every configured target is shipped to every registered
slave and the master probes locally too. To restrict a target to
specific slaves, set `target.slaves: ["berlin-1", "tokyo-1"]` — only
the named slaves (and not the master) will probe it.

On each **slave**, copy [`config.slave.example.json`](config.slave.example.json)
and set `master_url`, `token`, and a unique `name`. Then:

```bash
./gosmokeping --slave -config config.slave.json
```

The slave registers with the master, pulls the target list over HTTPS,
probes locally, and pushes cycle batches back every few seconds.
Buffered cycles survive short master outages (600-cycle ring,
drop-oldest). Slaves never touch InfluxDB or the UI — all storage and
alerting stays master-side. The UI shows a source-chip row (master +
every registered slave) so you can filter charts by origin.

## Development

```bash
make dev               # go run with debug logging
make ui-dev            # vite dev server on :5173 (proxies /api to :8080)
make test              # unit tests
make test-integration  # requires INFLUX_URL/INFLUX_TOKEN/INFLUX_ORG
make lint              # go vet
```

See [`CLAUDE.md`](CLAUDE.md) for the architecture notes an LLM (or a
human reviewer) needs to make sense of the pipeline — scheduler-as-hub,
config hot-reload contract, storage tiering, ICMP socket quirks, MTR
trace behavior, rollup task versioning, and the UI time-axis contract.

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
  storage/          # InfluxDB client (writer, reader, bootstrap)
  ui/               # embed.FS wrapper for the built SPA
ui/                 # React + Vite + uPlot SPA source
deploy/             # systemd unit
docs/screenshots/   # README screenshots
```

## Migrating from SmokePing

`smokeping2gosmokeping` reads a SmokePing `Config::Grammar` config and emits
an equivalent gosmokeping JSON config. It follows `@include` directives and
translates the common probe/alert shapes; constructs it can't map cleanly
(unusual probe types, complex alert patterns, `*** Presentation ***` settings)
are recorded in a sidecar notes file for human review.

```bash
smokeping2gosmokeping -in /etc/smokeping/config -out config.json
# writes config.json and config.json.notes.txt
```

Storage credentials are emitted as `${INFLUX_URL}` / `${INFLUX_TOKEN}` /
`${INFLUX_ORG}` placeholders — set them in the environment (or in a `.env`
file next to `config.json`) before starting gosmokeping. Add `-strict` to
make the tool exit 2 when any construct couldn't be fully translated, useful
for CI-driven config generation.

## License

MIT. Fork, break, improve.
