package storage

import (
	"testing"
	"time"
)

func TestPickCycleStep(t *testing.T) {
	cases := []struct {
		span time.Duration
		want time.Duration
	}{
		{time.Hour, 0},                         // raw
		{2 * time.Hour, 0},                     // raw (boundary)
		{3 * time.Hour, 2 * time.Minute},       // 2m tier
		{24 * time.Hour, 2 * time.Minute},      // 2m (boundary)
		{25 * time.Hour, 15 * time.Minute},     // 15m tier
		{7 * 24 * time.Hour, 15 * time.Minute}, // 15m (boundary)
		{8 * 24 * time.Hour, time.Hour},        // 1h tier
		{30 * 24 * time.Hour, time.Hour},       // 1h (boundary)
		{31 * 24 * time.Hour, 6 * time.Hour},   // 6h tier
		{180 * 24 * time.Hour, 6 * time.Hour},  // 6h (boundary)
		{181 * 24 * time.Hour, 24 * time.Hour}, // 1d tier
	}
	for _, c := range cases {
		if got := PickCycleStep(c.span); got != c.want {
			t.Errorf("span=%v: got %v, want %v", c.span, got, c.want)
		}
	}
}

func TestPickHopStep(t *testing.T) {
	cases := []struct {
		span     time.Duration
		interval time.Duration
		want     time.Duration
	}{
		{time.Hour, time.Second, MinHopStep},                    // finest tier
		{2 * time.Hour, time.Second, MinHopStep},                // finest tier (boundary)
		{time.Hour, 30 * time.Millisecond, MinHopStep},          // the one-hop mtr rate that overran the raw cap
		{time.Hour, 20 * time.Second, 20 * time.Second},         // never finer than the cadence
		{2 * time.Hour, 5 * time.Minute, 5 * time.Minute},       // the default interval
		{time.Hour, 20500 * time.Millisecond, 20 * time.Second}, // whole seconds, the unit the query renders
		{3 * time.Hour, time.Second, 5 * time.Minute},           // 5m tier
		{24 * time.Hour, time.Second, 5 * time.Minute},
		{24 * time.Hour, 10 * time.Minute, 10 * time.Minute},
		{25 * time.Hour, time.Second, 15 * time.Minute},
		{7 * 24 * time.Hour, time.Second, 15 * time.Minute},
		{7 * 24 * time.Hour, time.Hour, time.Hour},
	}
	for _, c := range cases {
		if got := PickHopStep(c.span, c.interval); got != c.want {
			t.Errorf("span=%v interval=%v: got %v, want %v", c.span, c.interval, got, c.want)
		}
	}
}

// A zero step is a raw grid, and a raw grid's row count is the producer's
// cycle rate — the one factor neither config nor the schema bounds.
func TestPickHopStepNeverReturnsARawGrid(t *testing.T) {
	intervals := []time.Duration{0, time.Millisecond, 30 * time.Millisecond, time.Second, 20 * time.Second, time.Minute, 71 * time.Minute}
	for span := time.Minute; span <= MaxHopTimelineWindow; span += 7 * time.Minute {
		for _, interval := range intervals {
			if got := PickHopStep(span, interval); got < MinHopStep {
				t.Fatalf("span=%v interval=%v: step %v under the %v floor", span, interval, got, MinHopStep)
			}
		}
	}
}
