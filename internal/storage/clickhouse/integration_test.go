//go:build integration

package clickhouse

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strings"
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

	w, err := NewWriter(ctx, log, cfg, 10)
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
	w, err := NewWriter(ctx, log, cfg, 10)
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
	w, _ := NewWriter(ctx, log, cfg, 10)
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
	w, _ := NewWriter(ctx, log, cfg, 10)

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

// TestReaderBucketedPercentilesMonotone seeds cycles with a fixed per-cycle
// percentile distribution and verifies the bucketed aggregation keeps the
// canonical p5 ≤ p25 ≤ median ≤ p75 ≤ p95 ordering. Regression for the
// avgWeighted(rtt_median_ms) bug that produced median < p25 because median
// used a different aggregation than the other percentile bands.
func TestReaderBucketedPercentilesMonotone(t *testing.T) {
	cfg, cleanup := testDSN(t)
	defer cleanup()
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	_ = Bootstrap(ctx, log, cfg)
	w, _ := NewWriter(ctx, log, cfg, 10)

	start := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	// Mix two RTT profiles so per-cycle medians differ across the bucket —
	// this is the shape that surfaced the bug in production (a stable target
	// at ~5ms where individual cycle medians occasionally landed at 4.0ms).
	profiles := [][]time.Duration{
		makeRTTs(20, 4900*time.Microsecond, 5000*time.Microsecond),
		makeRTTs(20, 4000*time.Microsecond, 5000*time.Microsecond),
	}
	for i := 0; i < 60; i++ {
		rtts := profiles[i%len(profiles)]
		w.OnCycle(ctx, scheduler.Cycle{
			Time:    start.Add(time.Duration(i) * time.Minute),
			Target:  config.TargetRef{Target: config.Target{Name: "tm"}, Group: "g"},
			Source:  "master",
			Sent:    len(rtts),
			Summary: stats.Compute(rtts),
		})
	}
	w.Close()
	time.Sleep(500 * time.Millisecond)

	r, _ := NewReader(ctx, cfg)
	defer r.Close()

	pts, err := r.QueryCycles(ctx,
		config.TargetRef{Target: config.Target{Name: "tm"}, Group: "g"},
		start, start.Add(time.Hour+time.Minute),
		storage.QueryFilter{Step: time.Hour},
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pts) == 0 {
		t.Fatalf("expected ≥ 1 bucket, got 0")
	}
	for i, p := range pts {
		if !(p.Min <= p.P5 && p.P5 <= p.P25 && p.P25 <= p.Median &&
			p.Median <= p.P75 && p.P75 <= p.P95 && p.P95 <= p.Max) {
			t.Errorf("bucket %d: percentiles not monotone: min=%g p5=%g p25=%g median=%g p75=%g p95=%g max=%g",
				i, p.Min, p.P5, p.P25, p.Median, p.P75, p.P95, p.Max)
		}
	}
}

// TestReaderBucketedLossExcludesFullLossCycles is the regression for the
// "loss going to 0" bars bug. A 100%-loss cycle stores all-zero percentile
// columns; weighting the bucket rollup by `sent` folds those zeros into the
// distribution and collapses the low percentiles to 0 (a full-height band to
// the log floor in the UI). The fix weights by received pings (sent - lost)
// so full-loss cycles drop out of the RTT shape while still counting toward
// loss_pct.
//
// Scenario: one 1h bucket holding 40 clean cycles (~100ms) interleaved with
// 20 fully-lost cycles. The bucket's low percentiles must stay near 100ms
// (not 0), and loss_pct must read ~33% (20 of 60 cycles lost).
func TestReaderBucketedLossExcludesFullLossCycles(t *testing.T) {
	cfg, cleanup := testDSN(t)
	defer cleanup()
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	_ = Bootstrap(ctx, log, cfg)
	w, _ := NewWriter(ctx, log, cfg, 10)

	start := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	clean := makeRTTs(20, 99*time.Millisecond, 101*time.Millisecond)
	target := config.TargetRef{Target: config.Target{Name: "tfl"}, Group: "g"}
	for i := 0; i < 60; i++ {
		c := scheduler.Cycle{
			Time:   start.Add(time.Duration(i) * time.Minute),
			Target: target,
			Source: "master",
			Sent:   20,
		}
		if i%3 == 0 {
			// Every third cycle is 100% loss: empty summary, all packets lost.
			c.LossCount = 20
			c.Summary = stats.Compute(nil)
		} else {
			c.Summary = stats.Compute(clean)
		}
		w.OnCycle(ctx, c)
	}
	w.Close()
	time.Sleep(500 * time.Millisecond)

	r, _ := NewReader(ctx, cfg)
	defer r.Close() //nolint:errcheck // test cleanup

	pts, err := r.QueryCycles(ctx, target,
		start, start.Add(time.Hour+time.Minute),
		storage.QueryFilter{Step: time.Hour},
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pts) == 0 {
		t.Fatalf("expected ≥ 1 bucket, got 0")
	}
	p := pts[0]
	// 20 of 60 cycles fully lost -> ~33%.
	if p.LossPct < 32 || p.LossPct > 35 {
		t.Errorf("LossPct = %.2f, want ~33", p.LossPct)
	}
	// The clean cycles sit at ~100ms; the full-loss zeros must NOT pull the low
	// percentiles down to 0. Pre-fix these all read 0.
	mustBePositive := map[string]float64{
		"Min": p.Min, "P5": p.P5, "P25": p.P25, "Median": p.Median, "P75": p.P75,
	}
	for name, v := range mustBePositive {
		if v < 90 {
			t.Errorf("%s = %.2f, want ~100 (full-loss zeros must not collapse the band)", name, v)
		}
	}
}

// TestReaderBucketedAllLossBucketIsFinite pins the NaN edge: a bucket whose
// every sub-cycle is 100% loss has zero total received packets, so the
// received-weighted avgWeighted(mean)/stddev divide by zero. Unguarded that
// yields NaN, which makes Go's encoding/json error out and kills the whole
// /cycles response. The query guards those to 0; this asserts the row comes
// back with finite fields and loss_pct == 100.
func TestReaderBucketedAllLossBucketIsFinite(t *testing.T) {
	cfg, cleanup := testDSN(t)
	defer cleanup()
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	_ = Bootstrap(ctx, log, cfg)
	w, _ := NewWriter(ctx, log, cfg, 10)

	start := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	target := config.TargetRef{Target: config.Target{Name: "tal"}, Group: "g"}
	for i := 0; i < 30; i++ {
		w.OnCycle(ctx, scheduler.Cycle{
			Time:      start.Add(time.Duration(i) * time.Minute),
			Target:    target,
			Source:    "master",
			Sent:      20,
			LossCount: 20,
			Summary:   stats.Compute(nil),
		})
	}
	w.Close()
	time.Sleep(500 * time.Millisecond)

	r, _ := NewReader(ctx, cfg)
	defer r.Close() //nolint:errcheck // test cleanup

	pts, err := r.QueryCycles(ctx, target,
		start, start.Add(time.Hour+time.Minute),
		storage.QueryFilter{Step: time.Hour},
	)
	if err != nil {
		t.Fatalf("query (would fail with NaN->JSON if unguarded): %v", err)
	}
	if len(pts) == 0 {
		t.Fatalf("expected ≥ 1 bucket, got 0")
	}
	p := pts[0]
	if p.LossPct < 99.9 {
		t.Errorf("LossPct = %.2f, want 100", p.LossPct)
	}
	for name, v := range map[string]float64{
		"Min": p.Min, "Max": p.Max, "Mean": p.Mean, "Median": p.Median,
		"StdDev": p.StdDev, "P5": p.P5, "P95": p.P95,
	} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			t.Errorf("%s = %v, want finite (NaN/Inf breaks JSON encoding)", name, v)
		}
	}
}

// TestReaderBucketedSourcesPreserved seeds cycles from multiple sources in
// the same bucket and asserts the bucketed query returns one row per
// (bucket, source) — regression for `GROUP BY bucket_ts` (without source)
// which collapsed all sources into a single row per bucket with
// `any(source)` picking an arbitrary label.
func TestReaderBucketedSourcesPreserved(t *testing.T) {
	cfg, cleanup := testDSN(t)
	defer cleanup()
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	_ = Bootstrap(ctx, log, cfg)
	w, _ := NewWriter(ctx, log, cfg, 10)

	start := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	sources := []string{"master", "slave-a", "slave-b"}
	rtts := makeRTTs(20, time.Millisecond, 2*time.Millisecond)
	for i := 0; i < 60; i++ {
		for _, src := range sources {
			w.OnCycle(ctx, scheduler.Cycle{
				Time:    start.Add(time.Duration(i) * time.Minute),
				Target:  config.TargetRef{Target: config.Target{Name: "ts"}, Group: "g"},
				Source:  src,
				Sent:    len(rtts),
				Summary: stats.Compute(rtts),
			})
		}
	}
	w.Close()
	time.Sleep(500 * time.Millisecond)

	r, _ := NewReader(ctx, cfg)
	defer r.Close()

	pts, err := r.QueryCycles(ctx,
		config.TargetRef{Target: config.Target{Name: "ts"}, Group: "g"},
		start, start.Add(time.Hour+time.Minute),
		storage.QueryFilter{Step: time.Hour},
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	gotSources := make(map[string]struct{})
	for _, p := range pts {
		gotSources[p.Source] = struct{}{}
	}
	for _, want := range sources {
		if _, ok := gotSources[want]; !ok {
			t.Errorf("source %q missing from bucketed result; got sources %v", want, gotSources)
		}
	}
}

// TestReaderQueryOverview seeds two targets with two sources each and asserts
// the per-(group,name,source) rollup is correct: one row per tuple, sparkline
// length 24, RTT/loss numbers reflect the underlying cycles.
func TestReaderQueryOverview(t *testing.T) {
	cfg, cleanup := testDSN(t)
	defer cleanup()
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := Bootstrap(ctx, log, cfg); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	w, _ := NewWriter(ctx, log, cfg, 10)

	now := time.Now().UTC().Truncate(time.Second)
	// Seed a 1h window of cycles, one per minute.
	from := now.Add(-time.Hour).Truncate(time.Minute)
	to := now

	// Profiles: master is clean (1-2ms, 0 loss); slave is lossy (5 lost of 20).
	cleanRTTs := makeRTTs(20, time.Millisecond, 2*time.Millisecond)
	lossyRTTs := makeRTTs(15, 30*time.Millisecond, 60*time.Millisecond)

	target1 := config.TargetRef{Target: config.Target{Name: "t1"}, Group: "g"}
	target2 := config.TargetRef{Target: config.Target{Name: "t2"}, Group: "g"}

	for i := 0; i < 60; i++ {
		ts := from.Add(time.Duration(i) * time.Minute)
		// target1 from master: clean
		w.OnCycle(ctx, scheduler.Cycle{
			Time:    ts,
			Target:  target1,
			Source:  "master",
			Sent:    20,
			Summary: stats.Compute(cleanRTTs),
		})
		// target1 from eu-west: lossy (5 lost of 20)
		w.OnCycle(ctx, scheduler.Cycle{
			Time:      ts,
			Target:    target1,
			Source:    "eu-west",
			Sent:      20,
			LossCount: 5,
			Summary:   stats.Compute(lossyRTTs),
		})
		// target2 from master only, clean
		w.OnCycle(ctx, scheduler.Cycle{
			Time:    ts,
			Target:  target2,
			Source:  "master",
			Sent:    20,
			Summary: stats.Compute(cleanRTTs),
		})
	}
	w.Close()
	time.Sleep(500 * time.Millisecond)

	r, _ := NewReader(ctx, cfg)
	defer r.Close() //nolint:errcheck // test cleanup

	rows, err := r.QueryOverview(ctx, from, to.Add(time.Minute),
		[]config.TargetRef{target1, target2})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	// 3 expected (target,source) tuples: (t1,master), (t1,eu-west), (t2,master).
	if len(rows) != 3 {
		t.Fatalf("rows=%d, want 3: %+v", len(rows), rows)
	}

	byKey := make(map[string]storage.OverviewSourceRow, len(rows))
	for _, row := range rows {
		byKey[row.Group+"/"+row.Name+"|"+row.Source] = row
	}

	clean := byKey["g/t1|master"]
	lossy := byKey["g/t1|eu-west"]
	other := byKey["g/t2|master"]

	if clean.LossAvg != 0 {
		t.Errorf("g/t1 master loss_avg=%v, want 0", clean.LossAvg)
	}
	if lossy.LossAvg <= clean.LossAvg {
		t.Errorf("g/t1 eu-west loss_avg=%v should exceed master (%v)", lossy.LossAvg, clean.LossAvg)
	}
	// 5/20 = 25%
	if lossy.LossAvg < 24 || lossy.LossAvg > 26 {
		t.Errorf("g/t1 eu-west loss_avg=%v, want ~25", lossy.LossAvg)
	}
	if lossy.RTTMax <= clean.RTTMax {
		t.Errorf("g/t1 eu-west rtt_max=%v should exceed master (%v)", lossy.RTTMax, clean.RTTMax)
	}
	if other.Source != "master" {
		t.Errorf("g/t2 source=%q, want master", other.Source)
	}

	// Sparkline length 24 across the board.
	for _, row := range rows {
		if len(row.Sparkline) != 24 {
			t.Errorf("%s/%s source=%s sparkline len=%d, want 24",
				row.Group, row.Name, row.Source, len(row.Sparkline))
		}
		// At least one bucket should have a non-nil value (we seeded 60 cycles).
		any := false
		for _, v := range row.Sparkline {
			if v != nil {
				any = true
				break
			}
		}
		if !any {
			t.Errorf("%s/%s source=%s sparkline all-nil; expected coverage",
				row.Group, row.Name, row.Source)
		}
	}

	// last_seen must be within the seeded window.
	for _, row := range rows {
		if row.LastSeen.Before(from) || row.LastSeen.After(to.Add(time.Minute)) {
			t.Errorf("%s/%s source=%s last_seen=%v outside [%v,%v]",
				row.Group, row.Name, row.Source, row.LastSeen, from, to)
		}
	}
}

// TestReaderQueryOverviewScopesToTargets verifies the (group,name) IN filter
// keeps rows belonging to unrelated targets out of the response.
func TestReaderQueryOverviewScopesToTargets(t *testing.T) {
	cfg, cleanup := testDSN(t)
	defer cleanup()
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	_ = Bootstrap(ctx, log, cfg)
	w, _ := NewWriter(ctx, log, cfg, 10)

	now := time.Now().UTC().Truncate(time.Second)
	from := now.Add(-30 * time.Minute)
	rtts := makeRTTs(20, time.Millisecond, 2*time.Millisecond)

	// Seed cycles for three distinct targets.
	for _, name := range []string{"included", "excluded", "alsoexcluded"} {
		w.OnCycle(ctx, scheduler.Cycle{
			Time:    from.Add(time.Minute),
			Target:  config.TargetRef{Target: config.Target{Name: name}, Group: "g"},
			Source:  "master",
			Sent:    20,
			Summary: stats.Compute(rtts),
		})
	}
	w.Close()
	time.Sleep(500 * time.Millisecond)

	r, _ := NewReader(ctx, cfg)
	defer r.Close() //nolint:errcheck // test cleanup

	// Ask for "included" only.
	rows, err := r.QueryOverview(ctx, from, now,
		[]config.TargetRef{{Target: config.Target{Name: "included"}, Group: "g"}})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows=%d, want 1 (only included): %+v", len(rows), rows)
	}
	if rows[0].Name != "included" {
		t.Errorf("rows[0].Name=%q, want included", rows[0].Name)
	}
}

// TestReaderQueryOverviewEmptyTargets verifies that calling with no target
// refs returns no rows and no error (rather than a bare IN () SQL error).
func TestReaderQueryOverviewEmptyTargets(t *testing.T) {
	cfg, cleanup := testDSN(t)
	defer cleanup()
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	_ = Bootstrap(ctx, log, cfg)

	r, _ := NewReader(ctx, cfg)
	defer r.Close() //nolint:errcheck // test cleanup

	rows, err := r.QueryOverview(ctx, time.Now().Add(-time.Hour), time.Now(), nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("rows=%d, want 0", len(rows))
	}
}

// makeRTTs returns n samples linearly spaced from lo to hi inclusive.
func makeRTTs(n int, lo, hi time.Duration) []time.Duration {
	out := make([]time.Duration, n)
	if n == 1 {
		out[0] = lo
		return out
	}
	for i := 0; i < n; i++ {
		out[i] = lo + time.Duration(int64(hi-lo)*int64(i)/int64(n-1))
	}
	return out
}

func TestReaderQueryRTTs(t *testing.T) {
	cfg, cleanup := testDSN(t)
	defer cleanup()
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	_ = Bootstrap(ctx, log, cfg)
	w, _ := NewWriter(ctx, log, cfg, 10)
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
	w, _ := NewWriter(ctx, log, cfg, 10)
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
	w, _ := NewWriter(ctx, log, cfg, 10)
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
	w, _ := NewWriter(ctx, log, cfg, 10)
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

// TestReaderQueryLatestHopsStaleSourceDropped asserts the LatestSince floor
// removes a source whose newest hop predates the cutoff while keeping a
// source that is still reporting. Regression for a removed/stopped probe
// origin rendering as a live path until its rows age out of retention.
func TestReaderQueryLatestHopsStaleSourceDropped(t *testing.T) {
	cfg, cleanup := testDSN(t)
	defer cleanup()
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := Bootstrap(ctx, log, cfg); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	w, _ := NewWriter(ctx, log, cfg, 10)
	now := time.Now().UTC().Truncate(time.Second)
	// "live" wrote 1 minute ago; "removed" last wrote an hour ago.
	srcTimes := map[string]time.Time{
		"live":    now.Add(-1 * time.Minute),
		"removed": now.Add(-1 * time.Hour),
	}
	for src, ts := range srcTimes {
		w.OnCycle(ctx, scheduler.Cycle{
			Time:   ts,
			Target: config.TargetRef{Target: config.Target{Name: "tlhs"}, Group: "g"},
			Source: src,
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
	ref := config.TargetRef{Target: config.Target{Name: "tlhs"}, Group: "g"}

	// Cutoff at 5 minutes ago: "live" survives, "removed" is dropped.
	cutoff := now.Add(-5 * time.Minute)
	pts, err := r.QueryLatestHops(ctx, ref, storage.QueryFilter{LatestSince: cutoff})
	if err != nil {
		t.Fatalf("query (floored): %v", err)
	}
	for _, p := range pts {
		if p.Source == "removed" {
			t.Errorf("stale source %q should have been dropped by LatestSince floor", p.Source)
		}
	}
	if got := countSources(pts, "live"); got != 2 {
		t.Errorf("live source: expected 2 hop rows, got %d", got)
	}

	// Zero floor: both sources returned (existing behaviour preserved).
	all, err := r.QueryLatestHops(ctx, ref, storage.QueryFilter{})
	if err != nil {
		t.Fatalf("query (unfloored): %v", err)
	}
	if got := countSources(all, "removed"); got != 2 {
		t.Errorf("with no floor, removed source should remain: got %d rows", got)
	}
}

func countSources(pts []storage.HopPoint, source string) int {
	n := 0
	for _, p := range pts {
		if p.Source == source {
			n++
		}
	}
	return n
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
	w, _ := NewWriter(ctx, log, cfg, 10)
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
	w, _ := NewWriter(ctx, log, cfg, 10)
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
	w, _ := NewWriter(ctx, log, cfg, 10)

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
	w, _ := NewWriter(ctx, log, cfg, 10)

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
	// WorstTime must point at the exact lossy cycle (i=5, start+150s), not the
	// bucket start — this is what lets a heatmap-cell click open the cycle that
	// justifies the cell's colour instead of the bucket's first (clean) cycle.
	wantWorst := start.Add(5 * 30 * time.Second)
	if !got.WorstTime.Equal(wantWorst) {
		t.Errorf("WorstTime: got %s, want %s (the 100%%-loss cycle, not bucket start %s)",
			got.WorstTime.UTC(), wantWorst.UTC(), got.Time.UTC())
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
		// Raw rows are a single cycle, so the worst-loss cycle is themselves.
		if !p.WorstTime.Equal(p.Time) {
			t.Errorf("raw row at %v: WorstTime=%s, want == Time", p.Time, p.WorstTime.UTC())
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
// the (? = ” OR source = ?) pattern was retired in favour of a query
// shape the CH planner can prune granules on.
func TestReaderSourceFilter(t *testing.T) {
	cfg, cleanup := testDSN(t)
	defer cleanup()
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := Bootstrap(ctx, log, cfg); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	w, _ := NewWriter(ctx, log, cfg, 10)
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

// Bootstrap DDL vs the writer's column lists, Append order vs table order, and
// each hop read path's own select/scan arity are invisible to the faked driver
// the unit suite uses, so only a live round trip proves them.
func TestIntegrationHopAnnotationsRoundTrip(t *testing.T) {
	cfg, cleanup := testDSN(t)
	defer cleanup()
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := Bootstrap(ctx, log, cfg); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	w, err := NewWriter(ctx, log, cfg, 10)
	if err != nil {
		t.Fatalf("writer: %v", err)
	}
	// The only live check that NewWriter routes through newWriterChans — the
	// unit suite cannot construct a real Writer.
	if got := cap(w.chans[tableProbeHop]); got != 131072 {
		t.Fatalf("hop chan cap = %d, want 131072", got)
	}
	if got := cap(w.chans[tableProbeRTT]); got != 40960 {
		t.Fatalf("rtt chan cap = %d, want 40960 for pings=10", got)
	}

	ref := config.TargetRef{Group: "g", Target: config.Target{Name: "unrt"}}
	at := time.Now().UTC().Truncate(time.Second)
	cy := scheduler.Cycle{
		Time:   at,
		Target: ref,
		Source: "master",
		Sent:   3,
		Hops: []probe.Hop{
			{Index: 1, IP: "10.0.0.1", RTTs: []time.Duration{time.Millisecond}, Sent: 3},
			{Index: 2, IP: "10.0.0.2", RTTs: []time.Duration{time.Millisecond}, Sent: 2},
			{Index: 2, IP: "10.0.9.9", RTTs: []time.Duration{2 * time.Millisecond}, Sent: 1},
			{Index: 3, IP: "10.0.0.3", RTTs: []time.Duration{time.Millisecond}, Sent: 3, Unreach: "admin-prohibited"},
		},
	}
	w.OnCycle(ctx, cy)
	// Second, older cycle so QueryHopsAt has a distinct nearest pick and the
	// marker round-trips: its terminal row is a target echo.
	cy2 := cy
	cy2.Time = at.Add(-2 * time.Minute)
	cy2.Hops = []probe.Hop{
		{Index: 1, IP: "10.0.0.1", RTTs: []time.Duration{time.Millisecond}, Sent: 3},
		{Index: 2, IP: "192.0.2.9", RTTs: []time.Duration{time.Millisecond}, Sent: 3, TargetReply: true},
	}
	w.OnCycle(ctx, cy2)
	w.Close()
	time.Sleep(500 * time.Millisecond)

	r, err := NewReader(ctx, cfg)
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	defer r.Close()

	// Path 1: QueryLatestHops.
	got, err := r.QueryLatestHops(ctx, ref, storage.QueryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d hop rows, want 4 (per (ttl,addr)): %+v", len(got), got)
	}
	byAddr := map[string]storage.HopPoint{}
	for _, h := range got {
		byAddr[h.IP] = h
	}
	if h := byAddr["10.0.0.3"]; h.Unreach != "admin-prohibited" {
		t.Fatalf("unreach did not survive the round trip: %+v", h)
	}
	if h := byAddr["10.0.9.9"]; h.Index != 2 || h.Sent != 1 {
		t.Fatalf("second responder row wrong: %+v", h)
	}
	if h := byAddr["10.0.0.1"]; h.Unreach != "" || h.TargetReply {
		t.Fatalf("annotations invented on a clean hop: %+v", h)
	}

	// Path 2: QueryHopsAt — its select list and scan are separate code.
	atHops, err := r.QueryHopsAt(ctx, ref, cy2.Time, 30*time.Minute, storage.QueryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var marked bool
	for _, h := range atHops {
		if h.IP == "192.0.2.9" && h.TargetReply {
			marked = true
		}
		if h.IP == "10.0.0.1" && h.TargetReply {
			t.Fatalf("QueryHopsAt marked an unmarked row: %+v", h)
		}
	}
	if !marked {
		t.Fatalf("QueryHopsAt lost the target-reply marker: %+v", atHops)
	}

	// Path 3: raw timeline (step 0) — third select list, shared scan.
	rawTL, err := r.QueryHopsTimeline(ctx, ref, at.Add(-time.Hour), at.Add(time.Hour), storage.QueryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var rawFound, rawMarked bool
	for _, h := range rawTL {
		if h.IP == "10.0.0.3" && h.Unreach == "admin-prohibited" {
			rawFound = true
		}
		if h.IP == "192.0.2.9" && h.TargetReply {
			rawMarked = true
		}
	}
	if !rawFound {
		t.Fatalf("raw timeline lost the annotation: %+v", rawTL)
	}
	if !rawMarked {
		t.Fatalf("raw timeline lost the target-reply marker: %+v", rawTL)
	}

	// Path 4: bucketed timeline — its own select and scan, max(unreach).
	tl, err := r.QueryHopsTimeline(ctx, ref, at.Add(-time.Hour), at.Add(time.Hour),
		storage.QueryFilter{Step: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, h := range tl {
		if h.IP == "10.0.0.3" && h.Unreach == "admin-prohibited" {
			found = true
		}
	}
	if !found {
		t.Fatalf("bucketed timeline lost the annotation: %+v", tl)
	}
}

// A freshly bootstrapped database gets the annotation columns from CREATE
// TABLE, so only a table that predates them proves Bootstrap still runs
// addColumnStatements — the shape every already-deployed instance upgrades from.
func TestIntegrationBootstrapUpgradesLegacyTables(t *testing.T) {
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

	// The codec is asserted alongside the type: an ADD COLUMN that omits the
	// codec its CREATE TABLE column carries leaves an upgraded deployment with
	// a different column definition than a fresh one.
	columnDef := func(table, column string) (string, string) {
		var defs []string
		err := conn.QueryRow(ctx,
			"SELECT groupArray(concat(type, '\t', compression_codec)) FROM system.columns WHERE database = ? AND table = ? AND name = ?",
			cfg.Database, table, column,
		).Scan(&defs)
		if err != nil {
			t.Fatalf("system.columns %s.%s: %v", table, column, err)
		}
		if len(defs) == 0 {
			return "", ""
		}
		typ, codec, _ := strings.Cut(defs[0], "\t")
		return typ, codec
	}
	columnType := func(table, column string) string {
		typ, _ := columnDef(table, column)
		return typ
	}

	added := []struct{ table, column, typ, codec string }{
		{"probe_rtt", "target_group", "LowCardinality(String)", ""},
		{"probe_hop", "target_group", "LowCardinality(String)", ""},
		{"probe_http", "target_group", "LowCardinality(String)", ""},
		{"probe_hop", "unreach", "LowCardinality(String)", ""},
		{"probe_hop", "target_reply", "UInt8", "CODEC(T64, ZSTD(1))"},
	}
	for _, c := range added {
		stmt := fmt.Sprintf("ALTER TABLE %s DROP COLUMN %s", c.table, c.column)
		if err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
		if got := columnType(c.table, c.column); got != "" {
			t.Fatalf("%s.%s still present after drop: %s", c.table, c.column, got)
		}
	}

	if err := Bootstrap(ctx, log, cfg); err != nil {
		t.Fatalf("re-bootstrap: %v", err)
	}
	for _, c := range added {
		typ, codec := columnDef(c.table, c.column)
		if typ != c.typ {
			t.Fatalf("%s.%s after upgrade = %q, want %q", c.table, c.column, typ, c.typ)
		}
		if codec != c.codec {
			t.Fatalf("%s.%s codec after upgrade = %q, want %q — a fresh CREATE TABLE and an upgrade must agree",
				c.table, c.column, codec, c.codec)
		}
	}

	w, err := NewWriter(ctx, log, cfg, 10)
	if err != nil {
		t.Fatalf("writer: %v", err)
	}
	ref := config.TargetRef{Group: "ug", Target: config.Target{Name: "upgraded"}}
	at := time.Now().UTC().Truncate(time.Second)
	w.OnCycle(ctx, scheduler.Cycle{
		Time:   at,
		Target: ref,
		Source: "master",
		Sent:   3,
		RTTs:   []time.Duration{time.Millisecond},
		Hops: []probe.Hop{
			{Index: 1, IP: "10.1.0.1", RTTs: []time.Duration{time.Millisecond}, Sent: 3},
			{Index: 2, IP: "10.1.0.2", RTTs: []time.Duration{time.Millisecond}, Sent: 3, Unreach: "host-unreachable", TargetReply: true},
		},
		HTTPSamples: []probe.HTTPSample{{Time: at, RTT: time.Millisecond, Status: 200}},
	})
	w.Close()
	time.Sleep(500 * time.Millisecond)

	r, err := NewReader(ctx, cfg)
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	defer r.Close()
	got, err := r.QueryLatestHops(ctx, ref, storage.QueryFilter{})
	if err != nil {
		t.Fatalf("latest hops: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d hop rows from the upgraded table, want 2: %+v", len(got), got)
	}
	var annotated bool
	for _, h := range got {
		if h.IP == "10.1.0.2" && h.Unreach == "host-unreachable" && h.TargetReply {
			annotated = true
		}
	}
	if !annotated {
		t.Fatalf("annotations did not survive the upgraded table: %+v", got)
	}
}

// A slave that stamps a cycle years ahead pinned itself as that source's
// newest hop row until the lie expired, and outlived probe_hop's TTL because
// that TTL derives from the row timestamp. Ingest refuses such a row now, but
// rows already written are only kept off the endpoint by the reader's
// ceiling — asserted here against a real server, since the unit test can only
// read the SQL text.
func TestIntegrationQueryLatestHopsIgnoresFutureRows(t *testing.T) {
	cfg, cleanup := testDSN(t)
	defer cleanup()
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := Bootstrap(ctx, log, cfg); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	w, _ := NewWriter(ctx, log, cfg, 10)
	now := time.Now().UTC().Truncate(time.Second)
	ref := config.TargetRef{Target: config.Target{Name: "future"}, Group: "g"}
	rows := map[string]time.Time{
		"honest":   now.Add(-time.Minute),
		"poisoned": now.AddDate(5, 0, 0),
	}
	for ip, ts := range rows {
		w.OnCycle(ctx, scheduler.Cycle{
			Time: ts, Target: ref, Source: "edge-1", Sent: 3,
			Hops: []probe.Hop{{Index: 1, IP: ip, Sent: 3, Lost: 0,
				RTTs: []time.Duration{time.Millisecond}}},
		})
	}
	if err := w.Close(); err != nil {
		t.Fatalf("writer close: %v", err)
	}

	r, _ := NewReader(ctx, cfg)
	defer r.Close() //nolint:errcheck // test cleanup
	pts, err := r.QueryLatestHops(ctx, ref, storage.QueryFilter{})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pts) != 1 {
		t.Fatalf("got %d rows, want the one honest row: %+v", len(pts), pts)
	}
	if pts[0].IP != "honest" {
		t.Fatalf("latest hop is %q, want the honest row — the future one is still served", pts[0].IP)
	}
}
