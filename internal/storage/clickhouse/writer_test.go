package clickhouse

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/scheduler"
)

// TestWriterDropsOnFullChannel asserts that an over-saturated OnCycle
// call returns without blocking and bumps the drop counter.
func TestWriterDropsOnFullChannel(t *testing.T) {
	w := newTestWriter(t, 1) // tiny channel buffer
	defer w.Close()

	// Fill the channel; subsequent sends should be dropped (non-blocking).
	for i := 0; i < 10; i++ {
		w.OnCycle(context.Background(), testCycle(time.Now()))
	}
	got := atomic.LoadUint64(&w.dropped[tableProbeCycle])
	if got == 0 {
		t.Fatal("expected dropped > 0")
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
