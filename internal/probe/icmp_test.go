package probe

import (
	"context"
	"testing"
	"time"
)

// NoTrace must suppress the opportunistic path walk without touching echo
// statistics. The trace itself needs a raw socket (CAP_NET_RAW), which this
// suite cannot assume is available, so we only assert the direction that
// holds regardless of environment: with NoTrace set, Hops is always empty
// and the echo RTTs are unaffected. We do not assert the converse (NoTrace
// false ⇒ Hops populated) — that depends on raw-socket privilege the test
// process may not have, and asserting it would make the test flaky rather
// than discriminating.
func TestICMPProbeNoTraceSkipsHops(t *testing.T) {
	p := NewICMP("icmp", time.Second, true)
	p.spacing = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := p.Probe(ctx, Target{Host: "127.0.0.1"}, 1)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(res.RTTs) != 1 || res.LossCount != 0 {
		t.Fatalf("echo stats affected by NoTrace: rtts=%d lossCount=%d", len(res.RTTs), res.LossCount)
	}
	if len(res.Hops) != 0 {
		t.Fatalf("got %d hops, want 0 with NoTrace set", len(res.Hops))
	}
}

// TestICMPProbeNoTraceGatesTraceCall replaces the trace seam with a spy so
// the gating logic is exercised directly, without depending on CAP_NET_RAW.
// This is what actually discriminates a correct guard from a deleted or
// inverted one: TestICMPProbeNoTraceSkipsHops alone cannot, because without
// raw-socket privilege traceHops fails regardless of the flag and Hops ends
// up empty either way.
func TestICMPProbeNoTraceGatesTraceCall(t *testing.T) {
	t.Run("NoTrace set: trace is not called", func(t *testing.T) {
		p := NewICMP("icmp", time.Second, true)
		p.spacing = time.Millisecond
		called := false
		p.trace = func(ctx context.Context, host, family string, rounds, maxTTL int, timeout, spacing time.Duration) ([]Hop, bool, error) {
			called = true
			return []Hop{{Index: 1, IP: "10.0.0.1"}}, true, nil
		}

		res, err := p.Probe(context.Background(), Target{Host: "127.0.0.1"}, 1)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if called {
			t.Fatal("trace func called despite NoTrace set")
		}
		if len(res.Hops) != 0 {
			t.Fatalf("got %d hops, want 0 with NoTrace set", len(res.Hops))
		}
	})

	t.Run("NoTrace clear: trace is called and its hops flow through", func(t *testing.T) {
		p := NewICMP("icmp", time.Second, false)
		p.spacing = time.Millisecond
		called := false
		want := []Hop{{Index: 1, IP: "10.0.0.1"}, {Index: 2, IP: "10.0.0.2"}}
		p.trace = func(ctx context.Context, host, family string, rounds, maxTTL int, timeout, spacing time.Duration) ([]Hop, bool, error) {
			called = true
			return want, true, nil
		}

		res, err := p.Probe(context.Background(), Target{Host: "127.0.0.1"}, 1)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if !called {
			t.Fatal("trace func not called with NoTrace clear")
		}
		if len(res.Hops) != len(want) {
			t.Fatalf("got %d hops, want %d from trace func", len(res.Hops), len(want))
		}
	})
}
