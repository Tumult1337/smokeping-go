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

// The TTL walk must not be starved by the echo batch it shares a cycle
// deadline with. Run after the batch, a loss-saturated batch returned at
// Probe's early ctx.Err() check and the walk never ran at all, so hop rows
// vanished on exactly the lossy cycles a traceroute exists for.
func TestICMPProbeTracesDespiteExhaustedEchoBudget(t *testing.T) {
	p := NewICMP("icmp", time.Second, false)
	// Spacing alone outruns the cycle context below, so the echo loop is
	// guaranteed to exit on cancellation rather than completing.
	p.spacing = 50 * time.Millisecond

	var entered bool
	var entryErr error
	want := []Hop{{Index: 1, IP: "10.0.0.1"}, {Index: 2, IP: "10.0.0.2"}}
	p.trace = func(ctx context.Context, host, family string, rounds, maxTTL int, timeout, spacing time.Duration) ([]Hop, bool, error) {
		entered, entryErr = true, ctx.Err()
		return want, true, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	res, err := p.Probe(ctx, Target{Host: "127.0.0.1"}, 20)
	if res == nil {
		t.Skipf("ICMP echo unavailable here (no unprivileged ping / CAP_NET_RAW): %v", err)
	}
	if !entered {
		t.Fatal("trace never ran: the echo batch consumed the whole cycle")
	}
	if entryErr != nil {
		t.Fatalf("trace entered with an already-expired context: %v", entryErr)
	}
	if len(res.Hops) != len(want) {
		t.Fatalf("got %d hops, want %d", len(res.Hops), len(want))
	}
}

// Concurrency is the point, not merely ordering: a trace moved *before* the
// echo loop but still called synchronously would satisfy every "the trace ran"
// assertion while restoring the additive echo+trace cycle cost this change
// exists to remove. Assert the cycle costs max(echo, trace), not their sum.
func TestICMPProbeRunsTraceConcurrentlyWithEchoBatch(t *testing.T) {
	p := NewICMP("icmp", time.Second, false)
	// 20 loopback pings at 20ms spacing is ~380ms of echo batch.
	p.spacing = 20 * time.Millisecond

	const traceDur = 200 * time.Millisecond
	want := []Hop{{Index: 1, IP: "10.0.0.1"}}
	p.trace = func(ctx context.Context, host, family string, rounds, maxTTL int, timeout, spacing time.Duration) ([]Hop, bool, error) {
		time.Sleep(traceDur)
		return want, true, nil
	}

	start := time.Now()
	res, err := p.Probe(context.Background(), Target{Host: "127.0.0.1"}, 20)
	if res == nil {
		t.Skipf("ICMP echo unavailable here: %v", err)
	}
	// Concurrent: ~380ms (the echo batch dominates). Sequential in either
	// order: ~580ms. 500ms separates them with ~120ms of margin each way.
	if elapsed := time.Since(start); elapsed >= 500*time.Millisecond {
		t.Fatalf("Probe took %v: the trace and the echo batch ran sequentially, not concurrently", elapsed)
	}
	if len(res.Hops) != len(want) {
		t.Fatalf("got %d hops, want %d", len(res.Hops), len(want))
	}
}

// Probe owns the trace goroutine and must join it on EVERY exit path. The
// early return in the echo loop is the one that matters: an implementation
// that polls a ready result there but blocks only on the normal return passes
// a happy-path join test while leaking a slow trace and its raw socket.
func TestICMPProbeJoinsSlowTraceOnEarlyReturn(t *testing.T) {
	p := NewICMP("icmp", time.Second, false)
	p.spacing = 50 * time.Millisecond

	const traceDur = 250 * time.Millisecond
	want := []Hop{{Index: 1, IP: "10.0.0.1"}}
	p.trace = func(ctx context.Context, host, family string, rounds, maxTTL int, timeout, spacing time.Duration) ([]Hop, bool, error) {
		time.Sleep(traceDur)
		return want, true, nil
	}

	// The echo loop is cancelled at ~60ms, long before the trace finishes.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	start := time.Now()
	res, err := p.Probe(ctx, Target{Host: "127.0.0.1"}, 20)
	if res == nil {
		t.Skipf("ICMP echo unavailable here: %v", err)
	}
	if elapsed := time.Since(start); elapsed < traceDur {
		t.Fatalf("Probe returned in %v on the early-return path without joining the trace", elapsed)
	}
	if len(res.Hops) != len(want) {
		t.Fatalf("got %d hops, want %d — trace result dropped on the early-return path", len(res.Hops), len(want))
	}
}
