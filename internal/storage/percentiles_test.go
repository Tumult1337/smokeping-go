package storage

import (
	"testing"

	"github.com/tumult/gosmokeping/internal/stats"
)

// TestPercentileAccessorsMatchStats guarantees that adding or renaming a
// percentile in stats.PercentileSet without updating this package fails at
// test time instead of silently dropping rollup data. The writer and reader
// both iterate these two slices in lock-step order.
func TestPercentileAccessorsMatchStats(t *testing.T) {
	if len(CyclePointPercentileAccessors) != len(stats.PercentileSet) {
		t.Fatalf("len mismatch: stats.PercentileSet=%d CyclePointPercentileAccessors=%d",
			len(stats.PercentileSet), len(CyclePointPercentileAccessors))
	}
	for i, spec := range stats.PercentileSet {
		acc := CyclePointPercentileAccessors[i]
		if acc.Name != spec.Name {
			t.Errorf("index %d: stats name %q != cycle-point name %q", i, spec.Name, acc.Name)
		}
	}
}

func TestCyclePointAccessorsRoundTrip(t *testing.T) {
	var cp CyclePoint
	for i, acc := range CyclePointPercentileAccessors {
		acc.Set(&cp, float64(i+1))
	}
	for i, acc := range CyclePointPercentileAccessors {
		if got := acc.Get(cp); got != float64(i+1) {
			t.Errorf("%s: got %v, want %v", acc.Name, got, i+1)
		}
	}
}
