//go:build integration

package clickhouse

import (
	"context"
	"log/slog"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/probe"
	"github.com/tumult/gosmokeping/internal/scheduler"
	"github.com/tumult/gosmokeping/internal/storage"
)

// newSeededReader bootstraps a fresh database, writes one cycle per supplied
// instant, and returns a reader over it. Each cycle carries `weight` pings,
// `weight` hop TTLs and `weight` HTTP samples, so a per-table count names
// exactly which instants a window admitted.
func newSeededReader(t *testing.T, ref config.TargetRef, at map[time.Time]int) *Reader {
	t.Helper()
	cfg, cleanup := testDSN(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := Bootstrap(ctx, log, cfg); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	w, err := NewWriter(ctx, log, cfg, 10)
	if err != nil {
		t.Fatalf("writer: %v", err)
	}
	for ts, weight := range at {
		c := scheduler.Cycle{
			Time: ts, Target: ref, ProbeName: "mtr", Source: "master", Sent: weight,
		}
		for i := 0; i < weight; i++ {
			c.RTTs = append(c.RTTs, time.Duration(i+1)*time.Millisecond)
			c.Hops = append(c.Hops, probe.Hop{
				Index: i + 1, IP: "10.0.0.1", Sent: 1,
				RTTs: []time.Duration{time.Millisecond},
			})
			c.HTTPSamples = append(c.HTTPSamples, probe.HTTPSample{
				Time: ts, RTT: time.Millisecond, Status: 200,
			})
		}
		w.OnCycle(ctx, c)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("writer close: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	r, err := NewReader(ctx, cfg)
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// Every read scoping a DateTime64(3) column by [from, to) must honour the
// milliseconds it was given: a bound rendered at whole-second precision moves
// each edge by up to a second, admitting a row before `from` and dropping one
// before `to`. The `atFrom` / `atTo` rows pin the half-open contract itself.
func TestIntegrationWindowEdgesAreMillisecondExact(t *testing.T) {
	ctx := context.Background()
	ref := config.TargetRef{Target: config.Target{Name: "edge"}, Group: "g"}

	base := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	from := base.Add(900 * time.Millisecond)
	to := from.Add(time.Second)
	below := base.Add(500 * time.Millisecond)   // before `from`, inside its truncation
	inside := base.Add(1500 * time.Millisecond) // before `to`, outside its truncation

	r := newSeededReader(t, ref, map[time.Time]int{below: 1, from: 2, inside: 4, to: 8})

	t.Run("cycles raw", func(t *testing.T) {
		pts, err := r.QueryCycles(ctx, ref, from, to, storage.QueryFilter{})
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		var got []time.Time
		for _, p := range pts {
			got = append(got, p.Time.UTC())
		}
		sort.Slice(got, func(i, j int) bool { return got[i].Before(got[j]) })
		want := []time.Time{from, inside}
		if len(got) != len(want) {
			t.Fatalf("times %v, want %v", got, want)
		}
		for i := range want {
			if !got[i].Equal(want[i]) {
				t.Fatalf("times %v, want %v", got, want)
			}
		}
	})

	t.Run("cycles bucketed", func(t *testing.T) {
		pts, err := r.QueryCycles(ctx, ref, from, to, storage.QueryFilter{Step: time.Hour})
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		var sent int64
		for _, p := range pts {
			sent += p.Sent
		}
		if sent != 6 {
			t.Fatalf("bucketed sent = %d, want 6 (the 2- and 4-ping cycles)", sent)
		}
	})

	t.Run("rtts", func(t *testing.T) {
		pts, err := r.QueryRTTs(ctx, ref, from, to, storage.QueryFilter{})
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if len(pts) != 6 {
			t.Fatalf("rtt rows = %d, want 6", len(pts))
		}
	})

	t.Run("http", func(t *testing.T) {
		pts, err := r.QueryHTTPSamples(ctx, ref, from, to, storage.QueryFilter{})
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if len(pts) != 6 {
			t.Fatalf("http rows = %d, want 6", len(pts))
		}
	})

	t.Run("hops timeline", func(t *testing.T) {
		res, err := r.QueryHopsTimeline(ctx, ref, from, to,
			storage.QueryFilter{Source: "master", Step: time.Hour})
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		var ttls []int64
		for _, p := range res.Hops {
			ttls = append(ttls, p.Index)
		}
		sort.Slice(ttls, func(i, j int) bool { return ttls[i] < ttls[j] })
		if len(ttls) != 4 || ttls[3] != 4 {
			t.Fatalf("grid ttls = %v, want the 4 of the 2- and 4-hop cycles", ttls)
		}
	})

	t.Run("overview", func(t *testing.T) {
		rows, err := r.QueryOverview(ctx, from, to, []config.TargetRef{ref})
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("overview rows = %d, want 1", len(rows))
		}
		if !rows[0].LastSeen.UTC().Equal(inside) {
			t.Fatalf("last_seen = %s, want %s", rows[0].LastSeen.UTC(), inside)
		}
	})
}

// QueryHopsAt derives its own window from `at` ± window/2, so both edges carry
// the centre's milliseconds. Under a second-truncated window the far cycle
// falls out and the near-but-excluded one becomes argMin's only candidate.
func TestIntegrationHopsAtWindowEdgesAreMillisecondExact(t *testing.T) {
	ctx := context.Background()
	ref := config.TargetRef{Target: config.Target{Name: "pin"}, Group: "g"}

	base := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	at := base.Add(900 * time.Millisecond)
	const window = 30 * time.Minute
	// Just inside the exclusive upper edge, and just outside the inclusive
	// lower one. Truncating both to the second swaps which is eligible.
	eligible := at.Add(window/2 - 400*time.Millisecond)
	excluded := at.Add(-window/2 - 400*time.Millisecond)

	r := newSeededReader(t, ref, map[time.Time]int{eligible: 1, excluded: 1})

	res, err := r.QueryHopsAt(ctx, ref, at, window, storage.QueryFilter{})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(res.Hops) != 1 {
		t.Fatalf("hops = %d rows, want the 1 of the eligible cycle: %+v", len(res.Hops), res.Hops)
	}
	if got := res.Hops[0].Time.UTC(); !got.Equal(eligible) {
		t.Fatalf("pinned %s, want %s (the cycle inside [at-15m, at+15m))", got, eligible)
	}
}

// The half-open contract of that same window: a cycle exactly at at-window/2
// is eligible, one exactly at at+window/2 is not.
func TestIntegrationHopsAtWindowIsHalfOpen(t *testing.T) {
	ctx := context.Background()
	const window = 30 * time.Minute

	base := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	at := base.Add(900 * time.Millisecond)

	lowRef := config.TargetRef{Target: config.Target{Name: "low"}, Group: "g"}
	low := newSeededReader(t, lowRef, map[time.Time]int{at.Add(-window / 2): 1})
	res, err := low.QueryHopsAt(ctx, lowRef, at, window, storage.QueryFilter{})
	if err != nil {
		t.Fatalf("low query: %v", err)
	}
	if len(res.Hops) != 1 {
		t.Fatalf("inclusive lower edge returned %d hop rows, want 1", len(res.Hops))
	}

	highRef := config.TargetRef{Target: config.Target{Name: "high"}, Group: "g"}
	high := newSeededReader(t, highRef, map[time.Time]int{at.Add(window / 2): 1})
	res, err = high.QueryHopsAt(ctx, highRef, at, window, storage.QueryFilter{})
	if err != nil {
		t.Fatalf("high query: %v", err)
	}
	if len(res.Hops) != 0 {
		t.Fatalf("exclusive upper edge returned %d hop rows, want 0: %+v", len(res.Hops), res.Hops)
	}
}

// QueryLatestHops' freshness floor is the same kind of bound. The CachingReader
// quantizes it to a whole minute before the reader sees it, so only a direct
// caller can observe the truncation — the floor is still bound the same way.
func TestIntegrationLatestSinceFloorIsMillisecondExact(t *testing.T) {
	ctx := context.Background()
	ref := config.TargetRef{Target: config.Target{Name: "fresh"}, Group: "g"}

	base := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	stale := base.Add(500 * time.Millisecond)
	r := newSeededReader(t, ref, map[time.Time]int{stale: 1})

	res, err := r.QueryLatestHops(ctx, ref, storage.QueryFilter{
		LatestSince: base.Add(900 * time.Millisecond),
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(res.Hops) != 0 {
		t.Fatalf("a row 400ms below the floor survived it: %+v", res.Hops)
	}
}

// The API's query-time bound is derived from the DateTime64(3) domain rather
// than picked, so this pins the derivation against the server that defines it:
// every instant the bound admits round-trips exactly, which is the property
// that keeps a query from addressing a different moment than it asked for.
//
// It deliberately does *not* require the bound to equal the server's domain.
// ClickHouse widened DateTime64 past [1900, 2299] — verified against 26.7.5.10,
// where both edges plus one millisecond round-trip intact — and an earlier
// version of this test read that as a failure, so a current server reddened a
// build over a bound that had only become conservative. Narrower than the
// server is safe and stays safe; the direction that would break us is the
// server narrowing below us, which the round-trip loop catches.
func TestIntegrationQueryTimeRangeMatchesClickHouse(t *testing.T) {
	cfg, cleanup := testDSN(t)
	defer cleanup()
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := Bootstrap(ctx, log, cfg); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	r, err := NewReader(ctx, cfg)
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	defer r.Close()

	roundTrip := func(ms int64) string {
		t.Helper()
		var s string
		q := "SELECT toString(" + dtMilli + ")"
		if err := r.conn.QueryRow(ctx, q, ms).Scan(&s); err != nil {
			t.Fatalf("round trip %d: %v", ms, err)
		}
		return s
	}
	for _, tc := range []struct {
		name string
		at   time.Time
		want string
	}{
		{"min", storage.MinQueryTime, "1900-01-01 00:00:00.000"},
		{"max", storage.MaxQueryTime, "2299-12-31 23:59:59.999"},
	} {
		if got := roundTrip(tc.at.UnixMilli()); got != tc.want {
			t.Errorf("%s: %s round-tripped as %s, want %s", tc.name, tc.at.UTC(), got, tc.want)
		}
	}
	// One millisecond outside each edge: the contract under test is that our own
	// validator refuses it, never that the server does. Whether the server also
	// mangles it is recorded, not asserted — that is the part that drifts.
	for _, tc := range []struct {
		name string
		at   time.Time
	}{
		{"below min", storage.MinQueryTime.Add(-time.Millisecond)},
		{"above max", storage.MaxQueryTime.Add(time.Millisecond)},
	} {
		if storage.ValidQueryTime(tc.at) {
			t.Errorf("%s: ValidQueryTime admitted %s, which is outside the bound it enforces",
				tc.name, tc.at.UTC())
		}
		got := roundTrip(tc.at.UnixMilli())
		if want := tc.at.UTC().Format("2006-01-02 15:04:05.000"); got == want {
			t.Logf("%s: server round-tripped %s intact, so its domain is wider than the bound — conservative, not a failure",
				tc.name, want)
		}
	}
}
