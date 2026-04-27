package storage

import (
	"testing"
	"time"
)

func TestPickResolution(t *testing.T) {
	now := time.Now()
	cases := []struct {
		span time.Duration
		want Resolution
	}{
		{1 * time.Hour, ResolutionRaw},
		{24 * time.Hour, ResolutionRaw},
		{25 * time.Hour, Resolution1h},
		{7 * 24 * time.Hour, Resolution1h},
		{30 * 24 * time.Hour, Resolution1h},
		{180 * 24 * time.Hour, Resolution1h},
		{365 * 24 * time.Hour, Resolution1d},
	}
	for _, c := range cases {
		got := PickResolution(now.Add(-c.span), now)
		if got != c.want {
			t.Errorf("span=%s: got %q want %q", c.span, got, c.want)
		}
	}
}

func TestBucketForHops(t *testing.T) {
	// Heatmap canvas is ~666 px wide; we want at most ~1 cell per pixel so the
	// hops/timeline payload tracks what the user can actually see. Tiers:
	//   span ≤ 6h:           raw (0)         — fine detail at narrow zoom
	//   6h < span ≤ 24h:     1m  buckets     — 1440 cells max
	//   24h < span ≤ 7d:     15m buckets     — 672 cells max for 7d
	cases := []struct {
		name string
		span time.Duration
		want time.Duration
	}{
		{"1h raw", 1 * time.Hour, 0},
		{"6h raw", 6 * time.Hour, 0},
		{"6h+1s 1m", 6*time.Hour + time.Second, time.Minute},
		{"24h 1m", 24 * time.Hour, time.Minute},
		{"24h+1s 15m", 24*time.Hour + time.Second, 15 * time.Minute},
		{"7d 15m", 7 * 24 * time.Hour, 15 * time.Minute},
		// Beyond 7d isn't reachable today (API caps timeline at 7d) but the
		// helper still has to return something sane — fall back to 15m so a
		// future caller doesn't accidentally request raw.
		{"30d falls back to 15m", 30 * 24 * time.Hour, 15 * time.Minute},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := BucketForHops(c.span)
			if got != c.want {
				t.Errorf("span=%s: got %s want %s", c.span, got, c.want)
			}
		})
	}
}
