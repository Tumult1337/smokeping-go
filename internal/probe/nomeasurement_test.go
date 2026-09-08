package probe

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/icmp"
)

// Local socket failures must not fabricate full target loss. ICMP cannot
// measure without its echo socket; MTR can retain a completed direct batch
// when only its raw trace socket is unavailable.
func TestLocalSocketFailuresDoNotFabricateFullLoss(t *testing.T) {
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

	t.Run("mtr trace has no raw socket", func(t *testing.T) {
		logs := captureLogs(t)
		p := NewMTR("mtr", time.Second)
		p.echo = func(context.Context, Target, int) (*Result, error) {
			return &Result{RTTs: []time.Duration{time.Millisecond, 2 * time.Millisecond}, Sent: 3, LossCount: 1}, nil
		}
		p.trace = func(context.Context, string, string, int, int, time.Duration, time.Duration) ([]Hop, roundStats, error) {
			return nil, roundStats{}, classifyListenErr(fs.ErrPermission)
		}
		res, err := p.Probe(context.Background(), Target{Host: "127.0.0.1"}, 3)
		if !errors.Is(err, errRawUnavailable) {
			t.Fatalf("err = %v, want errRawUnavailable", err)
		}
		if res == nil {
			t.Fatal("nil Result: the completed direct measurement was discarded")
		}
		if res.Sent != 3 || res.LossCount != 1 || len(res.RTTs) != 2 {
			t.Errorf("direct result not preserved: %+v", res)
		}
		if len(res.Hops) != 0 {
			t.Errorf("Hops = %v, want none: the walk never ran", res.Hops)
		}
		errorRecords := logs.at(slog.LevelError)
		if len(errorRecords) != 1 {
			t.Fatalf("got %d error records, want 1: %q", len(errorRecords), errorRecords)
		}
		for _, want := range []string{"mtr hops unavailable", "direct target measurement can continue"} {
			if !strings.Contains(errorRecords[0], want) {
				t.Fatalf("raw-socket diagnostic %q does not state %q", errorRecords[0], want)
			}
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
