package clickhouse

import (
	"regexp"
	"strings"
	"testing"
)

// The ALTER list is what upgrades a pre-existing table; the CREATE covers only
// fresh databases, so an integration run against a temp DB can never miss a
// dropped ALTER — this test is the guard.
func TestAddColumnStatements(t *testing.T) {
	stmts := addColumnStatements("")
	want := []string{
		"ALTER TABLE probe_rtt ADD COLUMN IF NOT EXISTS target_group LowCardinality(String) AFTER target_id",
		"ALTER TABLE probe_hop ADD COLUMN IF NOT EXISTS target_group LowCardinality(String) AFTER target_id",
		"ALTER TABLE probe_http ADD COLUMN IF NOT EXISTS target_group LowCardinality(String) AFTER target_id",
		"ALTER TABLE probe_hop ADD COLUMN IF NOT EXISTS unreach LowCardinality(String) AFTER hop_addr",
		"ALTER TABLE probe_hop ADD COLUMN IF NOT EXISTS target_reply UInt8 CODEC(T64, ZSTD(1)) AFTER unreach",
	}
	if len(stmts) != len(want) {
		t.Fatalf("got %d statements, want %d:\n%s", len(stmts), len(want), strings.Join(stmts, "\n"))
	}
	for i, w := range want {
		if stmts[i] != w {
			t.Fatalf("statement %d:\ngot  %q\nwant %q", i, stmts[i], w)
		}
	}
}

// Each ALTER re-declares a type and codec the table DDL also defines, and an
// upgraded deployment whose ALTER drifted from the CREATE ends up with a
// different column definition than a fresh one — this pins the two sources of
// truth to each other, whitespace-normalized, per column.
func TestAddColumnStatementsMatchTableDDL(t *testing.T) {
	ddls := map[string]string{
		"probe_cycle": ddlProbeCycle,
		"probe_rtt":   ddlProbeRTT,
		"probe_hop":   ddlProbeHop,
		"probe_http":  ddlProbeHTTP,
	}
	stmt := regexp.MustCompile(`^ALTER TABLE (\S+) ADD COLUMN IF NOT EXISTS (\S+) (.+) AFTER \S+$`)
	for _, s := range addColumnStatements("") {
		m := stmt.FindStringSubmatch(s)
		if m == nil {
			t.Fatalf("statement has an unexpected shape: %q", s)
		}
		table, column, decl := m[1], m[2], strings.Join(strings.Fields(m[3]), " ")
		ddl, ok := ddls[table]
		if !ok {
			t.Fatalf("statement targets unknown table %q", table)
		}
		found := ""
		for _, line := range strings.Split(ddl, "\n") {
			fields := strings.Fields(strings.TrimSuffix(strings.TrimSpace(line), ","))
			if len(fields) > 1 && fields[0] == column {
				found = strings.Join(fields[1:], " ")
			}
		}
		if found == "" {
			t.Fatalf("%s DDL has no column %q for %q", table, column, s)
		}
		if found != decl {
			t.Fatalf("%s.%s drifted: ALTER declares %q, CREATE declares %q", table, column, decl, found)
		}
	}
}

func TestAddColumnStatementsOnCluster(t *testing.T) {
	for _, s := range addColumnStatements("ch1") {
		if !strings.Contains(s, "ON CLUSTER ch1") {
			t.Fatalf("cluster mode statement missing ON CLUSTER: %q", s)
		}
	}
}
