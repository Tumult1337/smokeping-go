//go:build integration

package clickhouse

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/probe"
	"github.com/tumult/gosmokeping/internal/scheduler"
	"github.com/tumult/gosmokeping/internal/stats"
	"github.com/tumult/gosmokeping/internal/storage"
)

func testDSN(t *testing.T) (config.ClickHouse, func()) {
	t.Helper()
	addr := os.Getenv("CLICKHOUSE_ADDR")
	if addr == "" {
		t.Skip("CLICKHOUSE_ADDR not set")
	}
	buf := make([]byte, 4)
	_, _ = rand.Read(buf)
	db := "gosmokeping_test_" + hex.EncodeToString(buf)
	cfg := config.ClickHouse{
		Addr:     addr,
		Database: db,
		Username: os.Getenv("CLICKHOUSE_USERNAME"),
		Password: os.Getenv("CLICKHOUSE_PASSWORD"),
		Retention: config.ClickHouseRetention{
			CycleDays: 365, RTTDays: 14, HopDays: 90, HTTPDays: 14,
		},
		Batch: config.ClickHouseBatch{MaxRows: 1000, MaxInterval: "1s"},
	}
	if cfg.Username == "" {
		cfg.Username = "default"
	}
	cleanup := func() {
		opts := &clickhouse.Options{
			Addr: []string{cfg.Addr},
			Auth: clickhouse.Auth{Username: cfg.Username, Password: cfg.Password},
		}
		conn, err := clickhouse.Open(opts)
		if err != nil {
			t.Logf("cleanup open: %v", err)
			return
		}
		defer conn.Close()
		if err := conn.Exec(context.Background(), fmt.Sprintf("DROP DATABASE IF EXISTS %s", db)); err != nil {
			t.Logf("cleanup drop: %v", err)
		}
	}
	return cfg, cleanup
}

func TestBootstrapCreatesDatabaseAndTables(t *testing.T) {
	cfg, cleanup := testDSN(t)
	defer cleanup()

	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	if err := Bootstrap(ctx, log, cfg); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{cfg.Addr},
		Auth: clickhouse.Auth{Database: cfg.Database, Username: cfg.Username, Password: cfg.Password},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer conn.Close()

	for _, tbl := range []string{"probe_cycle", "probe_rtt", "probe_hop", "probe_http"} {
		var count uint64
		err := conn.QueryRow(ctx,
			"SELECT count() FROM system.tables WHERE database = ? AND name = ?",
			cfg.Database, tbl,
		).Scan(&count)
		if err != nil {
			t.Fatalf("query %s: %v", tbl, err)
		}
		if count != 1 {
			t.Errorf("table %s: expected 1 row, got %d", tbl, count)
		}
	}
}

func TestBootstrapIsIdempotent(t *testing.T) {
	cfg, cleanup := testDSN(t)
	defer cleanup()

	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	if err := Bootstrap(ctx, log, cfg); err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}
	if err := Bootstrap(ctx, log, cfg); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
}

func TestWriterCycleRoundTrip(t *testing.T) {
	cfg, cleanup := testDSN(t)
	defer cleanup()

	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := Bootstrap(ctx, log, cfg); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	w, err := NewWriter(ctx, log, cfg)
	if err != nil {
		t.Fatalf("new writer: %v", err)
	}
	defer w.Close()

	at := time.Now().UTC().Truncate(time.Millisecond)
	w.OnCycle(ctx, scheduler.Cycle{
		Time:      at,
		Target:    config.TargetRef{Target: config.Target{Name: "t1"}, Group: "g1"},
		ProbeName: "icmp",
		Source:    "master",
		Sent:      20,
		LossCount: 0,
		Summary: stats.Summary{
			Min: 1 * time.Millisecond, Max: 5 * time.Millisecond,
			Mean: 3 * time.Millisecond, Median: 3 * time.Millisecond,
			StdDev: 1 * time.Millisecond,
		},
	})

	// Wait for the 1s ticker to flush.
	time.Sleep(1500 * time.Millisecond)

	conn, _ := clickhouse.Open(&clickhouse.Options{
		Addr: []string{cfg.Addr},
		Auth: clickhouse.Auth{Database: cfg.Database, Username: cfg.Username, Password: cfg.Password},
	})
	defer conn.Close()

	var count uint64
	if err := conn.QueryRow(ctx,
		"SELECT count() FROM probe_cycle WHERE target_id = ?", "t1",
	).Scan(&count); err != nil {
		t.Fatalf("query: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 row, got %d", count)
	}

	_ = probe.Hop{} // silence unused import if probe is only referenced here
}

func TestWriterRTTHopHTTPRoundTrip(t *testing.T) {
	cfg, cleanup := testDSN(t)
	defer cleanup()

	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := Bootstrap(ctx, log, cfg); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	w, err := NewWriter(ctx, log, cfg)
	if err != nil {
		t.Fatalf("new writer: %v", err)
	}
	defer w.Close()

	at := time.Now().UTC().Truncate(time.Millisecond)
	w.OnCycle(ctx, scheduler.Cycle{
		Time:      at,
		Target:    config.TargetRef{Target: config.Target{Name: "t2"}, Group: "g"},
		ProbeName: "mtr",
		Source:    "master",
		Sent:      3,
		LossCount: 0,
		RTTs:      []time.Duration{1 * time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond},
		Hops: []probe.Hop{
			{Index: 1, IP: "10.0.0.1", Sent: 3, Lost: 0, RTTs: []time.Duration{500 * time.Microsecond}},
			{Index: 2, IP: "10.0.0.2", Sent: 3, Lost: 1, RTTs: []time.Duration{1500 * time.Microsecond}},
		},
		HTTPSamples: []probe.HTTPSample{
			{Time: at, RTT: 50 * time.Millisecond, Status: 200},
		},
	})

	time.Sleep(1500 * time.Millisecond)

	conn, _ := clickhouse.Open(&clickhouse.Options{
		Addr: []string{cfg.Addr},
		Auth: clickhouse.Auth{Database: cfg.Database, Username: cfg.Username, Password: cfg.Password},
	})
	defer conn.Close()

	cases := []struct {
		name  string
		query string
		want  uint64
	}{
		{"rtt", "SELECT count() FROM probe_rtt WHERE target_id = 't2'", 3},
		{"hop", "SELECT count() FROM probe_hop WHERE target_id = 't2'", 2},
		{"http", "SELECT count() FROM probe_http WHERE target_id = 't2'", 1},
	}
	for _, c := range cases {
		var got uint64
		if err := conn.QueryRow(ctx, c.query).Scan(&got); err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: got %d rows, want %d", c.name, got, c.want)
		}
	}
}

func TestReaderQueryCyclesRaw(t *testing.T) {
	cfg, cleanup := testDSN(t)
	defer cleanup()
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := Bootstrap(ctx, log, cfg); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	w, _ := NewWriter(ctx, log, cfg)
	at := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 3; i++ {
		w.OnCycle(ctx, scheduler.Cycle{
			Time:   at.Add(time.Duration(i) * time.Minute),
			Target: config.TargetRef{Target: config.Target{Name: "tc"}, Group: "g"},
			Source: "master",
			Sent:   20,
		})
	}
	w.Close()
	time.Sleep(500 * time.Millisecond)

	r, err := NewReader(ctx, cfg)
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	defer r.Close()

	pts, err := r.QueryCycles(ctx,
		config.TargetRef{Target: config.Target{Name: "tc"}, Group: "g"},
		at.Add(-time.Hour), at.Add(time.Hour),
		storage.QueryFilter{},
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pts) != 3 {
		t.Fatalf("expected 3 points, got %d", len(pts))
	}
}

func TestReaderQueryCyclesBucketed(t *testing.T) {
	cfg, cleanup := testDSN(t)
	defer cleanup()
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	_ = Bootstrap(ctx, log, cfg)
	w, _ := NewWriter(ctx, log, cfg)

	start := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	for i := 0; i < 120; i++ { // two hours worth at 1/min
		w.OnCycle(ctx, scheduler.Cycle{
			Time:   start.Add(time.Duration(i) * time.Minute),
			Target: config.TargetRef{Target: config.Target{Name: "tb"}, Group: "g"},
			Source: "master",
			Sent:   20,
		})
	}
	w.Close()
	time.Sleep(500 * time.Millisecond)

	r, _ := NewReader(ctx, cfg)
	defer r.Close()

	pts, err := r.QueryCycles(ctx,
		config.TargetRef{Target: config.Target{Name: "tb"}, Group: "g"},
		start, start.Add(2*time.Hour+time.Minute),
		storage.QueryFilter{Step: time.Hour},
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	// Two complete hours -> two buckets.
	if len(pts) < 2 {
		t.Fatalf("expected ≥ 2 buckets, got %d", len(pts))
	}
}
