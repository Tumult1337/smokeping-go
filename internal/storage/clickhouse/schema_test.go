package clickhouse

import (
	"regexp"
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
	for _, p := range []string{"p5_us", "p10_us", "p25_us", "p75_us", "p90_us", "p95_us"} {
		if !strings.Contains(ddl, p) {
			t.Errorf("schema missing percentile column %q", p)
		}
	}
}

// Production RTT mantissas defeat Gorilla's XOR transform, so the detail
// tables use ZSTD directly. Both declarations must stay aligned because they
// store the same measurement shape.
func TestSchemaRTTCodecs(t *testing.T) {
	decl := regexp.MustCompile(`(?m)^\s*rtt_ms\s+Float64\s+CODEC\((.+)\)\s*,?$`)
	found := decl.FindAllStringSubmatch(SchemaDDL(""), -1)
	if len(found) != 2 {
		t.Fatalf("found %d rtt_ms declarations, want one per detail table", len(found))
	}
	for _, match := range found {
		if match[1] != "ZSTD(6)" {
			t.Errorf("rtt_ms codec = %q, want ZSTD(6)", match[1])
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

// Whitespace-insensitive: the DDL aligns its column types.
func TestSchemaHopAnnotationColumns(t *testing.T) {
	for _, col := range []string{
		`unreach\s+LowCardinality\(String\),`,
		`target_reply\s+UInt8`,
	} {
		if !regexp.MustCompile(`(?m)^\s+` + col).MatchString(ddlProbeHop) {
			t.Errorf("probe_hop DDL missing column matching %q", col)
		}
	}
}

// storage.MinQueryTime/MaxQueryTime are the DateTime64(3) domain. Widening the
// scale (DateTime64(6), say) or dropping to DateTime moves that domain, so the
// bound stops describing what the column can hold.
func TestSchemaTimestampColumnsPinTheQueryTimeDomain(t *testing.T) {
	decl := regexp.MustCompile(`(?m)^\s*timestamp\s+(\S+)`)
	found := decl.FindAllStringSubmatch(SchemaDDL(""), -1)
	if len(found) != 4 {
		t.Fatalf("found %d timestamp column declarations, want one per table", len(found))
	}
	for _, m := range found {
		if m[1] != "DateTime64(3," {
			t.Errorf("timestamp declared %q; storage.MaxQueryTime is derived from DateTime64(3)", m[1])
		}
	}
}
