# Running a gosmokeping master node with Docker Compose

This walks you through standing up a **master** (main) node in Docker
Compose, including the InfluxDB v2 backend it writes to. Slaves are a
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
                        │ writes
                        ▼
        ┌───────────────────────────────┐
        │   influxdb v2  (smokeping_*)  │
        └───────────────────────────────┘
```

Slaves connect inbound to `:8080` over HTTP(S) using a shared bearer
token. There is **no built-in auth on the UI** — put a reverse proxy in
front before exposing the master to anything you don't trust.

---

## 1. Layout

Create a directory on the host to hold persistent config and the
compose file:

```
/opt/smokeping/
├── docker-compose.yml
├── config.json
└── .env
```

Everything below is run from `/opt/smokeping/`.

---

## 2. `.env`

Used by Compose for variable substitution **and** auto-loaded by the
gosmokeping binary at startup (it reads `.env` from the directory
holding `--config`, then from cwd). Real shell env always wins, a
missing file is a silent no-op.

```dotenv
# --- InfluxDB ---------------------------------------------------------
INFLUX_URL=http://influxdb:8086
INFLUX_ORG=smokeping
INFLUX_TOKEN=replace-with-a-long-random-admin-token

# Used only by the influxdb container's first-boot setup. After the
# initial `docker compose up`, the volume holds state and these values
# are ignored.
INFLUXDB_INIT_USERNAME=admin
INFLUXDB_INIT_PASSWORD=replace-with-a-long-random-password
INFLUXDB_INIT_BUCKET=smokeping_raw

# --- Cluster ----------------------------------------------------------
# Shared bearer token for /api/v1/cluster/*. Slaves must send the same
# value. Generate with: openssl rand -hex 32
CLUSTER_TOKEN=replace-with-a-different-long-random-secret

# --- Optional alert webhooks -----------------------------------------
DISCORD_WEBHOOK_URL=
SLACK_WEBHOOK_URL=
```

> The `INFLUX_TOKEN` you set here is what the master will use to write
> to InfluxDB *and* what the InfluxDB container will provision as its
> admin token on first boot. Keep them identical.

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
    "backend": "influxv2",
    "influxv2": {
      "url":        "${INFLUX_URL}",
      "token":      "${INFLUX_TOKEN}",
      "org":        "${INFLUX_ORG}",
      "bucket_raw": "smokeping_raw",
      "bucket_1h":  "smokeping_1h",
      "bucket_1d":  "smokeping_1d"
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

The rollup buckets (`smokeping_1h`, `smokeping_1d`) and their Flux tasks
are created automatically by the master at startup; you only need
`smokeping_raw` to exist on the InfluxDB side, which the init env vars
below take care of.

---

## 4. `docker-compose.yml`

```yaml
services:
  influxdb:
    image: influxdb:2.7
    restart: unless-stopped
    volumes:
      - influxdb-data:/var/lib/influxdb2
      - influxdb-config:/etc/influxdb2
    environment:
      DOCKER_INFLUXDB_INIT_MODE: setup
      DOCKER_INFLUXDB_INIT_USERNAME: ${INFLUXDB_INIT_USERNAME}
      DOCKER_INFLUXDB_INIT_PASSWORD: ${INFLUXDB_INIT_PASSWORD}
      DOCKER_INFLUXDB_INIT_ORG:      ${INFLUX_ORG}
      DOCKER_INFLUXDB_INIT_BUCKET:   ${INFLUXDB_INIT_BUCKET}
      DOCKER_INFLUXDB_INIT_ADMIN_TOKEN: ${INFLUX_TOKEN}
    # Expose the InfluxDB UI on the host only if you want to poke at it
    # directly. Comment out otherwise — the master reaches it over the
    # Compose network on `influxdb:8086`.
    ports:
      - "127.0.0.1:8086:8086"
    healthcheck:
      test: ["CMD", "influx", "ping"]
      interval: 10s
      timeout: 3s
      retries: 12

  gosmokeping:
    build:
      context: ../..   # path back to the repo root from /opt/smokeping
      dockerfile: Dockerfile
    image: gosmokeping:latest
    restart: unless-stopped
    depends_on:
      influxdb:
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
  influxdb-data:
  influxdb-config:
```

If you'd rather pin a pre-built image instead of `build:`, replace the
`build:` block with `image: your-registry/gosmokeping:tag` after pushing
the image you built from this repo.

---

## 5. Bring it up

```bash
cd /opt/smokeping
docker compose build         # one-time: builds the gosmokeping image
docker compose up -d
docker compose logs -f gosmokeping
```

You should see the master:
1. Connect to InfluxDB and create the `smokeping_1h` / `smokeping_1d`
   buckets + rollup tasks (logged as `bucket created` / `task created`).
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
| Backup metrics  | Snapshot the `influxdb-data` volume (or `influx backup` from inside the influxdb container). |
| Inspect storage | `docker compose exec influxdb influx bucket list --org smokeping`    |

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

Slaves never touch InfluxDB; they buffer up to 600 cycles in memory if
the master goes away (drop-oldest on overflow) and re-register once it
comes back. A 401 from the master on any cluster endpoint will exit
the slave with a non-zero status — rotate the token on both sides to
recover.

---

## Troubleshooting

| Symptom                                                      | Likely cause |
|--------------------------------------------------------------|--------------|
| `bootstrap: 401 unauthorized` at master startup              | `INFLUX_TOKEN` doesn't match the value provisioned in the InfluxDB volume. Either reset it via the InfluxDB UI or wipe the `influxdb-data` volume to re-init. |
| ICMP probes all show 100% loss, logs say `operation not permitted` | Kernel stripped the binary's file caps. Add `cap_add: [NET_RAW]` to the `gosmokeping` service. |
| `bootstrap: bucket already exists, task missing`             | You changed the rollup task version (`-vN` suffix in `storage/bootstrap.go`) without listing the old name in `deleteObsoleteTasks`. Not a Docker issue — fix in code. |
| Slave logs `register: 401`                                   | `CLUSTER_TOKEN` mismatch between master and slave. They must be byte-identical. |
| UI loads but charts are empty for a wide window              | Server echoes the resolved `from`/`to` on `/cycles` responses; the chart pins its x-axis to those. If they're missing or zero, the resolution layer rejected the query — check master logs for `resolve window`. |
