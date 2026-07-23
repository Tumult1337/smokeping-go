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
// path; debounce must never block it.
func TestDebounceNeverBlocksWhenOutIsFull(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	in := make(chan struct{}, 64)
	out := make(chan struct{}) // unbuffered, nobody reading
	go debounce(ctx, in, out, 10*time.Millisecond)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			in <- struct{}{}
			time.Sleep(time.Millisecond)
		}
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("debounce blocked its producer")
	}
}
