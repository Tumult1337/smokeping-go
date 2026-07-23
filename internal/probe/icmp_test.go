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
