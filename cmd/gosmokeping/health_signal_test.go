package main

import (
	"context"
	"testing"
	"time"
)

// A fleet restart touches every slave at once. Each touch fires OnChange, and
// without coalescing the master would rebuild its scheduler once per slave.
func TestDebounceCoalescesBursts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	in := make(chan struct{}, 64)
	out := make(chan struct{}, 8)
	go debounce(ctx, in, out, 40*time.Millisecond)

	for i := 0; i < 25; i++ {
		in <- struct{}{}
	}

	select {
	case <-out:
	case <-time.After(2 * time.Second):
		t.Fatal("no signal emitted")
	}

	select {
	case <-out:
		t.Fatal("burst produced more than one signal")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestDebounceEmitsAgainAfterQuiet(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	in := make(chan struct{}, 8)
	out := make(chan struct{}, 8)
	go debounce(ctx, in, out, 30*time.Millisecond)

	for round := 0; round < 2; round++ {
		in <- struct{}{}
		select {
		case <-out:
		case <-time.After(2 * time.Second):
			t.Fatalf("round %d: no signal emitted", round)
		}
	}
}

// The producer is a registry callback holding no lock but running on a request
// path; debounce must never block it. in's buffer is capacity 1, and each
// send waits longer than delay, so the timer fires (attempting the out send)
// before the next input arrives — if that out send weren't non-blocking, the
// debounce goroutine would wedge there, stop draining in, and the producer's
// next-but-one send would block on the full buffer. Spacing sends faster than
// delay (as the original version did) can't catch this: a trailing-edge
// timer never fires — and so never reaches the out send — while inputs keep
// arriving inside the delay window.
func TestDebounceNeverBlocksWhenOutIsFull(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const delay = 10 * time.Millisecond
	in := make(chan struct{}, 1)
	out := make(chan struct{}) // unbuffered, nobody reading
	go debounce(ctx, in, out, delay)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			in <- struct{}{}
			time.Sleep(3 * delay)
		}
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("debounce blocked its producer")
	}
}

// TestDebounceCoalescesBursts sends with no gaps, so it can't distinguish
// trailing-edge debounce (timer reset per input) from a leading-edge
// fixed-window throttle armed once. This test pins the trailing-edge
// contract: a trickle spaced under the delay must suppress output for as
// long as it continues, and only the delay after it stops should a signal
// land.
func TestDebounceResetsOnEachInputDuringTrickle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const delay = 50 * time.Millisecond
	in := make(chan struct{}, 8)
	out := make(chan struct{}, 8)
	go debounce(ctx, in, out, delay)

	// Trickle for longer than delay, each gap under delay. A fixed-window
	// throttle armed once at the first send would fire mid-trickle; a
	// trailing-edge debounce keeps resetting and stays silent throughout.
	trickleFor := 4 * delay
	tick := delay / 3
	deadline := time.Now().Add(trickleFor)
	for time.Now().Before(deadline) {
		in <- struct{}{}
		select {
		case <-out:
			t.Fatal("signal emitted while trickle still active")
		case <-time.After(tick):
		}
	}

	select {
	case <-out:
	case <-time.After(2 * time.Second):
		t.Fatal("no signal emitted after trickle stopped")
	}
}
