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
		{7 * 24 * time.Hour, ResolutionRaw},
		{8 * 24 * time.Hour, Resolution1h},
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
