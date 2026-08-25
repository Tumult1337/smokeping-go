package clickhouse

import (
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
		"ALTER TABLE probe_hop ADD COLUMN IF NOT EXISTS target_reply UInt8 AFTER unreach",
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

func TestAddColumnStatementsOnCluster(t *testing.T) {
	for _, s := range addColumnStatements("ch1") {
		if !strings.Contains(s, "ON CLUSTER ch1") {
			t.Fatalf("cluster mode statement missing ON CLUSTER: %q", s)
		}
	}
}
