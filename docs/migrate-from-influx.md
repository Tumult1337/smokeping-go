# Upgrading from the InfluxDB-era release

gosmokeping v1+ uses ClickHouse exclusively. InfluxDB v2/v3 support
was removed in PR #1 ("ClickHouse-only storage backend") and the old
code lives on the `legacy/influx` branch (last released as the
`v0-last-influx` tag).

There is **no in-place data migration**: the column-store schema is
incompatible with the bucket layout the Influx-era binary wrote, and
the rollup tasks don't translate. The upgrade is a clean cut, not a
data move.

This page walks through the orderly cutover. If you're standing up a
fresh deploy, skip to the [README quick start](../README.md#quick-start)
instead.

---

## What changes

| Concern | Influx era | ClickHouse era |
|---------|------------|----------------|
| Storage backend | InfluxDB v2 (default) or v3 (experimental) | ClickHouse (single supported backend) |
| Resolution tiers | Pre-aggregated buckets `smokeping_raw` / `_5m` / `_1h` / `_1d` written by Flux tasks | Single raw `probe_cycle` table; tiering happens at query time via `toStartOfInterval` |
| Retention | Per-bucket retention policy | Per-table TTL on the four `probe_*` tables, re-applied on every start |
| Auth | `INFLUX_TOKEN`, org, buckets in env vars | `CH_ADDR` / `CH_DATABASE` / `CH_USERNAME` / `CH_PASSWORD` in env vars |
| API query knob | `?resolution=auto\|raw\|5m\|1h\|1d` | `?step=raw\|1h\|1d` (back-compat override; auto-picked by default) |
| Config block | `storage.backend = "influxv2"` + `storage.influxv2 { url, token, org, bucket_* }` | `storage.clickhouse { addr, database, username, password, retention, batch, cluster }` |
| Cluster mode | n/a | `storage.clickhouse.cluster = "<name>"` → `ReplicatedMergeTree` on that CH cluster |
| Deploy docs | `docker-compose.influxv2.yml` / `docker-compose.influxv3.yml` (deleted) | `docker-compose.yml` (host-CH) + `docs/docker-master.md` (CH-in-stack) |

UI, scheduler, probes, alerts, master/slave protocol — unchanged.

---

## Decide whether you need the old data

Three honest options:

1. **Drop it.** SmokePing-style monitoring is most useful for the last
   week or two. If you don't actively query history beyond that, the
   simplest path is "stop the old stack, deploy fresh against CH,
   move on."
2. **Park it.** Keep the Influx instance running read-only on a side
   port and bookmark its UI / Grafana dashboards. Cheapest way to
   preserve access to history without translating data.
3. **Export it.** If you need to keep specific data extractable, dump
   it from Influx with the CLI before tearing the old stack down (see
   below). You won't be able to import it back into gosmokeping, but
   you'll have CSV/line-protocol files you can query with any tool.

Most people end up at option 1 or 2.

---

## Suggested cutover

### 1. Snapshot the old binary

```bash
git -C /opt/smokeping switch --detach v0-last-influx
git -C /opt/smokeping log -1 --oneline
```

Confirms you're on the last Influx-supporting commit. Don't delete the
working tree yet — you'll need it if you want to dump data.

### 2. (Optional) Export the historical data you care about

The Influx-era schema uses buckets named `smokeping_raw`, `smokeping_5m`,
`smokeping_1h`, `smokeping_1d`. From inside the old InfluxDB container:

```bash
docker compose exec influxdb influx query \
  --org smokeping \
  'from(bucket: "smokeping_raw")
     |> range(start: -30d)
     |> filter(fn: (r) => r._measurement == "probe_cycle")' \
  --raw > smokeping_raw_30d.csv
```

Repeat per bucket / range as needed. There's no automated import path
into ClickHouse — these files are for archive only.

### 3. Stand up ClickHouse

If you're running gosmokeping in the host-CH compose layout (`docker-compose.yml`
in the repo root), install CH on the host directly (apt/dnf packages) and
make sure it listens on `127.0.0.1:9000`. If you're using the in-stack
layout (`docs/docker-master.md`), CH comes up as part of the compose
stack — nothing extra needed.

Either way, create the database user gosmokeping will authenticate as:

```sql
CREATE USER gosmokeping IDENTIFIED BY '<CH_PASSWORD>';
GRANT CREATE DATABASE ON *.* TO gosmokeping;
GRANT CREATE TABLE, ALTER, SELECT, INSERT ON gosmokeping.* TO gosmokeping;
```

The `CREATE DATABASE` grant is only needed for the first start
(bootstrap auto-creates the database and the four `probe_*` tables);
you can revoke it afterwards if you want least-privilege.

You can also point gosmokeping at the existing `default` superuser
with its password if you set one — `default` keeps full grants on a
stock CH install. Verify with `clickhouse-client --user default
--password '<pw>' --query "CREATE DATABASE IF NOT EXISTS _test; DROP
DATABASE _test;"`.

### 4. Switch to the new code and update config

```bash
git -C /opt/smokeping switch main
git -C /opt/smokeping pull
```

Replace your `.env` with the new variable names:

```dotenv
# OLD — delete these
# INFLUX_URL=http://localhost:8086
# INFLUX_ORG=smokeping
# INFLUX_TOKEN=...

# NEW
CH_ADDR=127.0.0.1:9000
CH_DATABASE=gosmokeping
CH_USERNAME=default
CH_PASSWORD=<the password you set>
```

Replace the `storage` block in `config.json`:

```json
// OLD
"storage": {
  "backend": "influxv2",
  "influxv2": {
    "url":        "${INFLUX_URL}",
    "token":      "${INFLUX_TOKEN}",
    "org":        "${INFLUX_ORG}",
    "bucket_raw": "smokeping_raw",
    "bucket_5m":  "smokeping_5m",
    "bucket_1h":  "smokeping_1h",
    "bucket_1d":  "smokeping_1d"
  }
}

// NEW
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
}
```

`cluster` stays `""` unless you actually run a Keeper-backed
multi-replica ClickHouse cluster and want `ReplicatedMergeTree`.

Everything else in `config.json` (targets, probes, alerts, actions,
the `cluster` block for master/slave) is unchanged.

### 5. Start the new binary

For the host-CH compose layout:

```bash
cd /opt/smokeping
make deploy
docker compose logs -f gosmokeping
```

On the first start you should see:

```
clickhouse.bootstrap database=gosmokeping cluster=
http server listening addr=:8080
```

…followed by the first probe cycle within `interval` seconds. Open
`http://<host>:8080/` to confirm targets render. Bootstrap is
idempotent — subsequent restarts re-apply `ALTER TABLE … MODIFY TTL`
on the four `probe_*` tables and are a no-op otherwise.

### 6. Decommission the old stack

Once the new binary has been running long enough that you're confident
in it (a few hours of cycles, an alert event or two), retire the old
stack:

```bash
docker compose -f docker-compose.influxv2.yml down  # or influxv3
# don't `down -v` unless you're certain you don't want the data later
```

Keep the `influxdb-data` volume around for a few weeks. It's small
relative to anything else on the box and is the only copy of your
history.

---

## Reading the legacy archive

The `legacy/influx` branch is read-only history — bug fixes won't land
there. If you need to run it again:

```bash
git switch legacy/influx
# or, for a specific release (detached HEAD; tags aren't branches):
git switch --detach v0-last-influx
```

The old `docker-compose.influxv{2,3}.yml` files live on that branch
along with their docs.

---

## Why no automated migration

Two reasons:

1. **Schema shape is fundamentally different.** Influx writes
   per-cycle and per-rollup buckets; ClickHouse writes raw rows into
   four tables and computes tiers at query time. There's no rewrite
   that doesn't lose information either way.
2. **Most operators don't actually need the old data.** SmokePing-
   style monitoring is forward-looking; the cost of writing and
   testing a one-time data mover would land on every operator while
   only the rare power-user cares about preserving >30d of past
   cycles. The "stand up fresh + park the old binary" path is faster
   for almost everyone.

If you have a genuine multi-year archive you need queryable in the new
UI, open an issue with your retention requirements — it's a one-off
script rather than an upstream feature.
