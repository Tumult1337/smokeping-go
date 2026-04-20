package influxv2

import (
	"testing"
	"time"
)

func TestFormatEvery(t *testing.T) {
	cases := map[time.Duration]string{
		time.Hour:        "1h",
		6 * time.Hour:    "6h",
		24 * time.Hour:   "1d",
		48 * time.Hour:   "2d",
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
