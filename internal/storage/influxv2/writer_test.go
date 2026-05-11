package influxv2

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/scheduler"
)

func TestFormatEvery(t *testing.T) {
	cases := map[time.Duration]string{
		time.Hour:        "1h",
		6 * time.Hour:    "6h",
		24 * time.Hour:   "1d",
		48 * time.Hour:   "2d",
		5 * time.Minute:  "5m",
		30 * time.Minute: "30m",
		45 * time.Second: "45s",
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

// TestOnCycleNonBlockingDropsOldest is the regression test for the
// scheduler-stalls-when-influx-is-sick incident. OnCycle MUST NOT block
// even when the internal queue saturates: instead the oldest queued cycle
// is dropped and the freshest cycle takes its place.
func TestOnCycleNonBlockingDropsOldest(t *testing.T) {
	// Build a Writer skeleton without starting the flushLoop or touching
	// the v2 client, so the queue cannot drain on its own.
	const cap = 4
	w := &Writer{
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:   config.InfluxV2{},
		queue: make(chan scheduler.Cycle, cap),
		stop:  make(chan struct{}),
	}

	const total = 100
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range total {
			w.OnCycle(context.Background(), scheduler.Cycle{Time: time.Unix(int64(i), 0)})
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("OnCycle blocked despite a saturated queue")
	}

	if got := w.dropped.Load(); got != int64(total-cap) {
		t.Errorf("dropped count: got %d, want %d", got, total-cap)
	}
	if got := len(w.queue); got != cap {
		t.Fatalf("queue length after fill: got %d, want %d", got, cap)
	}
	// FIFO + drop-oldest means the survivors are the *last* `cap` cycles.
	for i := total - cap; i < total; i++ {
		c := <-w.queue
		if got := c.Time.Unix(); got != int64(i) {
			t.Errorf("queue order: got cycle ts=%d, want %d", got, i)
		}
	}
}
