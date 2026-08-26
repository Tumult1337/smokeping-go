package probe

import (
	"context"
	"testing"
	"time"
)

// The mirror must key on TargetReply, not on the deepest index: after an
// early echo followed by deeper silent rounds (the shape
// TestWalkRoundsMarksEarlyEchoRow pins), the deepest row is a silent
// intermediate — mirroring it reports an unresponsive router's numbers as
// the target's.
func TestMTRMirrorsTargetRows(t *testing.T) {
	m := NewMTR("mtr", time.Second)
	var called bool
	m.trace = func(ctx context.Context, host, family string, rounds, maxTTL int, timeout, spacing time.Duration) ([]Hop, roundStats, error) {
		called = true
		return []Hop{
			{Index: 1, IP: "10.0.0.1", RTTs: []time.Duration{time.Millisecond}, Sent: 2},
			{Index: 2, IP: "192.0.2.9", RTTs: []time.Duration{2 * time.Millisecond}, Sent: 2, Lost: 1, TargetReply: true},
			{Index: 3, IP: "", Sent: 1, Lost: 1},
			{Index: 4, IP: "", Sent: 1, Lost: 1},
		}, roundStats{attempted: 2, reached: 1}, nil
	}
	res, err := m.Probe(context.Background(), Target{Host: "example.invalid"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("m.trace seam bypassed")
	}
	if res.Sent != 2 || res.LossCount != 1 || len(res.RTTs) != 1 {
		t.Fatalf("mirror did not key on the marked row: Sent=%d Lost=%d RTTs=%d",
			res.Sent, res.LossCount, len(res.RTTs))
	}
	if res.RTTs[0] != 2*time.Millisecond {
		t.Fatalf("mirror carried a foreign RTT: %v", res.RTTs)
	}
}

// Unreached traces keep reporting full loss, not a mirrored intermediate.
func TestMTRUnreachedReportsFullLoss(t *testing.T) {
	m := NewMTR("mtr", time.Second)
	m.trace = func(ctx context.Context, host, family string, rounds, maxTTL int, timeout, spacing time.Duration) ([]Hop, roundStats, error) {
		return []Hop{{Index: 1, IP: "10.0.0.1", RTTs: []time.Duration{time.Millisecond}, Sent: 3}}, roundStats{attempted: 3}, nil
	}
	res, err := m.Probe(context.Background(), Target{Host: "example.invalid"}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if res.Sent != 3 || res.LossCount != 3 || len(res.RTTs) != 0 {
		t.Fatalf("unreached mirror leaked intermediate stats: %+v", res)
	}
}

// Two marked rows (anycast at the terminal) aggregate their RTTs into the
// mirror; the deepest row is a silent intermediate and must contribute
// nothing. Three rounds probe ttl 2 three times — one silent, one answered by
// each sibling — so the rows are what a real walk can produce.
func TestMTRMirrorsAggregateAcrossMarkedRows(t *testing.T) {
	m := NewMTR("mtr", time.Second)
	m.trace = func(ctx context.Context, host, family string, rounds, maxTTL int, timeout, spacing time.Duration) ([]Hop, roundStats, error) {
		return []Hop{
			{Index: 1, IP: "10.0.0.1", RTTs: []time.Duration{time.Millisecond}, Sent: 3},
			{Index: 2, IP: "192.0.2.9", RTTs: []time.Duration{2 * time.Millisecond}, Sent: 2, Lost: 1, TargetReply: true},
			{Index: 2, IP: "192.0.2.10", RTTs: []time.Duration{3 * time.Millisecond}, Sent: 1, TargetReply: true},
			{Index: 5, IP: "", Sent: 1, Lost: 1},
		}, roundStats{attempted: 3, reached: 2}, nil
	}
	res, err := m.Probe(context.Background(), Target{Host: "example.invalid"}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if res.Sent != 3 || res.LossCount != 1 || len(res.RTTs) != 2 {
		t.Fatalf("mirror did not aggregate marked rows: Sent=%d Lost=%d RTTs=%d",
			res.Sent, res.LossCount, len(res.RTTs))
	}
	if res.RTTs[0] != 2*time.Millisecond || res.RTTs[1] != 3*time.Millisecond {
		t.Fatalf("mirror lost a sibling's samples: %v", res.RTTs)
	}
}

// A route that lengthens across rounds marks the target at three TTLs, and
// each of those rows carries the losses of the rounds that walked past it.
// Summing the rows therefore counts one round several times: three rounds that
// all reached the target read as six sent and three lost, and 50% loss pages
// an operator for an outage that never happened.
// The mirror must carry the target's echo latencies only — the fixture is
// produced by the real walkRounds over mixedTerminalScript, not hand-written.
func TestMTRMirrorExcludesUnreachableRTTs(t *testing.T) {
	m := NewMTR("mtr", time.Second)
	m.trace = func(ctx context.Context, host, family string, rounds, maxTTL int, timeout, spacing time.Duration) ([]Hop, roundStats, error) {
		hops, stats := walkRounds(ctx, rounds, maxTTL, spacing, mixedTerminalScript(5*time.Millisecond, 900*time.Millisecond).step)
		return hops, stats, nil
	}
	res, err := m.Probe(context.Background(), Target{Host: "example.invalid"}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if res.Sent != 3 || res.LossCount != 2 {
		t.Fatalf("Sent=%d Lost=%d, want 3/2 — one echo in three rounds", res.Sent, res.LossCount)
	}
	if len(res.RTTs) != res.Sent-res.LossCount {
		t.Fatalf("len(RTTs)=%d disagrees with Sent-LossCount=%d: %v", len(res.RTTs), res.Sent-res.LossCount, res.RTTs)
	}
	if res.RTTs[0] != 5*time.Millisecond {
		t.Fatalf("mirror carried the unreachable's RTT: %v", res.RTTs)
	}
}

func TestMTRSentCountsRoundsNotHopRows(t *testing.T) {
	m := NewMTR("mtr", time.Second)
	m.trace = func(ctx context.Context, host, family string, rounds, maxTTL int, timeout, spacing time.Duration) ([]Hop, roundStats, error) {
		return []Hop{
			{Index: 1, IP: "10.0.0.1", RTTs: []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}, Sent: 3},
			{Index: 2, IP: "192.0.2.9", RTTs: []time.Duration{2 * time.Millisecond}, Sent: 3, Lost: 2, TargetReply: true},
			{Index: 3, IP: "192.0.2.9", RTTs: []time.Duration{3 * time.Millisecond}, Sent: 2, Lost: 1, TargetReply: true},
			{Index: 4, IP: "192.0.2.9", RTTs: []time.Duration{4 * time.Millisecond}, Sent: 1, TargetReply: true},
		}, roundStats{attempted: 3, reached: 3}, nil
	}
	res, err := m.Probe(context.Background(), Target{Host: "example.invalid"}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if res.Sent != 3 || res.LossCount != 0 {
		t.Fatalf("Sent=%d Lost=%d, want 3 rounds sent and none lost", res.Sent, res.LossCount)
	}
	if len(res.RTTs) != 3 {
		t.Fatalf("mirror dropped a round's RTT: %v", res.RTTs)
	}
}

// A walk the cycle deadline cut short must report the rounds that ran: a Sent
// preset from the requested count reports loss for rounds never sent.
func TestMTRSentTracksTruncatedWalk(t *testing.T) {
	m := NewMTR("mtr", time.Second)
	m.trace = func(ctx context.Context, host, family string, rounds, maxTTL int, timeout, spacing time.Duration) ([]Hop, roundStats, error) {
		return []Hop{{Index: 1, IP: "10.0.0.1", RTTs: []time.Duration{time.Millisecond}, Sent: 1}}, roundStats{attempted: 1}, nil
	}
	res, err := m.Probe(context.Background(), Target{Host: "example.invalid"}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if res.Sent != 1 || res.LossCount != 1 {
		t.Fatalf("Sent=%d Lost=%d, want the single round that ran", res.Sent, res.LossCount)
	}
}
