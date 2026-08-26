package clickhouse

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/tumult/gosmokeping/internal/config"
)

// Bootstrap creates the configured database and the four probe_* tables
// if they don't exist, then applies retention TTL via ALTER. Idempotent:
// safe to call on every process start.
func Bootstrap(ctx context.Context, log *slog.Logger, cfg config.ClickHouse) error {
	// Connect to the server (no Database in Auth — we may need to create it).
	root, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{cfg.Addr},
		Auth: clickhouse.Auth{Username: cfg.Username, Password: cfg.Password},
		TLS:  tlsConfig(cfg.TLS),
	})
	if err != nil {
		return fmt.Errorf("open clickhouse: %w", err)
	}
	defer root.Close() //nolint:errcheck // connection teardown; error not actionable

	createDB := fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s", cfg.Database)
	if cfg.Cluster != "" {
		// In cluster mode every replica needs the database before the
		// subsequent CREATE TABLE … ON CLUSTER fans out via the DDL queue,
		// otherwise replicas without the DB fail their leg of the DDL.
		createDB = fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s ON CLUSTER %s", cfg.Database, cfg.Cluster)
	}
	if err := root.Exec(ctx, createDB); err != nil {
		return fmt.Errorf("create database: %w", err)
	}
	log.Info("clickhouse.bootstrap", "database", cfg.Database, "cluster", cfg.Cluster)

	// Connect again with Database set, for table-level DDL.
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{cfg.Addr},
		Auth: clickhouse.Auth{Database: cfg.Database, Username: cfg.Username, Password: cfg.Password},
		TLS:  tlsConfig(cfg.TLS),
	})
	if err != nil {
		return fmt.Errorf("open clickhouse (db): %w", err)
	}
	defer conn.Close() //nolint:errcheck // connection teardown; error not actionable

	// Before any DDL: PerTableDDL embeds the same retention in CREATE TABLE,
	// so checking only the ALTER loop below left a fresh install creating its
	// tables with an unrepresentable TTL and aborting afterwards — and every
	// later start then failed at the guard without ever reaching the ALTER
	// that would repair them.
	for _, t := range retentionTTLs(cfg) {
		if err := ttlWithinDateTime(t.days); err != nil {
			return fmt.Errorf("retention %s: %w", t.table, err)
		}
	}

	for _, ddl := range PerTableDDL(cfg.Cluster,
		cfg.Retention.CycleDays, cfg.Retention.RTTDays,
		cfg.Retention.HopDays, cfg.Retention.HTTPDays,
	) {
		if err := conn.Exec(ctx, ddl); err != nil {
			return fmt.Errorf("create table: %w (ddl: %s)", err, ddl)
		}
	}

	for _, stmt := range addColumnStatements(cfg.Cluster) {
		if err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("add column: %w (stmt: %s)", err, stmt)
		}
	}

	// Apply TTLs even on re-bootstrap so config changes take effect.
	for _, t := range retentionTTLs(cfg) {
		stmt := fmt.Sprintf("ALTER TABLE %s MODIFY TTL toDateTime(timestamp) + INTERVAL %d DAY",
			t.table, t.days)
		if cfg.Cluster != "" {
			stmt = fmt.Sprintf("ALTER TABLE %s ON CLUSTER %s MODIFY TTL toDateTime(timestamp) + INTERVAL %d DAY",
				t.table, cfg.Cluster, t.days)
		}
		if err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("modify ttl %s: %w", t.table, err)
		}
	}

	return nil
}

// tableTTL pairs a table with the retention configured for it.
type tableTTL struct {
	table string
	days  int
}

func retentionTTLs(cfg config.ClickHouse) []tableTTL {
	return []tableTTL{
		{"probe_cycle", cfg.Retention.CycleDays},
		{"probe_rtt", cfg.Retention.RTTDays},
		{"probe_hop", cfg.Retention.HopDays},
		{"probe_http", cfg.Retention.HTTPDays},
	}
}

// maxDateTime is config's ceiling under this package's name, kept so the
// bootstrap tests read against the value the DDL is actually bounded by.
var maxDateTime = config.MaxDateTime

// ttlWithinDateTime refuses a retention whose expiry falls outside DateTime.
// config.Validate applies the same check, so this is defence in depth for a
// config that reached the process without it — a slave building from a
// master-supplied config, the same reason probe.Build re-checks the ping
// budget.
func ttlWithinDateTime(days int) error {
	return config.RetentionWithinDateTime(days, nowFn())
}

// nowFn is the injectable clock for ttlWithinDateTime.
var nowFn = time.Now

func tlsConfig(enabled bool) *tls.Config {
	if !enabled {
		return nil
	}
	return &tls.Config{MinVersion: tls.VersionTLS12}
}

// addColumnStatements upgrades tables that predate a column. CREATE TABLE IF
// NOT EXISTS never reconciles an existing table, and the flush inserts name
// their columns, so a missing column fails writes until this runs — it must
// stay ahead of NewWriter in openStorage. Metadata-only: no historical part is
// rewritten, and old rows read as "" / 0, which is why the reader requires a
// non-empty group rather than treating "" as a wildcard. Order matters:
// unreach must exist before target_reply's AFTER clause names it, and each
// type must carry the same codec its CREATE TABLE column does or an upgraded
// deployment ends up with a different column definition than a fresh one.
func addColumnStatements(cluster string) []string {
	cols := []struct{ table, column, typ, after string }{
		{"probe_rtt", "target_group", "LowCardinality(String)", "target_id"},
		{"probe_hop", "target_group", "LowCardinality(String)", "target_id"},
		{"probe_http", "target_group", "LowCardinality(String)", "target_id"},
		{"probe_hop", "unreach", "LowCardinality(String)", "hop_addr"},
		{"probe_hop", "target_reply", "UInt8 CODEC(T64, ZSTD(1))", "unreach"},
	}
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		on := ""
		if cluster != "" {
			on = " ON CLUSTER " + cluster
		}
		out = append(out, fmt.Sprintf("ALTER TABLE %s%s ADD COLUMN IF NOT EXISTS %s %s AFTER %s",
			c.table, on, c.column, c.typ, c.after))
	}
	return out
}
