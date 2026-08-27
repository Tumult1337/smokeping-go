package probe

import (
	"context"
	"errors"
	"io/fs"
	"testing"
	"time"

	"golang.org/x/net/icmp"
)

// scheduler.runCycle branches only on `res == nil` to choose between leaving a
// gap and stamping Sent = cfg.Pings, so a probe that never put a packet on the
// wire must return a non-nil Result. Nothing asserted that for either probe:
// reverting both fixes left the whole suite green.
func TestProbesThatSentNothingReturnAGapNotFullLoss(t *testing.T) {
	t.Run("icmp cannot open a socket", func(t *testing.T) {
		orig := listenFn
		t.Cleanup(func() { listenFn = orig })
		listenFn = func(bool) (*icmp.PacketConn, error) { return nil, fs.ErrPermission }

		p := NewICMP("icmp", time.Second, true)
		res, err := p.Probe(context.Background(), Target{Host: "127.0.0.1"}, 5)
		if err == nil {
			t.Fatal("listen failure reported no error")
		}
		if res == nil {
			t.Fatal("nil Result: the scheduler stamps Sent = cfg.Pings, so one local socket fault writes a 100%-loss cycle for every icmp target every interval")
		}
		if res.Sent != 0 {
			t.Errorf("Sent = %d, want 0: no packet left the host", res.Sent)
		}
	})

	t.Run("mtr has no raw socket", func(t *testing.T) {
		p := NewMTR("mtr", time.Second)
		p.trace = func(context.Context, string, string, int, int, time.Duration, time.Duration) ([]Hop, roundStats, error) {
			return nil, roundStats{}, classifyListenErr(fs.ErrPermission)
		}
		res, err := p.Probe(context.Background(), Target{Host: "127.0.0.1"}, 3)
		if !errors.Is(err, errRawUnavailable) {
			t.Fatalf("err = %v, want errRawUnavailable", err)
		}
		if res == nil {
			t.Fatal("nil Result: a missing CAP_NET_RAW pages every mtr target at 100% loss instead of recording the gap it is")
		}
		if res.Sent != 0 {
			t.Errorf("Sent = %d, want 0: the walk never ran", res.Sent)
		}
	})
}

// The gap only applies to a fault on our side. A resolve failure is a fact
// about the target and must still stamp full loss, or a blackholed host — the
// thing an operator most needs paged — becomes a permanent silent gap.
func TestUnresolvableHostIsStillFullLoss(t *testing.T) {
	p := NewICMP("icmp", 50*time.Millisecond, true)
	res, err := p.Probe(context.Background(), Target{Host: "no-such-host.invalid"}, 3)
	if err == nil {
		t.Skip("resolver answered for .invalid")
	}
	if res != nil {
		t.Errorf("resolve failure returned a Result, which leaves a gap where the target should page")
	}
}
