# Running a gosmokeping master node with Docker Compose

This walks you through standing up a **master** (main) node in Docker
Compose, including the ClickHouse backend it writes to. Slaves are a
separate concern — see the *Adding slaves* section at the bottom.

The repo ships a `Dockerfile` (multi-stage: Vite UI build → Go build →
Alpine runtime) but no compose file. Everything below assumes you build
the image from this repo.

---

## What you end up with

```
                 :8080 (UI + API + /api/v1/cluster/*)
                        ▲
                        │
        ┌───────────────┴───────────────┐
        │   gosmokeping-master          │
        │   (probes targets locally,    │
        │    writes cycles, evaluates   │
        │    alerts, serves UI)         │
        └───────────────┬───────────────┘
                        │ writes (native :9000)
                        ▼
        ┌───────────────────────────────┐
        │   clickhouse (probe_* tables) │
        └───────────────────────────────┘
```

Slaves connect inbound to `:8080` over HTTP(S) using a shared bearer
token. There is **no built-in auth on the UI** — put a reverse proxy in
front before exposing the master to anything you don't trust.

---

## 1. Layout

Pick one of two host layouts. The rest of this guide assumes **Pattern A**;
where it matters, Pattern B is called out inline.

**Pattern A — repo lives at `/opt/smokeping/` (simplest).** Clone the
repo straight into the deploy directory and put `docker-compose.yml`,
`config.json`, and `.env` next to the source. The compose `build.context`
is just `.`.

```
/opt/smokeping/                  # the repo, with extras alongside
├── Dockerfile                   # from the repo
├── cmd/ internal/ ui/ ...       # from the repo
├── docker-compose.yml           # you create
├── config.json                  # you create (from config.example.json)
└── .env                         # you create (from .env.example)
```

```bash
sudo git clone https://github.com/<owner>/gosmokeping /opt/smokeping
cd /opt/smokeping
```

**Pattern B — runtime dir separate from the source checkout.** Use this
if you want to keep the deploy directory minimal and the repo elsewhere
(e.g. `/srv/src/gosmokeping`). The compose file's `build.context` then
needs to point at the repo by absolute path:

```
/opt/smokeping/                  # runtime only
├── docker-compose.yml
├── config.json
└── .env

/srv/src/gosmokeping/            # the repo, anywhere you like
└── Dockerfile, cmd/, ...
```

```yaml
    build:
      context: /srv/src/gosmokeping
      dockerfile: Dockerfile
```

Everything below is run from `/opt/smokeping/`.

> **Skip the build entirely.** The repo's GitHub Actions workflow
> (`.github/workflows/build.yml`) builds and pushes a multi-tagged image
> to `ghcr.io/<owner>/gosmokeping` on every push to `main` and on tagged
> releases (`latest`, `main`, `<tag>`, `sha-<short>`). To use it, delete
> the `build:` block in section 4 and replace it with:
>
> ```yaml
>     image: ghcr.io/<owner>/gosmokeping:latest
> ```
>
> No local `Dockerfile` or repo checkout needed in that case — only the
> compose file, `config.json`, and `.env`.

---

## 2. `.env`

Used by Compose for variable substitution **and** auto-loaded by the
gosmokeping binary at startup (it reads `.env` from the directory
holding `--config`, then from cwd). Real shell env always wins, a
missing file is a silent no-op.

```dotenv
# --- ClickHouse -------------------------------------------------------
# The hostname `clickhouse` resolves to the service in the compose
# network. 9000 is the native protocol port.
CH_ADDR=clickhouse:9000
CH_DATABASE=gosmokeping
CH_USERNAME=gosmokeping
CH_PASSWORD=replace-with-a-long-random-password

# Used only by the clickhouse container's first-boot setup. After the
# initial `docker compose up`, the volume holds state and these values
# are ignored.
CLICKHOUSE_INIT_DB=gosmokeping
CLICKHOUSE_INIT_USER=gosmokeping
CLICKHOUSE_INIT_PASSWORD=replace-with-a-long-random-password

# --- Cluster ----------------------------------------------------------
# Shared bearer token for /api/v1/cluster/*. Slaves must send the same
# value. Generate with: openssl rand -hex 32
CLUSTER_TOKEN=replace-with-a-different-long-random-secret

# --- Optional alert webhooks -----------------------------------------
DISCORD_WEBHOOK_URL=
SLACK_WEBHOOK_URL=
```

> Keep `CH_PASSWORD` and `CLICKHOUSE_INIT_PASSWORD` identical — the
> first is what the master authenticates with; the second is what the
> ClickHouse container provisions on first boot. Same for the user and
> database fields.

---

## 3. `config.json`

Start from `config.example.json` in the repo root and add the
**`cluster`** block — that's what flips the master into cluster mode.
Everything else (targets, probes, alerts) is identical to standalone.

```json
{
  "listen": ":8080",
  "interval": "30s",
  "pings": 10,

  "cluster": {
    "token": "${CLUSTER_TOKEN}",
    "source": "master"
  },

  "storage": {
    "clickhouse": {
      "addr":     "${CH_ADDR}",
      "database": "${CH_DATABASE}",
      "username": "${CH_USERNAME}",
      "password": "${CH_PASSWORD}",
      "tls":      false,
      "cluster":  "",
      "retention": {
        "cycle_days": 365,
        "rtt_days":   14,
        "hop_days":   90,
        "http_days":  14
      },
      "batch": {
        "max_rows":     1000,
        "max_interval": "1s"
      }
    }
  },

  "probes": {
    "icmp": { "type": "icmp", "timeout": "5s" },
    "http": { "type": "http", "timeout": "10s" },
    "tcp":  { "type": "tcp",  "timeout": "5s" },
    "dns":  { "type": "dns",  "timeout": "5s" },
    "mtr":  { "type": "mtr",  "timeout": "2s" }
  },

  "targets": [
    {
      "group": "external",
      "title": "External",
      "targets": [
        { "name": "cloudflare", "host": "1.1.1.1", "probe": "icmp",
          "alerts": ["high-loss", "high-latency"] },
        { "name": "google-dns", "host": "8.8.8.8", "probe": "icmp" },
        { "name": "cloudflare-path", "host": "1.1.1.1", "probe": "mtr" }
      ]
    }
  ],

  "alerts": {
    "high-loss":    { "condition": "loss_pct > 5",      "sustained": 3, "actions": ["log"] },
    "high-latency": { "condition": "rtt_median > 50ms", "sustained": 5, "actions": ["log"] }
  },

  "actions": {
    "log": { "type": "log" }
  }
}
```

Cluster-block notes:

| Field    | Purpose                                                                 |
|----------|-------------------------------------------------------------------------|
| `token`  | Required. Shared bearer for `/api/v1/cluster/{register,config,cycles}`. |
| `source` | Optional, defaults to `"master"`. Stamped on locally-probed cycles so the UI can filter by origin. |

To restrict a target to specific slaves only, add `"slaves":
["frankfurt-1", "tokyo-1"]` on that target — named slaves probe it,
the master skips it locally, and other slaves never see it. Omit the
field to let everyone (master + every registered slave) probe it.

Storage bootstrap runs on every start: the master issues `CREATE
DATABASE IF NOT EXISTS` and `CREATE TABLE IF NOT EXISTS` for the four
`probe_*` tables (`probe_cycle`, `probe_rtt`, `probe_hop`, `probe_http`)
and re-applies the retention TTLs via `ALTER TABLE … MODIFY TTL`, so a
retention change takes effect on the next process restart. Replicated
mode (set `storage.clickhouse.cluster` to a real `<remote_servers>`
cluster name) requires Keeper/ZooKeeper and `{shard}`/`{replica}`
macros on every replica.

---

## 4. `docker-compose.yml`

```yaml
services:
  clickhouse:
    image: clickhouse/clickhouse-server:24.3
    restart: unless-stopped
    ulimits:
      nofile:
        soft: 262144
        hard: 262144
    volumes:
      - clickhouse-data:/var/lib/clickhouse
      - clickhouse-logs:/var/log/clickhouse-server
    environment:
      CLICKHOUSE_DB:                    ${CLICKHOUSE_INIT_DB}
      CLICKHOUSE_USER:                  ${CLICKHOUSE_INIT_USER}
      CLICKHOUSE_PASSWORD:              ${CLICKHOUSE_INIT_PASSWORD}
      CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT: 1
    # Expose ClickHouse on the host only if you want to query it
    # directly. Comment out otherwise — the master reaches it over the
    # Compose network on `clickhouse:9000`.
    ports:
      - "127.0.0.1:9000:9000"   # native protocol
      - "127.0.0.1:8123:8123"   # HTTP / play UI
    healthcheck:
      test: ["CMD", "wget", "--no-verbose", "--tries=1", "--spider", "http://localhost:8123/ping"]
      interval: 10s
      timeout: 3s
      retries: 12

  gosmokeping:
    # Pattern A (repo at /opt/smokeping): context: .
    # Pattern B (repo elsewhere):         context: /absolute/path/to/repo
    # Or skip building and use:           image: ghcr.io/<owner>/gosmokeping:latest
    build:
      context: .
      dockerfile: Dockerfile
    image: gosmokeping:latest
    restart: unless-stopped
    depends_on:
      clickhouse:
        condition: service_healthy
    ports:
      - "8080:8080"
    volumes:
      - ./config.json:/opt/smokeping/config.json:ro
      - ./.env:/opt/smokeping/.env:ro
    # The image already has cap_net_raw+ep set on the binary via setcap.
    # Some kernels strip file caps from bind-mounted layers; if ICMP
    # probes start failing with "operation not permitted", uncomment:
    # cap_add:
    #   - NET_RAW

volumes:
  clickhouse-data:
  clickhouse-logs:
```

If you'd rather pin the prebuilt image published by this repo's CI,
replace the entire `build:` block with:

```yaml
    image: ghcr.io/<owner>/gosmokeping:latest
```

…and `docker compose pull` instead of `docker compose build`. The
`build.yml` workflow tags every `main` push as `latest` plus `sha-…`,
and every `vX.Y.Z` git tag as that tag.

---

## 5. Bring it up

```bash
cd /opt/smokeping
docker compose build         # building locally; skip if using ghcr.io/...
# docker compose pull        # use this instead if you swapped build: → image: ghcr.io/...
docker compose up -d
docker compose logs -f gosmokeping
```

You should see the master:
1. Connect to ClickHouse, create the `gosmokeping` database and the
   four `probe_*` tables (logged as `clickhouse.bootstrap database=…`),
   and re-apply TTLs.
2. Start the scheduler and probe its first cycle within `interval`
   seconds.
3. Open the cluster ingest endpoints on `:8080`.

Verify:

```bash
curl http://localhost:8080/api/v1/health
curl http://localhost:8080/api/v1/targets | jq .
```

Then open `http://<host>:8080/` in a browser.

---

## 6. Day-2 operations

| What            | How                                                                  |
|-----------------|----------------------------------------------------------------------|
| Reload config   | Edit `config.json`, then `docker compose kill -s HUP gosmokeping`.   |
| Tail logs       | `docker compose logs -f gosmokeping`                                 |
| Update binary   | `git pull && docker compose build && docker compose up -d gosmokeping` |
| Backup metrics  | Snapshot the `clickhouse-data` volume, or use `clickhouse-backup` against a running container. |
| Inspect storage | `docker compose exec clickhouse clickhouse-client --query "SELECT count() FROM gosmokeping.probe_cycle"` |
| Change retention | Edit `storage.clickhouse.retention.*_days` in `config.json` and restart the master — `ALTER … MODIFY TTL` runs on every start. |

Hot-reload caveat: the alert evaluator's per-target state lives in
memory only. A full restart of the master returns every alert to OK
until the next `sustained` cycles' worth of evidence rebuilds the
state. `SIGHUP` keeps that state intact — prefer it over `restart`
when you're only changing config.

---

## 7. Reverse proxy (recommended)

The binary has zero auth on the UI or read API. The `/api/v1/cluster/*`
endpoints are bearer-token gated, but **everything else is open**.
Don't expose `:8080` to the public internet without a proxy.

Sketch with Caddy in front, terminating TLS and basic-auth-ing the UI
while passing the cluster paths through unchanged so slaves can hit
them with their bearer token:

```caddy
smokeping.example.com {
    @cluster path /api/v1/cluster/*
    handle @cluster {
        reverse_proxy gosmokeping:8080
    }
    handle {
        basic_auth {
            you $2a$14$...bcrypt-hash...
        }
        reverse_proxy gosmokeping:8080
    }
}
```

---

## 8. Adding slaves

On each remote host (these run **outside** this Compose stack — they're
separate boxes in different networks/regions):

```json
{
  "cluster": {
    "master_url": "https://smokeping.example.com",
    "token":      "${CLUSTER_TOKEN}",
    "name":       "frankfurt-1",
    "push_every": "5s",
    "pull_every": "60s"
  }
}
```

Run with:

```bash
gosmokeping --slave -config config.slave.json
```

…or, for Docker, the same image with a different command:

```yaml
services:
  gosmokeping-slave:
    image: gosmokeping:latest
    restart: unless-stopped
    command: ["--slave", "-config", "/opt/smokeping/config.json"]
    volumes:
      - ./config.slave.json:/opt/smokeping/config.json:ro
      - ./.env:/opt/smokeping/.env:ro
```

The slave name (`frankfurt-1` here) shows up as a chip in the master's
UI. Use it in `target.slaves: ["frankfurt-1"]` on the master if you
want a target probed only from that location.

Slaves never touch ClickHouse; they buffer up to 600 cycles in memory
if the master goes away (drop-oldest on overflow) and re-register once
it comes back. A 401 from the master on any cluster endpoint will exit
the slave with a non-zero status — rotate the token on both sides to
recover.

---

## Troubleshooting

| Symptom                                                      | Likely cause |
|--------------------------------------------------------------|--------------|
| `bootstrap: create database: code 516, authentication failed` | `CH_USERNAME`/`CH_PASSWORD` don't match what ClickHouse provisioned. Either reset them inside the container with `clickhouse-client` or wipe the `clickhouse-data` volume to re-init. |
| `bootstrap: create database: code 139, There is no Zookeeper configuration in server config` | You set `storage.clickhouse.cluster` but the container has no Keeper/ZooKeeper. Either run with `cluster: ""` (single-node MergeTree) or add `<keeper_server>` + `<remote_servers>` config under `/etc/clickhouse-server/config.d/`. |
| ICMP probes all show 100% loss, logs say `operation not permitted` | Kernel stripped the binary's file caps. Add `cap_add: [NET_RAW]` to the `gosmokeping` service. |
| Slave logs `register: 401`                                   | `CLUSTER_TOKEN` mismatch between master and slave. They must be byte-identical. |
| UI loads but charts are empty for a wide window              | Server echoes the resolved `from`/`to` on `/cycles` responses; the chart pins its x-axis to those. If they're missing or zero, the tier-ladder picker rejected the window — check master logs for `query window` / `pick step`. |
