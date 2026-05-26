package clickhouse

import (
	"fmt"
	"strings"
)

// PerTableDDL returns the four CREATE TABLE IF NOT EXISTS statements with
// TTL values formatted in, one per slice entry. When cluster is non-empty,
// each statement is rewritten with ON CLUSTER <cluster> and the engine
// becomes ReplicatedMergeTree('/clickhouse/tables/{shard}/<table>', '{replica}').
func PerTableDDL(cluster string, cycleDays, rttDays, hopDays, httpDays int) []string {
	out := []string{
		fmt.Sprintf(ddlProbeCycle, cycleDays),
		fmt.Sprintf(ddlProbeRTT, rttDays),
		fmt.Sprintf(ddlProbeHop, hopDays),
		fmt.Sprintf(ddlProbeHTTP, httpDays),
	}
	if cluster == "" {
		return out
	}
	for i, ddl := range out {
		out[i] = injectCluster(ddl, cluster)
	}
	return out
}

// SchemaDDL is the test-friendly form: every table at default TTLs joined
// with semicolons. Production bootstrap uses PerTableDDL with configured
// retention values.
func SchemaDDL(cluster string) string {
	return strings.Join(PerTableDDL(cluster, 365, 14, 90, 14), ";\n") + ";\n"
}

func injectCluster(ddl, cluster string) string {
	for _, tbl := range []string{"probe_cycle", "probe_rtt", "probe_hop", "probe_http"} {
		needle := tbl + " ("
		if !strings.Contains(ddl, needle) {
			continue
		}
		ddl = strings.Replace(ddl, needle,
			fmt.Sprintf("%s ON CLUSTER %s (", tbl, cluster), 1)
		ddl = strings.Replace(ddl, "ENGINE = MergeTree",
			fmt.Sprintf("ENGINE = ReplicatedMergeTree('/clickhouse/tables/{shard}/%s', '{replica}')", tbl),
			1)
		return ddl
	}
	return ddl
}

const ddlProbeCycle = `CREATE TABLE IF NOT EXISTS probe_cycle (
  timestamp      DateTime64(3, 'UTC')                              CODEC(DoubleDelta, ZSTD(1)),
  target_id      LowCardinality(String),
  target_group   LowCardinality(String),
  source         LowCardinality(String),
  probe_type     LowCardinality(String),
  sent           UInt16                                            CODEC(T64, ZSTD(1)),
  lost           UInt16                                            CODEC(T64, ZSTD(1)),
  loss_pct       Float32                                           CODEC(Gorilla, ZSTD(1)),
  rtt_min_ms     Float64                                           CODEC(Gorilla, ZSTD(1)),
  rtt_max_ms     Float64                                           CODEC(Gorilla, ZSTD(1)),
  rtt_mean_ms    Float64                                           CODEC(Gorilla, ZSTD(1)),
  rtt_median_ms  Float64                                           CODEC(Gorilla, ZSTD(1)),
  rtt_stddev_ms  Float64                                           CODEC(Gorilla, ZSTD(1)),
  p5_ms          Float64                                           CODEC(Gorilla, ZSTD(1)),
  p10_ms         Float64                                           CODEC(Gorilla, ZSTD(1)),
  p15_ms         Float64                                           CODEC(Gorilla, ZSTD(1)),
  p20_ms         Float64                                           CODEC(Gorilla, ZSTD(1)),
  p25_ms         Float64                                           CODEC(Gorilla, ZSTD(1)),
  p30_ms         Float64                                           CODEC(Gorilla, ZSTD(1)),
  p35_ms         Float64                                           CODEC(Gorilla, ZSTD(1)),
  p40_ms         Float64                                           CODEC(Gorilla, ZSTD(1)),
  p45_ms         Float64                                           CODEC(Gorilla, ZSTD(1)),
  p55_ms         Float64                                           CODEC(Gorilla, ZSTD(1)),
  p60_ms         Float64                                           CODEC(Gorilla, ZSTD(1)),
  p65_ms         Float64                                           CODEC(Gorilla, ZSTD(1)),
  p70_ms         Float64                                           CODEC(Gorilla, ZSTD(1)),
  p75_ms         Float64                                           CODEC(Gorilla, ZSTD(1)),
  p80_ms         Float64                                           CODEC(Gorilla, ZSTD(1)),
  p85_ms         Float64                                           CODEC(Gorilla, ZSTD(1)),
  p90_ms         Float64                                           CODEC(Gorilla, ZSTD(1)),
  p95_ms         Float64                                           CODEC(Gorilla, ZSTD(1)),
  p99_ms         Float64                                           CODEC(Gorilla, ZSTD(1))
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(timestamp)
ORDER BY (target_id, source, timestamp)
TTL toDateTime(timestamp) + INTERVAL %d DAY
SETTINGS index_granularity = 8192`

const ddlProbeRTT = `CREATE TABLE IF NOT EXISTS probe_rtt (
  timestamp   DateTime64(3, 'UTC')   CODEC(DoubleDelta, ZSTD(1)),
  target_id   LowCardinality(String),
  source      LowCardinality(String),
  seq         UInt16,
  rtt_ms      Float64                CODEC(Gorilla, ZSTD(1))
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(timestamp)
ORDER BY (target_id, source, timestamp, seq)
TTL toDateTime(timestamp) + INTERVAL %d DAY`

const ddlProbeHop = `CREATE TABLE IF NOT EXISTS probe_hop (
  timestamp     DateTime64(3, 'UTC')   CODEC(DoubleDelta, ZSTD(1)),
  target_id     LowCardinality(String),
  source        LowCardinality(String),
  ttl           UInt8,
  hop_addr      LowCardinality(String),
  sent          UInt16                 CODEC(T64, ZSTD(1)),
  lost          UInt16                 CODEC(T64, ZSTD(1)),
  loss_pct      Float32                CODEC(Gorilla, ZSTD(1)),
  rtt_min_ms    Float64                CODEC(Gorilla, ZSTD(1)),
  rtt_max_ms    Float64                CODEC(Gorilla, ZSTD(1)),
  rtt_mean_ms   Float64                CODEC(Gorilla, ZSTD(1)),
  rtt_median_ms Float64                CODEC(Gorilla, ZSTD(1))
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(timestamp)
ORDER BY (target_id, source, timestamp, ttl)
TTL toDateTime(timestamp) + INTERVAL %d DAY`

const ddlProbeHTTP = `CREATE TABLE IF NOT EXISTS probe_http (
  timestamp   DateTime64(3, 'UTC')   CODEC(DoubleDelta, ZSTD(1)),
  target_id   LowCardinality(String),
  source      LowCardinality(String),
  seq         UInt16,
  rtt_ms      Float64                CODEC(Gorilla, ZSTD(1)),
  status      UInt16                 CODEC(T64, ZSTD(1)),
  error       String
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(timestamp)
ORDER BY (target_id, source, timestamp, seq)
TTL toDateTime(timestamp) + INTERVAL %d DAY`
