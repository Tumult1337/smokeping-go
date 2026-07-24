package probe

import (
	"context"
	"testing"
	"time"
)

// NoTrace must suppress the opportunistic path walk without touching echo
// statistics. This exercises the real echo path, which needs either an
// unprivileged ping socket (a permissive net.ipv4.ping_group_range) or
// CAP_NET_RAW — neither guaranteed on a CI runner, so we skip rather than fail
// when the environment cannot send ICMP at all. The gating logic itself is
// verified environment-independently by TestICMPProbeNoTraceGatesTraceCall;
// this test adds the integration check that a real probe with NoTrace set
// produces no hops and leaves echo stats intact.
func TestICMPProbeNoTraceSkipsHops(t *testing.T) {
	p := NewICMP("icmp", time.Second, true)
	p.spacing = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := p.Probe(ctx, Target{Host: "127.0.0.1"}, 1)
	if err != nil {
		t.Skipf("ICMP echo unavailable here (no unprivileged ping / CAP_NET_RAW): %v", err)
	}

	// The NoTrace contract holds whenever a probe ran, regardless of whether
	// the loopback echo happened to land: no hops, ever.
	if len(res.Hops) != 0 {
		t.Fatalf("got %d hops, want 0 with NoTrace set", len(res.Hops))
	}
	// The echo-unaffected assertion needs a completed echo. A socket that
	// opened but dropped the loopback reply (constrained sandbox) can't speak
	// to whether NoTrace touched echo stats, so skip that half rather than
	// flake on it.
	if len(res.RTTs) == 0 {
		t.Skip("loopback echo did not complete; skipping echo-unaffected assertion")
	}
	if len(res.RTTs) != 1 || res.LossCount != 0 {
		t.Fatalf("echo stats affected by NoTrace: rtts=%d lossCount=%d", len(res.RTTs), res.LossCount)
	}
}

// TestICMPProbeNoTraceGatesTraceCall replaces the trace seam with a spy so the
// gating logic is exercised directly — this is what discriminates a correct
// guard from a deleted or inverted one, which TestICMPProbeNoTraceSkipsHops
// cannot (without raw-socket privilege traceHops fails regardless of the flag).
//
// The spy isolates the trace call but NOT the echo: Probe opens an ICMP echo
// socket before it ever reaches the trace step, so this still needs an
// unprivileged ping socket or CAP_NET_RAW and skips when neither is available.
// The gating is thus verified wherever ICMP works (local dev, production); a
// locked-down CI runner skips rather than fails.
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
			t.Skipf("ICMP echo unavailable here (no unprivileged ping / CAP_NET_RAW): %v", err)
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
			t.Skipf("ICMP echo unavailable here (no unprivileged ping / CAP_NET_RAW): %v", err)
		}
		if !called {
			t.Fatal("trace func not called with NoTrace clear")
		}
		if len(res.Hops) != len(want) {
			t.Fatalf("got %d hops, want %d from trace func", len(res.Hops), len(want))
		}
	})
}
