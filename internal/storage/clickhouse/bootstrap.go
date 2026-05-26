package clickhouse

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"

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
	defer root.Close()

	if err := root.Exec(ctx, fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s", cfg.Database)); err != nil {
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
	defer conn.Close()

	for _, ddl := range PerTableDDL(cfg.Cluster,
		cfg.Retention.CycleDays, cfg.Retention.RTTDays,
		cfg.Retention.HopDays, cfg.Retention.HTTPDays,
	) {
		if err := conn.Exec(ctx, ddl); err != nil {
			return fmt.Errorf("create table: %w (ddl: %s)", err, ddl)
		}
	}

	// Apply TTLs even on re-bootstrap so config changes take effect.
	type ttl struct {
		table string
		days  int
	}
	for _, t := range []ttl{
		{"probe_cycle", cfg.Retention.CycleDays},
		{"probe_rtt", cfg.Retention.RTTDays},
		{"probe_hop", cfg.Retention.HopDays},
		{"probe_http", cfg.Retention.HTTPDays},
	} {
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

func tlsConfig(enabled bool) *tls.Config {
	if !enabled {
		return nil
	}
	return &tls.Config{MinVersion: tls.VersionTLS12}
}
