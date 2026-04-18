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

func TestFormatEvery(t *testing.T) {
	cases := map[time.Duration]string{
		time.Hour:       "1h",
		6 * time.Hour:   "6h",
		24 * time.Hour:  "1d",
		48 * time.Hour:  "2d",
		30 * time.Minute: "1800s",
	}
	for d, want := range cases {
		if got := formatEvery(d); got != want {
			t.Errorf("formatEvery(%s): got %q want %q", d, got, want)
		}
	}
}

func TestMs(t *testing.T) {
	if got := ms(1500 * time.Microsecond); got != 1.5 {
		t.Errorf("ms(1500us) = %v, want 1.5", got)
	}
	if got := ms(0); got != 0 {
		t.Errorf("ms(0) = %v, want 0", got)
	}
}
