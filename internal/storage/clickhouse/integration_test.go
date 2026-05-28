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
		drop := fmt.Sprintf("DROP DATABASE IF EXISTS %s", db)
		if cfg.Cluster != "" {
			drop = fmt.Sprintf("DROP DATABASE IF EXISTS %s ON CLUSTER %s SYNC", db, cfg.Cluster)
		}
		if err := conn.Exec(context.Background(), drop); err != nil {
			t.Logf("cleanup drop: %v", err)
		}
	}
	return cfg, cleanup
}

// testDSNCluster mirrors testDSN but pins cfg.Cluster from the
// CLICKHOUSE_CLUSTER env var, skipping when it's unset. Lets a single
// invocation of the integration suite cover both single-node MergeTree
// and ReplicatedMergeTree code paths without duplicating the schema.
func testDSNCluster(t *testing.T) (config.ClickHouse, func()) {
	t.Helper()
	cluster := os.Getenv("CLICKHOUSE_CLUSTER")
	if cluster == "" {
		t.Skip("CLICKHOUSE_CLUSTER not set")
	}
	cfg, cleanup := testDSN(t)
	cfg.Cluster = cluster
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

func TestReaderQueryRTTs(t *testing.T) {
	cfg, cleanup := testDSN(t)
	defer cleanup()
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	_ = Bootstrap(ctx, log, cfg)
	w, _ := NewWriter(ctx, log, cfg)
	at := time.Now().UTC().Truncate(time.Second)
	w.OnCycle(ctx, scheduler.Cycle{
		Time:   at,
		Target: config.TargetRef{Target: config.Target{Name: "tr"}, Group: "g"},
		Source: "master",
		Sent:   3,
		RTTs:   []time.Duration{1 * time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond},
	})
	w.Close()
	time.Sleep(500 * time.Millisecond)

	r, _ := NewReader(ctx, cfg)
	defer r.Close()
	pts, err := r.QueryRTTs(ctx,
		config.TargetRef{Target: config.Target{Name: "tr"}, Group: "g"},
		at.Add(-time.Hour), at.Add(time.Hour),
		storage.QueryFilter{},
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pts) != 3 {
		t.Fatalf("expected 3 samples, got %d", len(pts))
	}
}

func TestReaderQueryHTTPSamples(t *testing.T) {
	cfg, cleanup := testDSN(t)
	defer cleanup()
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	_ = Bootstrap(ctx, log, cfg)
	w, _ := NewWriter(ctx, log, cfg)
	at := time.Now().UTC().Truncate(time.Second)
	w.OnCycle(ctx, scheduler.Cycle{
		Time:        at,
		Target:      config.TargetRef{Target: config.Target{Name: "th"}, Group: "g"},
		Source:      "master",
		Sent:        1,
		HTTPSamples: []probe.HTTPSample{{Time: at, RTT: 100 * time.Millisecond, Status: 200}},
	})
	w.Close()
	time.Sleep(500 * time.Millisecond)

	r, _ := NewReader(ctx, cfg)
	defer r.Close()
	pts, err := r.QueryHTTPSamples(ctx,
		config.TargetRef{Target: config.Target{Name: "th"}, Group: "g"},
		at.Add(-time.Hour), at.Add(time.Hour),
		storage.QueryFilter{},
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pts) != 1 || pts[0].Status != 200 {
		t.Fatalf("unexpected: %+v", pts)
	}
}

func TestReaderQueryLatestHops(t *testing.T) {
	cfg, cleanup := testDSN(t)
	defer cleanup()
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	_ = Bootstrap(ctx, log, cfg)
	w, _ := NewWriter(ctx, log, cfg)
	at := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 3; i++ {
		w.OnCycle(ctx, scheduler.Cycle{
			Time:   at.Add(time.Duration(i) * time.Minute),
			Target: config.TargetRef{Target: config.Target{Name: "tlh"}, Group: "g"},
			Source: "master",
			Sent:   3,
			Hops: []probe.Hop{
				{Index: 1, IP: "10.0.0.1", Sent: 3, Lost: 0, RTTs: []time.Duration{1 * time.Millisecond}},
				{Index: 2, IP: "10.0.0.2", Sent: 3, Lost: 0, RTTs: []time.Duration{2 * time.Millisecond}},
			},
		})
	}
	w.Close()
	time.Sleep(500 * time.Millisecond)

	r, _ := NewReader(ctx, cfg)
	defer r.Close()
	pts, err := r.QueryLatestHops(ctx,
		config.TargetRef{Target: config.Target{Name: "tlh"}, Group: "g"},
		storage.QueryFilter{},
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pts) != 2 {
		t.Fatalf("expected 2 hops, got %d", len(pts))
	}
	for _, p := range pts {
		if !p.Time.Equal(at.Add(2 * time.Minute)) {
			t.Errorf("hop %d not from latest cycle: time = %v", p.Index, p.Time)
		}
	}
}

// TestReaderQueryLatestHopsPerSource asserts the all-source path returns
// one cycle per source — not the single global most-recent cycle. The
// previous implementation used max(timestamp) without GROUP BY source,
// so whichever source flushed most recently won and every other source
// disappeared from the all-view.
func TestReaderQueryLatestHopsPerSource(t *testing.T) {
	cfg, cleanup := testDSN(t)
	defer cleanup()
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := Bootstrap(ctx, log, cfg); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	w, _ := NewWriter(ctx, log, cfg)
	base := time.Now().UTC().Truncate(time.Second).Add(-10 * time.Minute)
	// Two sources, each with multiple cycles. Source "slave-eu" gets its
	// most-recent cycle written *after* "master"'s most-recent — the bug
	// shape was "global max wins" so without per-source grouping master
	// would disappear from the response.
	for _, src := range []string{"master", "slave-eu"} {
		offset := time.Duration(0)
		if src == "slave-eu" {
			offset = 5 * time.Second
		}
		for i := 0; i < 3; i++ {
			w.OnCycle(ctx, scheduler.Cycle{
				Time:   base.Add(time.Duration(i)*time.Minute + offset),
				Target: config.TargetRef{Target: config.Target{Name: "tlhp"}, Group: "g"},
				Source: src,
				Sent:   3,
				Hops: []probe.Hop{
					{Index: 1, IP: "10.0.0.1", Sent: 3, Lost: 0, RTTs: []time.Duration{1 * time.Millisecond}},
					{Index: 2, IP: "10.0.0.2", Sent: 3, Lost: 0, RTTs: []time.Duration{2 * time.Millisecond}},
				},
			})
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("writer close: %v", err)
	}

	r, _ := NewReader(ctx, cfg)
	defer r.Close() //nolint:errcheck // test cleanup
	pts, err := r.QueryLatestHops(ctx,
		config.TargetRef{Target: config.Target{Name: "tlhp"}, Group: "g"},
		storage.QueryFilter{}, // unfiltered: expect both sources
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pts) != 4 {
		t.Fatalf("expected 4 rows (2 sources × 2 ttls), got %d: %+v", len(pts), pts)
	}
	bySource := map[string]int{}
	for _, p := range pts {
		bySource[p.Source]++
	}
	for _, want := range []string{"master", "slave-eu"} {
		if bySource[want] != 2 {
			t.Errorf("source %q: expected 2 hop rows, got %d", want, bySource[want])
		}
	}
}

// TestReaderQueryHopsAtSingleCycle writes three cycles inside the query
// window and asserts QueryHopsAt returns exactly the hops from the cycle
// nearest `at`, not a windowed mix. Regression for the bug where the
// previous LIMIT-N implementation stacked rows from several cycles into
// one response, causing the UI to render the hop table multiple times.
//
// The window is intentionally wider than the cycle spacing so the
// "naive ORDER BY abs(dt) LIMIT N" implementation would return rows
// from all three cycles.
func TestReaderQueryHopsAtSingleCycle(t *testing.T) {
	cfg, cleanup := testDSN(t)
	defer cleanup()
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := Bootstrap(ctx, log, cfg); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	w, _ := NewWriter(ctx, log, cfg)
	base := time.Now().UTC().Truncate(time.Second).Add(-5 * time.Minute)
	cycleTimes := []time.Time{
		base,
		base.Add(20 * time.Second),
		base.Add(40 * time.Second),
	}
	for _, ts := range cycleTimes {
		w.OnCycle(ctx, scheduler.Cycle{
			Time:   ts,
			Target: config.TargetRef{Target: config.Target{Name: "thsc"}, Group: "g"},
			Source: "master",
			Sent:   3,
			Hops: []probe.Hop{
				{Index: 1, IP: "10.0.0.1", Sent: 3, Lost: 0, RTTs: []time.Duration{1 * time.Millisecond}},
				{Index: 2, IP: "10.0.0.2", Sent: 3, Lost: 0, RTTs: []time.Duration{2 * time.Millisecond}},
			},
		})
	}
	if err := w.Close(); err != nil {
		t.Fatalf("writer close: %v", err)
	}

	r, _ := NewReader(ctx, cfg)
	defer r.Close() //nolint:errcheck // test cleanup

	target := cycleTimes[1] // the middle cycle
	pts, err := r.QueryHopsAt(ctx,
		config.TargetRef{Target: config.Target{Name: "thsc"}, Group: "g"},
		target, 30*time.Minute,
		storage.QueryFilter{Source: "master"},
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pts) != 2 {
		t.Fatalf("expected 2 hops (one ttl=1 + one ttl=2), got %d (timestamps: %v)",
			len(pts), uniqueTimes(pts))
	}
	for _, p := range pts {
		if !p.Time.Equal(target) {
			t.Errorf("hop ttl=%d not pinned to target cycle: time = %v, want %v",
				p.Index, p.Time, target)
		}
	}
}

// TestReaderQueryHopsAtPerSourceCycle exercises the per-source pinning
// branch: with two sources writing their own cycles at slightly
// different timestamps, an unfiltered QueryHopsAt should return each
// source's nearest cycle independently — not pick whichever source
// happens to have a row closest to `at` and drop the other.
func TestReaderQueryHopsAtPerSourceCycle(t *testing.T) {
	cfg, cleanup := testDSN(t)
	defer cleanup()
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := Bootstrap(ctx, log, cfg); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	w, _ := NewWriter(ctx, log, cfg)
	base := time.Now().UTC().Truncate(time.Second).Add(-5 * time.Minute)
	// Two sources, each with three cycles at staggered offsets.
	for _, src := range []string{"master", "slave-eu"} {
		offset := time.Duration(0)
		if src == "slave-eu" {
			offset = 5 * time.Second
		}
		for i := 0; i < 3; i++ {
			w.OnCycle(ctx, scheduler.Cycle{
				Time:   base.Add(time.Duration(i)*20*time.Second + offset),
				Target: config.TargetRef{Target: config.Target{Name: "thpc"}, Group: "g"},
				Source: src,
				Sent:   3,
				Hops: []probe.Hop{
					{Index: 1, IP: "10.0.0.1", Sent: 3, Lost: 0, RTTs: []time.Duration{1 * time.Millisecond}},
				},
			})
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("writer close: %v", err)
	}

	r, _ := NewReader(ctx, cfg)
	defer r.Close() //nolint:errcheck // test cleanup

	at := base.Add(25 * time.Second) // between cycle[0] (5s offset for slave) and cycle[1]
	pts, err := r.QueryHopsAt(ctx,
		config.TargetRef{Target: config.Target{Name: "thpc"}, Group: "g"},
		at, 30*time.Minute,
		storage.QueryFilter{}, // unfiltered: expect one cycle per source
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pts) != 2 {
		t.Fatalf("expected exactly 2 rows (one ttl=1 per source), got %d (rows: %+v)", len(pts), pts)
	}
	seen := map[string]bool{}
	for _, p := range pts {
		if seen[p.Source] {
			t.Errorf("duplicate source in response: %q", p.Source)
		}
		seen[p.Source] = true
	}
	for _, want := range []string{"master", "slave-eu"} {
		if !seen[want] {
			t.Errorf("missing source %q in response", want)
		}
	}
}

func uniqueTimes(pts []storage.HopPoint) []time.Time {
	seen := map[time.Time]struct{}{}
	var out []time.Time
	for _, p := range pts {
		if _, ok := seen[p.Time]; ok {
			continue
		}
		seen[p.Time] = struct{}{}
		out = append(out, p.Time)
	}
	return out
}

func TestReaderQueryHopsTimelineRawAndBucketed(t *testing.T) {
	cfg, cleanup := testDSN(t)
	defer cleanup()
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	_ = Bootstrap(ctx, log, cfg)
	w, _ := NewWriter(ctx, log, cfg)

	start := time.Now().UTC().Truncate(time.Hour).Add(-26 * time.Hour) // forces bucketed tier
	// Two cycles, second one's hop 2 addr flips (path flap).
	w.OnCycle(ctx, scheduler.Cycle{
		Time:   start,
		Target: config.TargetRef{Target: config.Target{Name: "tht"}, Group: "g"},
		Source: "master",
		Sent:   3,
		Hops: []probe.Hop{
			{Index: 1, IP: "10.0.0.1", Sent: 3, Lost: 0, RTTs: []time.Duration{1 * time.Millisecond}},
			{Index: 2, IP: "10.0.0.2", Sent: 3, Lost: 0, RTTs: []time.Duration{2 * time.Millisecond}},
		},
	})
	w.OnCycle(ctx, scheduler.Cycle{
		Time:   start.Add(5 * time.Minute),
		Target: config.TargetRef{Target: config.Target{Name: "tht"}, Group: "g"},
		Source: "master",
		Sent:   3,
		Hops: []probe.Hop{
			{Index: 1, IP: "10.0.0.1", Sent: 3, Lost: 0, RTTs: []time.Duration{1 * time.Millisecond}},
			{Index: 2, IP: "10.0.0.99", Sent: 3, Lost: 1, RTTs: []time.Duration{4 * time.Millisecond}}, // flipped
		},
	})
	w.Close()
	time.Sleep(500 * time.Millisecond)

	r, _ := NewReader(ctx, cfg)
	defer r.Close()

	// Bucketed query (span > 24h triggers 15m tier).
	pts, err := r.QueryHopsTimeline(ctx,
		config.TargetRef{Target: config.Target{Name: "tht"}, Group: "g"},
		start.Add(-time.Hour), start.Add(48*time.Hour),
		storage.QueryFilter{Step: 15 * time.Minute},
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	// Same bucket (within 15m): ttl=1 collapses to 1 row (stable addr).
	// ttl=2 has two distinct addrs in the same bucket: expect 2 rows.
	var ttl2 int
	for _, p := range pts {
		if p.Index == 2 {
			ttl2++
		}
	}
	if ttl2 != 2 {
		t.Fatalf("expected 2 rows for ttl=2 (path flap), got %d (full: %+v)", ttl2, pts)
	}
}

// TestReaderQueryHopsTimelineSpikePreservation pins the bucketed-query
// contract that brief loss spikes survive averaging.
//
// Regression: the bucketed SELECT projects both an averaged loss
// (`avg_loss_pct = 100*sum(lost)/sum(sent)`) and the per-cycle maximum
// (`max_loss_pct = max(loss_pct)`). A prior revision aliased the
// averaged value as `loss_pct`, which shadowed the underlying column
// and turned `max(loss_pct)` into max-of-an-aggregate — ClickHouse
// returns a 500 ("aggregate function inside aggregate function"). The
// API surfaced that as a 502. This test runs the bucketed query and
// asserts the value semantics, so any reintroduction of the shadowing
// (or any other SQL break that swallows the row) trips it before
// deploy.
//
// Scenario: one bucket-sized window with 10 hops at ttl=1, IP fixed.
// Nine cycles are clean (loss_pct=0); one cycle drops every packet
// (loss_pct=100). The bucket's averaged loss is 10%, but the spike
// max stays 100%. The heatmap colors by MaxLossPct so the spike
// remains visible — the test enforces both numbers.
func TestReaderQueryHopsTimelineSpikePreservation(t *testing.T) {
	cfg, cleanup := testDSN(t)
	defer cleanup()
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := Bootstrap(ctx, log, cfg); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	w, _ := NewWriter(ctx, log, cfg)

	// All 10 cycles inside the same 15-min bucket. Truncate to the bucket
	// boundary so all writes land in the same toStartOfInterval slot.
	start := time.Now().UTC().Truncate(15 * time.Minute).Add(-26 * time.Hour)
	for i := 0; i < 10; i++ {
		lost := 0
		if i == 5 {
			lost = 3 // one 100%-loss cycle in the middle of the bucket
		}
		w.OnCycle(ctx, scheduler.Cycle{
			Time:   start.Add(time.Duration(i) * 30 * time.Second),
			Target: config.TargetRef{Target: config.Target{Name: "thsp"}, Group: "g"},
			Source: "master",
			Sent:   3,
			Hops: []probe.Hop{
				{Index: 1, IP: "10.0.0.1", Sent: 3, Lost: lost, RTTs: []time.Duration{1 * time.Millisecond}},
			},
		})
	}
	if err := w.Close(); err != nil {
		t.Fatalf("writer close: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	r, _ := NewReader(ctx, cfg)
	defer r.Close() //nolint:errcheck // test cleanup

	// Bucketed query — this is the SQL path that broke when the alias
	// shadowed the column. A regression here surfaces as a query error
	// (Fatalf below), not a wrong value.
	pts, err := r.QueryHopsTimeline(ctx,
		config.TargetRef{Target: config.Target{Name: "thsp"}, Group: "g"},
		start.Add(-time.Hour), start.Add(time.Hour),
		storage.QueryFilter{Step: 15 * time.Minute},
	)
	if err != nil {
		t.Fatalf("bucketed query: %v", err)
	}
	if len(pts) != 1 {
		t.Fatalf("expected 1 bucketed row (single bucket × ttl=1 × one IP), got %d: %+v", len(pts), pts)
	}
	got := pts[0]
	// 1 lossy cycle × 3 packets out of 10 cycles × 3 packets = 10% average.
	if got.LossPct < 9 || got.LossPct > 11 {
		t.Errorf("LossPct: got %.2f, want ~10 (1 of 10 cycles at 100%%)", got.LossPct)
	}
	// max(loss_pct) over the bucket: the lossy cycle's per-cycle loss = 100%.
	if got.MaxLossPct < 99.9 {
		t.Errorf("MaxLossPct: got %.2f, want 100 (the 100%%-loss spike must survive bucketing)", got.MaxLossPct)
	}

	// Raw path must mirror LossPct into MaxLossPct so the UI can read
	// MaxLossPct uniformly without branching.
	rawPts, err := r.QueryHopsTimeline(ctx,
		config.TargetRef{Target: config.Target{Name: "thsp"}, Group: "g"},
		start.Add(-time.Hour), start.Add(time.Hour),
		storage.QueryFilter{Step: 0}, // raw
	)
	if err != nil {
		t.Fatalf("raw query: %v", err)
	}
	if len(rawPts) != 10 {
		t.Fatalf("raw: expected 10 rows (one per cycle), got %d", len(rawPts))
	}
	for _, p := range rawPts {
		if p.MaxLossPct != p.LossPct {
			t.Errorf("raw row at %v: MaxLossPct=%.2f, LossPct=%.2f — raw rows must mirror",
				p.Time, p.MaxLossPct, p.LossPct)
		}
	}
}

// TestBootstrapClusterMode runs the full bootstrap path with
// ON CLUSTER injected, verifying:
//   - CREATE DATABASE … ON CLUSTER lands the DB everywhere (not just on
//     the connection node, which was the bug previously masked by
//     single-node testing).
//   - CREATE TABLE … ON CLUSTER … ReplicatedMergeTree succeeds (engine
//     substitution + {shard}/{replica} macro resolution).
//   - ALTER TABLE … ON CLUSTER MODIFY TTL succeeds against the just-
//     created replicated tables.
//   - The engine reported by system.tables is ReplicatedMergeTree, not
//     MergeTree.
//
// Skipped unless CLICKHOUSE_CLUSTER is set; a single-node CH that still
// has the implicit `default` cluster can run this with CLICKHOUSE_CLUSTER=default
// to exercise the DDL syntax (no real replication, but full DDL coverage).
//
// Limitation: the assertions below only query system.tables on the
// connection node. Against a true multi-replica cluster, this catches
// the DDL succeeding at-large but not "node A succeeded, node B failed
// silently" — the Go client surfaces per-replica errors from ON CLUSTER
// statements as a non-nil Exec error, so the Bootstrap call itself is
// the real assertion; the post-check is a smoke test.
func TestBootstrapClusterMode(t *testing.T) {
	cfg, cleanup := testDSNCluster(t)
	defer cleanup()

	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	if err := Bootstrap(ctx, log, cfg); err != nil {
		t.Fatalf("bootstrap (cluster=%q): %v", cfg.Cluster, err)
	}

	// Idempotency under cluster mode — second Bootstrap re-issues all the
	// ON CLUSTER DDLs and must still succeed.
	if err := Bootstrap(ctx, log, cfg); err != nil {
		t.Fatalf("re-bootstrap (cluster=%q): %v", cfg.Cluster, err)
	}

	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{cfg.Addr},
		Auth: clickhouse.Auth{Database: cfg.Database, Username: cfg.Username, Password: cfg.Password},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer conn.Close() //nolint:errcheck // test cleanup

	for _, tbl := range []string{"probe_cycle", "probe_rtt", "probe_hop", "probe_http"} {
		var engine string
		err := conn.QueryRow(ctx,
			"SELECT engine FROM system.tables WHERE database = ? AND name = ?",
			cfg.Database, tbl,
		).Scan(&engine)
		if err != nil {
			t.Errorf("query engine for %s: %v", tbl, err)
			continue
		}
		if engine != "ReplicatedMergeTree" {
			t.Errorf("table %s: engine = %q, want ReplicatedMergeTree", tbl, engine)
		}
	}
}

// TestReaderSourceFilter confirms QueryFilter.Source narrows results to
// one source and that the empty-source path returns everything. Together
// they exercise both branches of the sourceFilter helper introduced when
// the (? = '' OR source = ?) pattern was retired in favour of a query
// shape the CH planner can prune granules on.
func TestReaderSourceFilter(t *testing.T) {
	cfg, cleanup := testDSN(t)
	defer cleanup()
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := Bootstrap(ctx, log, cfg); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	w, _ := NewWriter(ctx, log, cfg)
	at := time.Now().UTC().Truncate(time.Second)
	for i, source := range []string{"master", "slave-eu", "slave-us"} {
		w.OnCycle(ctx, scheduler.Cycle{
			Time:   at.Add(time.Duration(i) * time.Second),
			Target: config.TargetRef{Target: config.Target{Name: "tsf"}, Group: "g"},
			Source: source,
			Sent:   20,
		})
	}
	if err := w.Close(); err != nil {
		t.Fatalf("writer close: %v", err)
	}

	r, _ := NewReader(ctx, cfg)
	defer r.Close() //nolint:errcheck // test cleanup

	all, err := r.QueryCycles(ctx,
		config.TargetRef{Target: config.Target{Name: "tsf"}, Group: "g"},
		at.Add(-time.Hour), at.Add(time.Hour),
		storage.QueryFilter{},
	)
	if err != nil {
		t.Fatalf("query all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("unfiltered: expected 3 rows, got %d", len(all))
	}

	one, err := r.QueryCycles(ctx,
		config.TargetRef{Target: config.Target{Name: "tsf"}, Group: "g"},
		at.Add(-time.Hour), at.Add(time.Hour),
		storage.QueryFilter{Source: "slave-eu"},
	)
	if err != nil {
		t.Fatalf("query filtered: %v", err)
	}
	if len(one) != 1 {
		t.Fatalf("filtered: expected 1 row, got %d", len(one))
	}
	if one[0].Source != "slave-eu" {
		t.Errorf("filtered: source = %q, want slave-eu", one[0].Source)
	}
}
