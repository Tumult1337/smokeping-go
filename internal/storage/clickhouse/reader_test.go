package clickhouse

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
		{24 * time.Hour, 0},                    // raw (boundary)
		{25 * time.Hour, time.Hour},            // 1h tier
		{180 * 24 * time.Hour, time.Hour},      // 1h (boundary)
		{181 * 24 * time.Hour, 24 * time.Hour}, // 1d tier
	}
	for _, c := range cases {
		if got := pickCycleStep(c.span); got != c.want {
			t.Errorf("span=%v: got %v, want %v", c.span, got, c.want)
		}
	}
}

func TestPickHopStep(t *testing.T) {
	cases := []struct {
		span time.Duration
		want time.Duration
	}{
		{time.Hour, 0},
		{24 * time.Hour, 0},
		{25 * time.Hour, 15 * time.Minute},
	}
	for _, c := range cases {
		if got := pickHopStep(c.span); got != c.want {
			t.Errorf("span=%v: got %v, want %v", c.span, got, c.want)
		}
	}
}
