package clickhouse

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/scheduler"
)

// TestOfferDropsWhenChannelFull asserts the offer primitive drops rather
// than blocks when the per-table channel has no space. This is the in-
// memory channel-saturation case; the post-Close case is covered by
// TestOfferDropsAfterClose below.
func TestOfferDropsWhenChannelFull(t *testing.T) {
	w := newTestWriter(t, 1) // tiny channel buffer, no consumer goroutines
	defer w.Close() //nolint:errcheck // test cleanup

	// First send fills the channel; the rest must be dropped immediately.
	for i := 0; i < 10; i++ {
		w.OnCycle(context.Background(), testCycle(time.Now()))
	}
	if got := atomic.LoadUint64(&w.dropped[tableProbeCycle]); got != 9 {
		t.Fatalf("expected exactly 9 drops (buffer=1, sends=10), got %d", got)
	}
}

// TestOfferDropsAfterClose proves the writer's Close contract: once Close
// has returned, further OnCycle calls increment the drop counter rather
// than queueing into a channel with no reader. Regression guard for a
// silent-data-loss bug — the original implementation only checked channel
// fullness, which made post-Close sends succeed (into the buffer) and
// vanish on GC with no observability.
func TestOfferDropsAfterClose(t *testing.T) {
	w := newTestWriter(t, 16) // generous buffer; we're testing the closed flag, not fullness
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	before := atomic.LoadUint64(&w.dropped[tableProbeCycle])
	for i := 0; i < 5; i++ {
		w.OnCycle(context.Background(), testCycle(time.Now()))
	}
	after := atomic.LoadUint64(&w.dropped[tableProbeCycle])
	if after-before != 5 {
		t.Fatalf("expected 5 post-Close drops, got %d", after-before)
	}
}

// TestCloseIsIdempotent guards against double-close panics. The cmd-side
// composition root may call Close from multiple defers under unusual
// shutdown paths.
func TestCloseIsIdempotent(t *testing.T) {
	w := newTestWriter(t, 1)
	if err := w.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

// TestDroppedReports proves the Dropped() accessor returns a per-table
// snapshot suitable for surfacing via /api/v1/health or a metric.
func TestDroppedReports(t *testing.T) {
	w := newTestWriter(t, 0) // zero-buffer => every send drops
	defer w.Close() //nolint:errcheck // test cleanup

	w.OnCycle(context.Background(), testCycle(time.Now()))
	w.OnCycle(context.Background(), testCycle(time.Now()))
	got := w.Dropped()
	if got["probe_cycle"] != 2 {
		t.Fatalf("probe_cycle dropped = %d, want 2 (got %+v)", got["probe_cycle"], got)
	}
	if got["probe_rtt"] != 0 {
		t.Errorf("probe_rtt dropped = %d, want 0", got["probe_rtt"])
	}
}

// Test helpers used only by writer_test.go; kept here so they don't ship
// in the binary.

func newTestWriter(_ *testing.T, bufSize int) *Writer {
	w := &Writer{}
	for i := range w.chans {
		w.chans[i] = make(chan any, bufSize)
	}
	return w
}

func testCycle(at time.Time) scheduler.Cycle {
	return scheduler.Cycle{Time: at}
}
