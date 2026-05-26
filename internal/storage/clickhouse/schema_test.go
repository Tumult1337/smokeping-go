package clickhouse

import (
	"strings"
	"testing"
)

func TestSchemaContainsAllTables(t *testing.T) {
	for _, want := range []string{
		"CREATE TABLE IF NOT EXISTS probe_cycle",
		"CREATE TABLE IF NOT EXISTS probe_rtt",
		"CREATE TABLE IF NOT EXISTS probe_hop",
		"CREATE TABLE IF NOT EXISTS probe_http",
	} {
		if !strings.Contains(SchemaDDL(""), want) {
			t.Errorf("schema missing %q", want)
		}
	}
}

func TestSchemaPercentileColumns(t *testing.T) {
	ddl := SchemaDDL("")
	for _, p := range []string{"p5_ms", "p10_ms", "p25_ms", "p75_ms", "p90_ms", "p95_ms", "p99_ms"} {
		if !strings.Contains(ddl, p) {
			t.Errorf("schema missing percentile column %q", p)
		}
	}
}

func TestSchemaOnClusterRewrite(t *testing.T) {
	ddl := SchemaDDL("ch_cluster_a")
	if !strings.Contains(ddl, "ON CLUSTER ch_cluster_a") {
		t.Error("cluster mode: missing ON CLUSTER clause")
	}
	if !strings.Contains(ddl, "ReplicatedMergeTree") {
		t.Error("cluster mode: engine not rewritten to ReplicatedMergeTree")
	}
}
