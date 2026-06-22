# Migration: RTT columns Float64 ms → UInt32 µs (BREAKING)

`probe_cycle` and `probe_hop` now store their latency columns as **UInt32
microseconds** (`*_us`) with `CODEC(T64, ZSTD)` instead of **Float64
milliseconds** (`*_ms`) with `CODEC(Gorilla, ZSTD)`. Gorilla XOR-coding fails on
noisy RTT floats (~1.04× compression); integer µs + T64 compresses ~4.9×. On a
real dataset this shrank the two tables from ~23 GiB to ~5 GiB.

The `storage.Reader` contract is unchanged — the reader divides µs→ms in SQL, so
the API and UI still see milliseconds. Only the on-disk schema changed.

`probe_rtt` and `probe_http` are **not** touched (already compress well and carry
a NaN "no-response" guard that doesn't map to an unsigned int).

## Why this needs a manual backfill

ClickHouse columns are single-typed: a column can't be Float64 for old rows and
UInt32 for new ones. A plain `ALTER … MODIFY COLUMN Float64 → UInt32` would
**truncate to integer milliseconds** (12.345 → 12) — lossy *and* the wrong scale
(no ×1000). So existing rows must be rebuilt with an explicit `×1000` cast.

The new binary's writer does positional batch inserts, so it **must not run
against the old-schema tables**. Migrate before starting the new build.

## Runbook (single-node MergeTree)

Stop writes first — the rebuild assumes no concurrent inserts.

```bash
systemctl stop gosmokeping        # or however you run it; do NOT start the new binary yet
```

Then, in `clickhouse-client` (set `USE gosmokeping` or qualify names):

```sql
-- ── probe_cycle ────────────────────────────────────────────────────────────
CREATE TABLE gosmokeping.probe_cycle_new
(
  timestamp      DateTime64(3, 'UTC')   CODEC(DoubleDelta, ZSTD(1)),
  target_id      LowCardinality(String),
  target_group   LowCardinality(String),
  source         LowCardinality(String),
  probe_type     LowCardinality(String),
  sent           UInt16   CODEC(T64, ZSTD(1)),
  lost           UInt16   CODEC(T64, ZSTD(1)),
  loss_pct       Float32  CODEC(Gorilla, ZSTD(1)),
  rtt_min_us     UInt32   CODEC(T64, ZSTD(1)),
  rtt_max_us     UInt32   CODEC(T64, ZSTD(1)),
  rtt_mean_us    UInt32   CODEC(T64, ZSTD(1)),
  rtt_median_us  UInt32   CODEC(T64, ZSTD(1)),
  rtt_stddev_us  UInt32   CODEC(T64, ZSTD(1)),
  p5_us  UInt32 CODEC(T64, ZSTD(1)), p10_us UInt32 CODEC(T64, ZSTD(1)),
  p15_us UInt32 CODEC(T64, ZSTD(1)), p20_us UInt32 CODEC(T64, ZSTD(1)),
  p25_us UInt32 CODEC(T64, ZSTD(1)), p30_us UInt32 CODEC(T64, ZSTD(1)),
  p35_us UInt32 CODEC(T64, ZSTD(1)), p40_us UInt32 CODEC(T64, ZSTD(1)),
  p45_us UInt32 CODEC(T64, ZSTD(1)), p55_us UInt32 CODEC(T64, ZSTD(1)),
  p60_us UInt32 CODEC(T64, ZSTD(1)), p65_us UInt32 CODEC(T64, ZSTD(1)),
  p70_us UInt32 CODEC(T64, ZSTD(1)), p75_us UInt32 CODEC(T64, ZSTD(1)),
  p80_us UInt32 CODEC(T64, ZSTD(1)), p85_us UInt32 CODEC(T64, ZSTD(1)),
  p90_us UInt32 CODEC(T64, ZSTD(1)), p95_us UInt32 CODEC(T64, ZSTD(1))
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(timestamp)
ORDER BY (target_id, source, timestamp)
TTL toDateTime(timestamp) + INTERVAL 365 DAY     -- bootstrap re-applies your configured retention on next start
SETTINGS index_granularity = 8192;

-- ms_col → least(round(ms*1000), MaxUint32) mirrors the writer's durUS() clamp.
INSERT INTO gosmokeping.probe_cycle_new
SELECT
  timestamp, target_id, target_group, source, probe_type, sent, lost, loss_pct,
  toUInt32(least(round(rtt_min_ms    * 1000), 4294967295)),
  toUInt32(least(round(rtt_max_ms    * 1000), 4294967295)),
  toUInt32(least(round(rtt_mean_ms   * 1000), 4294967295)),
  toUInt32(least(round(rtt_median_ms * 1000), 4294967295)),
  toUInt32(least(round(rtt_stddev_ms * 1000), 4294967295)),
  toUInt32(least(round(p5_ms *1000),4294967295)), toUInt32(least(round(p10_ms*1000),4294967295)),
  toUInt32(least(round(p15_ms*1000),4294967295)), toUInt32(least(round(p20_ms*1000),4294967295)),
  toUInt32(least(round(p25_ms*1000),4294967295)), toUInt32(least(round(p30_ms*1000),4294967295)),
  toUInt32(least(round(p35_ms*1000),4294967295)), toUInt32(least(round(p40_ms*1000),4294967295)),
  toUInt32(least(round(p45_ms*1000),4294967295)), toUInt32(least(round(p55_ms*1000),4294967295)),
  toUInt32(least(round(p60_ms*1000),4294967295)), toUInt32(least(round(p65_ms*1000),4294967295)),
  toUInt32(least(round(p70_ms*1000),4294967295)), toUInt32(least(round(p75_ms*1000),4294967295)),
  toUInt32(least(round(p80_ms*1000),4294967295)), toUInt32(least(round(p85_ms*1000),4294967295)),
  toUInt32(least(round(p90_ms*1000),4294967295)), toUInt32(least(round(p95_ms*1000),4294967295))
FROM gosmokeping.probe_cycle;

-- Verify counts match before swapping.
SELECT
  (SELECT count() FROM gosmokeping.probe_cycle)     AS old_rows,
  (SELECT count() FROM gosmokeping.probe_cycle_new) AS new_rows;

-- ── probe_hop ──────────────────────────────────────────────────────────────
CREATE TABLE gosmokeping.probe_hop_new
(
  timestamp     DateTime64(3, 'UTC')   CODEC(DoubleDelta, ZSTD(1)),
  target_id     LowCardinality(String),
  source        LowCardinality(String),
  ttl           UInt8,
  hop_addr      LowCardinality(String),
  sent          UInt16   CODEC(T64, ZSTD(1)),
  lost          UInt16   CODEC(T64, ZSTD(1)),
  loss_pct      Float32  CODEC(Gorilla, ZSTD(1)),
  rtt_min_us    UInt32   CODEC(T64, ZSTD(1)),
  rtt_max_us    UInt32   CODEC(T64, ZSTD(1)),
  rtt_mean_us   UInt32   CODEC(T64, ZSTD(1)),
  rtt_median_us UInt32   CODEC(T64, ZSTD(1))
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(timestamp)
ORDER BY (target_id, source, timestamp, ttl)
TTL toDateTime(timestamp) + INTERVAL 90 DAY
SETTINGS index_granularity = 8192;

INSERT INTO gosmokeping.probe_hop_new
SELECT
  timestamp, target_id, source, ttl, hop_addr, sent, lost, loss_pct,
  toUInt32(least(round(rtt_min_ms    * 1000), 4294967295)),
  toUInt32(least(round(rtt_max_ms    * 1000), 4294967295)),
  toUInt32(least(round(rtt_mean_ms   * 1000), 4294967295)),
  toUInt32(least(round(rtt_median_ms * 1000), 4294967295))
FROM gosmokeping.probe_hop;

SELECT
  (SELECT count() FROM gosmokeping.probe_hop)     AS old_rows,
  (SELECT count() FROM gosmokeping.probe_hop_new) AS new_rows;

-- ── Swap (atomic) once both counts match ─────────────────────────────────────
RENAME TABLE
  gosmokeping.probe_cycle TO gosmokeping.probe_cycle_old,
  gosmokeping.probe_cycle_new TO gosmokeping.probe_cycle,
  gosmokeping.probe_hop   TO gosmokeping.probe_hop_old,
  gosmokeping.probe_hop_new   TO gosmokeping.probe_hop;
```

Start the new binary, confirm the UI renders RTT correctly (values should look
identical — they're still ms), then reclaim space:

```sql
DROP TABLE gosmokeping.probe_cycle_old;
DROP TABLE gosmokeping.probe_hop_old;
```

## Notes

- **Disk:** the rebuild holds old + new copies simultaneously (~23 GiB extra for
  a dataset this size). Ensure free space before starting, or migrate one table
  at a time and drop the `_old` copy before doing the next.
- **`probe_hop` shortcut:** if you don't need historical hop data (it ages out in
  90 days anyway), skip its `CREATE/INSERT/RENAME` and just
  `TRUNCATE TABLE gosmokeping.probe_hop` then let the new binary's bootstrap
  recreate it — except bootstrap won't change an existing table's column types,
  so you must `DROP TABLE gosmokeping.probe_hop` (not truncate) and let bootstrap
  recreate it fresh in the new schema. Loses hop history; keeps cycle history.
- **CH cluster mode** (`storage.clickhouse.cluster` set → ReplicatedMergeTree):
  add `ON CLUSTER <name>` to every `CREATE`/`RENAME`/`DROP`, and use the
  `ReplicatedMergeTree('/clickhouse/tables/{shard}/<table>', '{replica}')` engine
  to match `injectCluster` in `schema.go`.
- **Rollback:** until you `DROP` the `_old` tables, rollback is a reverse
  `RENAME` + redeploying the previous binary.
