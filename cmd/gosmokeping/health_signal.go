package main

import (
	"context"
	"time"
)

// healthSignalDelay coalesces registry churn into a single scheduler rebuild.
// A fleet restart touches every slave within a few seconds, and each touch
// that changes the mesh fires a change callback; rebuilding per slave would
// tear down and restart the probe loop N times in a row.
const healthSignalDelay = 5 * time.Second

// debounce forwards bursts on in to a single send on out, delay after the
// burst goes quiet.
//
// Both ends are non-blocking by construction: the producer is a registry
// callback running on an HTTP request path, and the consumer is a scheduler
// signal channel that may already hold a pending wakeup. A dropped send is
// correct — out carries "something changed", not a count, and the lifecycle
// re-reads authoritative state when it wakes.
func debounce(ctx context.Context, in <-chan struct{}, out chan<- struct{}, delay time.Duration) {
	timer := time.NewTimer(delay)
	if !timer.Stop() {
		<-timer.C
	}
	armed := false
	for {
		select {
		case <-ctx.Done():
			if armed && !timer.Stop() {
				<-timer.C
			}
			return
		case <-in:
			if armed && !timer.Stop() {
				<-timer.C
			}
			timer.Reset(delay)
			armed = true
		case <-timer.C:
			armed = false
			select {
			case out <- struct{}{}:
			default:
			}
		}
	}
}
