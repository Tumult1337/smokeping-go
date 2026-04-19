//go:build integration

package storage

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/scheduler"
	"github.com/tumult/gosmokeping/internal/stats"
)

// Integration tests require a live InfluxDB v2 at INFLUX_URL with INFLUX_TOKEN
// and INFLUX_ORG set. Run with: go test -tags=integration ./internal/storage
func testConfig(t *testing.T) config.InfluxDB {
	t.Helper()
	url := os.Getenv("INFLUX_URL")
	token := os.Getenv("INFLUX_TOKEN")
	org := os.Getenv("INFLUX_ORG")
	if url == "" || token == "" || org == "" {
		t.Skip("INFLUX_URL/INFLUX_TOKEN/INFLUX_ORG not set")
	}
	return config.InfluxDB{
		URL:       url,
		Token:     token,
		Org:       org,
		BucketRaw: "gosmokeping_test_raw",
		Bucket1h:  "gosmokeping_test_1h",
		Bucket1d:  "gosmokeping_test_1d",
	}
}

func TestBootstrapAndWrite(t *testing.T) {
	cfg := testConfig(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := Bootstrap(ctx, log, cfg); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	w := NewWriter(log, cfg)
	defer w.Close()

	if err := w.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	c := scheduler.Cycle{
		Time: time.Now(),
		Target: config.TargetRef{
			Group:  "testgroup",
			Target: config.Target{Name: "testtarget", Host: "127.0.0.1", Probe: "icmp"},
		},
		ProbeName: "icmp",
		Sent:      3,
		RTTs:      []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond},
		Summary:   stats.Compute([]time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond}),
	}
	w.OnCycle(ctx, c)
	// Force flush so the read-back below sees our data.
	w.write.Flush()

	r := NewReader(cfg)
	defer r.Close()

	from := c.Time.Add(-time.Minute)
	to := c.Time.Add(time.Minute)
	cycles, err := r.QueryCycles(ctx, c.Target, from, to, ResolutionRaw, "")
	if err != nil {
		t.Fatalf("query cycles: %v", err)
	}
	if len(cycles) == 0 {
		t.Fatal("no cycles returned")
	}
	if cycles[0].Median == 0 {
		t.Errorf("median = 0, want >0")
	}

	rtts, err := r.QueryRTTs(ctx, c.Target, from, to, "")
	if err != nil {
		t.Fatalf("query rtts: %v", err)
	}
	if len(rtts) != 3 {
		t.Errorf("got %d rtts, want 3", len(rtts))
	}
}

// TestSourceTagRoundtrip verifies that cycles written with Cycle.Source=X are
// retrievable via QueryCycles with source=X, invisible to source=Y, and still
// returned by source="" (no-filter mode — the backward-compat path).
func TestSourceTagRoundtrip(t *testing.T) {
	cfg := testConfig(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := Bootstrap(ctx, log, cfg); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	w := NewWriter(log, cfg)
	defer w.Close()

	c := scheduler.Cycle{
		Time: time.Now(),
		Target: config.TargetRef{
			Group:  "testgroup",
			Target: config.Target{Name: "sourcetag", Host: "127.0.0.1", Probe: "icmp"},
		},
		ProbeName: "icmp",
		Source:    "eu-west",
		Sent:      2,
		RTTs:      []time.Duration{5 * time.Millisecond, 7 * time.Millisecond},
		Summary:   stats.Compute([]time.Duration{5 * time.Millisecond, 7 * time.Millisecond}),
	}
	w.OnCycle(ctx, c)
	w.write.Flush()

	r := NewReader(cfg)
	defer r.Close()

	from := c.Time.Add(-time.Minute)
	to := c.Time.Add(time.Minute)

	matching, err := r.QueryCycles(ctx, c.Target, from, to, ResolutionRaw, "eu-west")
	if err != nil {
		t.Fatalf("query cycles (matching source): %v", err)
	}
	if len(matching) == 0 {
		t.Fatal("no cycles with source=eu-west")
	}

	other, err := r.QueryCycles(ctx, c.Target, from, to, ResolutionRaw, "us-east")
	if err != nil {
		t.Fatalf("query cycles (other source): %v", err)
	}
	if len(other) != 0 {
		t.Errorf("got %d cycles with source=us-east, want 0", len(other))
	}

	unfiltered, err := r.QueryCycles(ctx, c.Target, from, to, ResolutionRaw, "")
	if err != nil {
		t.Fatalf("query cycles (unfiltered): %v", err)
	}
	if len(unfiltered) == 0 {
		t.Fatal("unfiltered query returned nothing")
	}
}
