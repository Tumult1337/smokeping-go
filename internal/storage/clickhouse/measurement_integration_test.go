//go:build integration

package clickhouse

import (
	"context"
	"io"
	"log/slog"
	"math"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/storage"
)

// All history consumers read the same stored fixtures, including the 1/0
// legacy row that must disappear and the 10/9 row that must become 100% loss.
func TestHistoricalMeasurementAgreement(t *testing.T) {
	cfg, cleanup := testDSN(t)
	defer cleanup()
	ctx := context.Background()
	if err := Bootstrap(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), cfg); err != nil {
		t.Fatal(err)
	}
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{cfg.Addr},
		Auth: clickhouse.Auth{Database: cfg.Database, Username: cfg.Username, Password: cfg.Password},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	reader := &Reader{conn: conn}
	start := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	end := start.Add(24 * time.Minute)
	type cycleFixture struct {
		sent, lost uint16
		rtt        uint32
		loss       float32
	}
	for _, tc := range []struct {
		name                      string
		cycles                    []cycleFixture
		wantRows                  int
		wantSent, wantLost        int64
		wantLoss, wantOverviewAvg float64
		wantRTT                   bool
	}{
		{"legacy_zero_only", []cycleFixture{{1, 0, 0, 0}}, 0, 0, 0, 0, 0, false},
		{"legacy_loss_only", []cycleFixture{{10, 9, 0, 90}}, 1, 9, 9, 100, 100, false},
		{"mixed", []cycleFixture{{10, 9, 100000, 90}, {1, 0, 0, 0}, {10, 9, 0, 90}}, 2, 19, 18, 1800.0 / 19, 95, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref := config.TargetRef{Group: "measurement", Target: config.Target{Name: tc.name}}
			for index, cy := range tc.cycles {
				at := start.Add(time.Duration(index+1) * time.Second)
				if err := conn.Exec(ctx, `INSERT INTO probe_cycle
  (timestamp, target_id, target_group, source, probe_type, sent, lost, loss_pct,
   rtt_min_us, rtt_max_us, rtt_mean_us, rtt_median_us, p5_us, p95_us)
VALUES (?, ?, ?, 'master', 'mtr', ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
					at, ref.Target.Name, ref.Group, cy.sent, cy.lost, cy.loss, cy.rtt, cy.rtt, cy.rtt, cy.rtt, cy.rtt, cy.rtt); err != nil {
					t.Fatal(err)
				}
				if err := conn.Exec(ctx, `INSERT INTO probe_hop
  (timestamp, target_id, target_group, source, ttl, sent, lost)
VALUES (?, ?, ?, 'master', 1, 1, 1)`, at, ref.Target.Name, ref.Group); err != nil {
					t.Fatal(err)
				}
			}
			raw, err := reader.QueryCycles(ctx, ref, start, end, storage.QueryFilter{Source: "master"})
			if err != nil || len(raw) != tc.wantRows {
				t.Fatalf("raw = %+v, err = %v; want %d rows", raw, err, tc.wantRows)
			}
			for index, cy := range tc.cycles {
				at := start.Add(time.Duration(index+1) * time.Second)
				pinned, err := reader.QueryHopsAt(ctx, ref, at, 2*time.Millisecond, storage.QueryFilter{Source: "master"})
				if err != nil || len(pinned.Hops) != 1 {
					t.Fatalf("pinned hops = %+v, err = %v", pinned, err)
				}
				if cy.rtt == 0 && cy.lost == 0 {
					if len(pinned.Cycles) != 0 {
						t.Fatalf("zero-completed pinned loss = %+v", pinned.Cycles)
					}
					continue
				}
				var matching *storage.CyclePoint
				for _, point := range raw {
					if point.Time.Equal(at) {
						matching = &point
					}
				}
				if matching == nil || len(pinned.Cycles) != 1 {
					t.Fatalf("missing matching raw/pinned cycle at %s: %+v", at, pinned)
				}
				counter := pinned.Cycles[0]
				if counter.Sent != matching.Sent || counter.LossCount != matching.LossCount || counter.LossPct != matching.LossPct {
					t.Fatalf("pinned %+v disagrees with raw %+v", counter, matching)
				}
			}
			bucketed, err := reader.QueryCycles(ctx, ref, start, end, storage.QueryFilter{Source: "master", Step: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			timeline, err := reader.QueryHopsTimeline(ctx, ref, start, end, storage.QueryFilter{Source: "master", Step: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			overview, err := reader.QueryOverview(ctx, start, end, []config.TargetRef{ref})
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantRows == 0 {
				if len(bucketed) != 0 || len(timeline.TimelineLoss) != 0 || len(overview) != 0 {
					t.Fatalf("zero-completed measurements retained: buckets=%+v timeline=%+v overview=%+v", bucketed, timeline.TimelineLoss, overview)
				}
				return
			}
			if len(bucketed) != 1 || len(timeline.TimelineLoss) != 1 || len(overview) != 1 {
				t.Fatalf("expected one aggregate per consumer: buckets=%+v timeline=%+v overview=%+v", bucketed, timeline.TimelineLoss, overview)
			}
			bucket, loss, summary := bucketed[0], timeline.TimelineLoss[0], overview[0]
			if bucket.Sent != tc.wantSent || bucket.LossCount != tc.wantLost || math.Abs(bucket.LossPct-tc.wantLoss) > 1e-9 {
				t.Fatalf("bucket = %+v; want sent/lost/loss %d/%d/%g", bucket, tc.wantSent, tc.wantLost, tc.wantLoss)
			}
			if loss.Sent != bucket.Sent || loss.LossCount != bucket.LossCount || loss.LossPct != bucket.LossPct {
				t.Fatalf("timeline %+v disagrees with bucket %+v", loss, bucket)
			}
			worstAt := start.Add(time.Duration(len(tc.cycles)) * time.Second)
			if !loss.WorstTime.Equal(worstAt) {
				t.Fatalf("worst cycle = %s, want normalized full-loss cycle %s", loss.WorstTime, worstAt)
			}
			if summary.HasRTT != tc.wantRTT || summary.LossAvg != tc.wantOverviewAvg || summary.LossMax != 100 {
				t.Fatalf("overview = %+v; want HasRTT=%v, loss avg/max=%g/100", summary, tc.wantRTT, tc.wantOverviewAvg)
			}
			wantLatency := 0.0
			if tc.wantRTT {
				wantLatency = 100
			}
			for _, latency := range []float64{bucket.Min, bucket.Max, bucket.Mean, bucket.Median, bucket.P5, bucket.P95, summary.RTTMedian, summary.RTTP95, summary.RTTMax} {
				if latency != wantLatency {
					t.Fatalf("latency = %g, want %gms", latency, wantLatency)
				}
			}
			for slot, latency := range summary.Sparkline {
				if slot == 0 && tc.wantRTT {
					if latency == nil || *latency != 100 {
						t.Fatalf("measured spark slot = %v, want 100ms", latency)
					}
				} else if latency != nil {
					t.Fatalf("unmeasured spark slot %d = %g, want nil", slot, *latency)
				}
			}
		})
	}
}
