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
	m.trace = func(ctx context.Context, host, family string, rounds, maxTTL int, timeout, spacing time.Duration) ([]Hop, bool, error) {
		called = true
		return []Hop{
			{Index: 1, IP: "10.0.0.1", RTTs: []time.Duration{time.Millisecond}, Sent: 2},
			{Index: 2, IP: "192.0.2.9", RTTs: []time.Duration{2 * time.Millisecond}, Sent: 2, Lost: 1, TargetReply: true},
			{Index: 3, IP: "", Sent: 1, Lost: 1},
			{Index: 4, IP: "", Sent: 1, Lost: 1},
		}, true, nil
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
	m.trace = func(ctx context.Context, host, family string, rounds, maxTTL int, timeout, spacing time.Duration) ([]Hop, bool, error) {
		return []Hop{{Index: 1, IP: "10.0.0.1", RTTs: []time.Duration{time.Millisecond}, Sent: 3}}, false, nil
	}
	res, err := m.Probe(context.Background(), Target{Host: "example.invalid"}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if res.Sent != 3 || res.LossCount != 3 || len(res.RTTs) != 0 {
		t.Fatalf("unreached mirror leaked intermediate stats: %+v", res)
	}
}
