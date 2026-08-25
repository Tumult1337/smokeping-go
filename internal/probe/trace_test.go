package probe

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/icmp"
)

// swapListenRaw routes traceHops' socket open through a stub for the duration
// of one test. Not parallel-safe by design; none of these tests call
// t.Parallel.
func swapListenRaw(t *testing.T, fn func(bool) (*icmp.PacketConn, error)) {
	t.Helper()
	orig := listenRawFn
	listenRawFn = fn
	t.Cleanup(func() { listenRawFn = orig })
}

// Only permission errors may claim errRawUnavailable: that class is logged
// once per process and then silenced, so classifying a transient EMFILE into
// it makes every later raw-socket failure invisible.
func TestTraceHopsClassifiesListenErrors(t *testing.T) {
	tests := []struct {
		name        string
		listenErr   error
		wantRawUnav bool
	}{
		{"EPERM is permission", &net.OpError{Op: "listen", Err: os.NewSyscallError("socket", syscall.EPERM)}, true},
		{"EACCES is permission", &net.OpError{Op: "listen", Err: os.NewSyscallError("socket", syscall.EACCES)}, true},
		{"wrapped fs.ErrPermission is permission", fmt.Errorf("denied: %w", fs.ErrPermission), true},
		{"EMFILE is transient", &net.OpError{Op: "listen", Err: os.NewSyscallError("socket", syscall.EMFILE)}, false},
		{"ENOBUFS is transient", &net.OpError{Op: "listen", Err: os.NewSyscallError("socket", syscall.ENOBUFS)}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			swapListenRaw(t, func(bool) (*icmp.PacketConn, error) { return nil, tc.listenErr })
			_, _, err := traceHops(context.Background(), "127.0.0.1", "", 1, 1, time.Millisecond, 0)
			if err == nil {
				t.Fatal("expected an error from a failed listen")
			}
			if got := errors.Is(err, errRawUnavailable); got != tc.wantRawUnav {
				t.Fatalf("errors.Is(err, errRawUnavailable) = %v, want %v (err: %v)", got, tc.wantRawUnav, err)
			}
			if !errors.Is(err, tc.listenErr) {
				t.Fatalf("underlying error lost from chain: %v", err)
			}
		})
	}
}

// The throttle is time-based and counts what it suppresses: a throttle
// without a count is a blind window — up to ~366 failures/minute fleet-wide
// would collapse into one line, quietly recreating a milder version of the
// invisibility this task exists to fix.
func TestTraceErrLogThrottle(t *testing.T) {
	resetTraceErrThrottle()
	// Elapsed values are strictly positive: time.Since never returns 0, and
	// the throttle uses 0 as its never-logged sentinel.
	if ok, n := traceErrLogAllowed(time.Second); !ok || n != 0 {
		t.Fatalf("first warn: ok=%v n=%d, want allowed with 0 suppressed", ok, n)
	}
	if ok, _ := traceErrLogAllowed(time.Second + traceErrLogEvery/3); ok {
		t.Fatal("second warn inside the window must be suppressed")
	}
	if ok, _ := traceErrLogAllowed(time.Second + traceErrLogEvery/2); ok {
		t.Fatal("third warn inside the window must be suppressed")
	}
	if ok, n := traceErrLogAllowed(2*time.Second + traceErrLogEvery); !ok || n != 2 {
		t.Fatalf("post-window warn: ok=%v n=%d, want allowed with 2 suppressed", ok, n)
	}
	if ok, n := traceErrLogAllowed(3*time.Second + 2*traceErrLogEvery); !ok || n != 0 {
		t.Fatalf("suppressed count must reset once reported: ok=%v n=%d", ok, n)
	}
}

// CanonicalUnreach is the wire-ingest guard: every closed-set label passes
// verbatim, an unknown non-empty value folds to the fixed fallback, and
// empty stays empty (empty means "no annotation", and inventing one would
// annotate every hop). All 13 labels are enumerated so a whitelist that
// shrank to the two labels other tests happen to use goes RED here.
func TestCanonicalUnreach(t *testing.T) {
	valid := []string{
		"net-unreachable", "host-unreachable", "proto-unreachable",
		"port-unreachable", "frag-needed", "source-route-failed",
		"admin-prohibited", "no-route", "beyond-scope",
		"addr-unreachable", "policy-fail", "reject-route",
		"unreachable-other",
	}
	for _, label := range valid {
		if got := CanonicalUnreach(label); got != label {
			t.Errorf("CanonicalUnreach(%q) = %q, want it unchanged", label, got)
		}
	}
	if got := CanonicalUnreach(""); got != "" {
		t.Fatalf("empty must stay empty, got %q", got)
	}
	for _, hostile := range []string{"<img src=x onerror=alert(1)>", "unreachable-code-999999", "x"} {
		if got := CanonicalUnreach(hostile); got != "unreachable-other" {
			t.Fatalf("CanonicalUnreach(%q) = %q, want unreachable-other", hostile, got)
		}
	}
}

// scriptStep replays a per-(round,ttl) script and records every call, so
// assertions can cover both what the walk produced and what it probed.
type scriptStep struct {
	replies map[[2]int]ttlReply
	calls   [][2]int
}

func (s *scriptStep) step(_ context.Context, round, ttl int) ttlReply {
	s.calls = append(s.calls, [2]int{round, ttl})
	if r, ok := s.replies[[2]int{round, ttl}]; ok {
		return r
	}
	return ttlReply{}
}

func (s *scriptStep) called(round, ttl int) bool {
	for _, c := range s.calls {
		if c == [2]int{round, ttl} {
			return true
		}
	}
	return false
}

func hopByIndex(t *testing.T, hops []Hop, idx int) Hop {
	t.Helper()
	for _, h := range hops {
		if h.Index == idx {
			return h
		}
	}
	t.Fatalf("no hop at index %d in %+v", idx, hops)
	return Hop{}
}

func te(addr string) ttlReply {
	return ttlReply{addr: addr, rtt: time.Millisecond, kind: replyTimeExceeded}
}
func ech(addr string) ttlReply { return ttlReply{addr: addr, rtt: time.Millisecond, kind: replyEcho} }

// A route that lengthens mid-cycle must have its new downstream hops probed:
// the old cross-round finalTTL clamp froze the walk at the shortest path yet
// seen, so hops added by a reroute never appeared at all.
func TestWalkRoundsFollowsRouteLengthening(t *testing.T) {
	s := &scriptStep{replies: map[[2]int]ttlReply{
		{0, 1}: te("10.0.0.1"), {0, 2}: ech("192.0.2.9"),
		{1, 1}: te("10.0.0.1"), {1, 2}: te("10.0.1.2"), {1, 3}: te("10.0.1.3"), {1, 4}: ech("192.0.2.9"),
	}}
	hops, reached := walkRounds(context.Background(), 2, 10, 0, s.step)
	if !reached {
		t.Fatal("target answered in both rounds; reached must be true")
	}
	if !s.called(1, 3) || !s.called(1, 4) {
		t.Fatalf("round 1 never probed past the round-0 path length; calls: %v", s.calls)
	}
	if h := hopByIndex(t, hops, 3); h.Sent != 1 {
		t.Fatalf("hop 3 sent = %d, want 1", h.Sent)
	}
	hopByIndex(t, hops, 4)
}

// A route that shortens must not discard rows already collected on the longer
// path: they carry real measurements from the rounds that walked it.
func TestWalkRoundsKeepsRowsFromShortenedRoute(t *testing.T) {
	s := &scriptStep{replies: map[[2]int]ttlReply{
		{0, 1}: te("10.0.0.1"), {0, 2}: te("10.0.0.2"), {0, 3}: te("10.0.0.3"), {0, 4}: ech("192.0.2.9"),
		{1, 1}: te("10.0.0.1"), {1, 2}: ech("192.0.2.9"),
	}}
	hops, _ := walkRounds(context.Background(), 2, 10, 0, s.step)
	if h := hopByIndex(t, hops, 3); h.Sent != 1 || h.IP != "10.0.0.3" {
		t.Fatalf("longer-path hop 3 lost or altered: %+v", h)
	}
	if h := hopByIndex(t, hops, 4); h.Sent != 1 {
		t.Fatalf("longer-path hop 4 lost: %+v", h)
	}
}

// Reaching the target still ends the round — the per-round stop is what keeps
// a stable path from costing maxTTL probes every round.
func TestWalkRoundsStopsEachRoundAtEcho(t *testing.T) {
	s := &scriptStep{replies: map[[2]int]ttlReply{
		{0, 1}: te("10.0.0.1"), {0, 2}: ech("192.0.2.9"),
		{1, 1}: te("10.0.0.1"), {1, 2}: ech("192.0.2.9"),
	}}
	_, _ = walkRounds(context.Background(), 2, 10, 0, s.step)
	for _, c := range s.calls {
		if c[1] > 2 {
			t.Fatalf("walk probed ttl %d past the reached target in round %d", c[1], c[0])
		}
	}
}

// The composed shape behind the redaction and mirror designs: the target
// echoes at ttl 2 in round 0, later rounds stay silent through maxTTL. The
// echo row must carry TargetReply, deeper silent rows must not, and reached
// stays true — TestRedactTerminalHopKeysOnTargetReply (internal/api) and
// TestMTRMirrorsTargetRows both fix their fixtures to this exact output.
func TestWalkRoundsMarksEarlyEchoRow(t *testing.T) {
	s := &scriptStep{replies: map[[2]int]ttlReply{
		{0, 1}: te("10.0.0.1"), {0, 2}: ech("192.0.2.9"),
	}}
	hops, reached := walkRounds(context.Background(), 2, 4, 0, s.step)
	if !reached {
		t.Fatal("round 0 reached the target; a silent round 1 must not clear it")
	}
	target := hopByIndex(t, hops, 2)
	if !target.TargetReply || target.IP != "192.0.2.9" {
		t.Fatalf("echo row not marked: %+v", target)
	}
	for _, h := range hops {
		if h.Index != 2 && h.TargetReply {
			t.Fatalf("non-echo row marked at index %d: %+v", h.Index, h)
		}
	}
	if deepest := hopByIndex(t, hops, 4); deepest.IP != "" || deepest.TargetReply {
		t.Fatalf("silent deep row must stay unmarked and addressless: %+v", deepest)
	}
}

// Cancellation stops the walk but still emits what was collected.
func TestWalkRoundsEmitsPartialOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	step := func(_ context.Context, round, ttl int) ttlReply {
		calls++
		if calls == 2 {
			cancel()
		}
		return te(fmt.Sprintf("10.0.0.%d", ttl))
	}
	hops, reached := walkRounds(ctx, 3, 10, 0, step)
	if reached {
		t.Fatal("nothing echoed; reached must be false")
	}
	if len(hops) != 2 {
		t.Fatalf("got %d hops, want the 2 collected before cancel: %+v", len(hops), hops)
	}
}
