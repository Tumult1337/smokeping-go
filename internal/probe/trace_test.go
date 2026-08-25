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
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
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

func teRTT(addr string, rtt time.Duration) ttlReply {
	return ttlReply{addr: addr, rtt: rtt, kind: replyTimeExceeded}
}

// ECMP responders at one TTL must each get their own row with their own
// samples, in first-seen order. RTT values are distinct on purpose: a
// length-only assertion stays green when rows swap their samples.
func TestWalkRoundsSplitsECMPRespondersPerAddress(t *testing.T) {
	s := &scriptStep{replies: map[[2]int]ttlReply{
		{0, 1}: teRTT("10.0.0.1", 5*time.Millisecond), {0, 2}: ech("192.0.2.9"),
		{1, 1}: teRTT("10.0.9.9", 7*time.Millisecond), {1, 2}: ech("192.0.2.9"),
		{2, 1}: teRTT("10.0.9.9", 9*time.Millisecond), {2, 2}: ech("192.0.2.9"),
	}}
	hops, _ := walkRounds(context.Background(), 3, 10, 0, s.step)

	var ttl1 []Hop
	for _, h := range hops {
		if h.Index == 1 {
			ttl1 = append(ttl1, h)
		}
	}
	if len(ttl1) != 2 {
		t.Fatalf("want one row per responder at ttl 1, got %+v", hops)
	}
	if ttl1[0].IP != "10.0.0.1" || ttl1[1].IP != "10.0.9.9" {
		t.Fatalf("responder rows not in first-seen order: %+v", ttl1)
	}
	a, b := ttl1[0], ttl1[1]
	if a.Sent != 1 || len(a.RTTs) != 1 || a.RTTs[0] != 5*time.Millisecond {
		t.Fatalf("A's samples wrong or foreign: %+v", a)
	}
	if b.Sent != 2 || len(b.RTTs) != 2 || b.RTTs[0] != 7*time.Millisecond || b.RTTs[1] != 9*time.Millisecond {
		t.Fatalf("B's samples wrong or foreign: %+v", b)
	}
	if a.TargetReply || b.TargetReply {
		t.Fatalf("intermediate responders must not be marked: %+v", ttl1)
	}
}

// Loss at a TTL cannot be attributed to a responder that answered; it stays
// on the first-seen responder's row so single-responder numbers stay
// bit-identical to the pre-split behavior.
func TestWalkRoundsAttachesLossToFirstResponder(t *testing.T) {
	s := &scriptStep{replies: map[[2]int]ttlReply{
		{1, 1}: teRTT("10.0.0.1", 5*time.Millisecond),
		{2, 1}: teRTT("10.0.9.9", 7*time.Millisecond),
	}}
	hops, reached := walkRounds(context.Background(), 3, 1, 0, s.step)
	if reached {
		t.Fatal("no echo in script")
	}
	var a, b Hop
	for _, h := range hops {
		switch h.IP {
		case "10.0.0.1":
			a = h
		case "10.0.9.9":
			b = h
		}
	}
	if a.Sent != 2 || a.Lost != 1 || len(a.RTTs) != 1 {
		t.Fatalf("first responder must absorb the ttl's loss (sent=2 lost=1), got %+v", a)
	}
	if b.Sent != 1 || b.Lost != 0 {
		t.Fatalf("later responder must stay clean, got %+v", b)
	}
	for _, h := range hops {
		if h.Index == 1 && h.IP == "" {
			t.Fatalf("silent row emitted although responders exist: %+v", hops)
		}
	}
}

// A TTL nothing ever answered still emits its silent row — the heatmap reads
// IP=="" as no-reply and the row carries the loss evidence.
func TestWalkRoundsEmitsSilentRowWhenNoResponder(t *testing.T) {
	s := &scriptStep{replies: map[[2]int]ttlReply{}}
	hops, _ := walkRounds(context.Background(), 3, 1, 0, s.step)
	if len(hops) != 1 {
		t.Fatalf("got %d hops, want 1 silent row: %+v", len(hops), hops)
	}
	if h := hops[0]; h.IP != "" || h.Sent != 3 || h.Lost != 3 {
		t.Fatalf("silent row wrong: %+v", h)
	}
}

// Anycast/flap at the terminal: two different addresses echo at one TTL, and
// BOTH rows must be marked — the mirror and the redaction aggregate marked
// rows, so an unmarked second target address would leak and under-count.
func TestWalkRoundsMarksEveryEchoResponder(t *testing.T) {
	s := &scriptStep{replies: map[[2]int]ttlReply{
		{0, 1}: te("10.0.0.1"), {0, 2}: {addr: "192.0.2.9", rtt: 2 * time.Millisecond, kind: replyEcho},
		{1, 1}: te("10.0.0.1"), {1, 2}: {addr: "192.0.2.10", rtt: 3 * time.Millisecond, kind: replyEcho},
	}}
	hops, _ := walkRounds(context.Background(), 2, 10, 0, s.step)
	marked := 0
	for _, h := range hops {
		if h.Index == 2 {
			if !h.TargetReply {
				t.Fatalf("echo responder unmarked: %+v", h)
			}
			marked++
		}
	}
	if marked != 2 {
		t.Fatalf("want 2 marked target rows, got %d: %+v", marked, hops)
	}
}

func unreach(addr, label string) ttlReply {
	return ttlReply{addr: addr, rtt: time.Millisecond, kind: replyUnreachable, unreach: label}
}

// An unreachable-reporting gateway is the true end of the path: continuing
// past it re-elicits the same gateway at every later TTL and fabricates a
// clean 30-hop path out of a single router, at zero loss, in exactly the
// cycle an operator is debugging.
func TestWalkRoundsStopsAtUnreachable(t *testing.T) {
	s := &scriptStep{replies: map[[2]int]ttlReply{}}
	for round := range 3 {
		s.replies[[2]int{round, 1}] = te("10.0.0.1")
		for ttl := 2; ttl <= 30; ttl++ {
			s.replies[[2]int{round, ttl}] = unreach("10.0.0.2", "host-unreachable")
		}
	}
	hops, reached := walkRounds(context.Background(), 3, 30, 0, s.step)
	if reached {
		t.Fatal("an unreachable reply must not count as reaching the target")
	}
	if len(hops) != 2 {
		t.Fatalf("fabricated path: got %d hops, want 2: %+v", len(hops), hops)
	}
	for _, c := range s.calls {
		if c[1] > 2 {
			t.Fatalf("walk probed ttl %d past the unreachable terminal in round %d", c[1], c[0])
		}
	}
	gw := hopByIndex(t, hops, 2)
	if gw.Unreach != "host-unreachable" || gw.IP != "10.0.0.2" || gw.Sent != 3 || gw.TargetReply {
		t.Fatalf("gateway row wrong: %+v", gw)
	}
	if h := hopByIndex(t, hops, 1); h.Unreach != "" {
		t.Fatalf("intermediate hop must carry no annotation: %+v", h)
	}
}

// The annotation belongs to the responder that sent it: a TTL answered by
// router A (TimeExceeded) in one round and by gateway B (unreachable) in
// another must annotate B's row only.
func TestWalkRoundsAnnotatesTheUnreachableResponder(t *testing.T) {
	s := &scriptStep{replies: map[[2]int]ttlReply{
		{0, 1}: te("10.0.0.1"),
		{1, 1}: unreach("10.0.9.9", "admin-prohibited"),
	}}
	hops, _ := walkRounds(context.Background(), 2, 5, 0, s.step)
	var a, b Hop
	for _, h := range hops {
		switch h.IP {
		case "10.0.0.1":
			a = h
		case "10.0.9.9":
			b = h
		}
	}
	if a.Unreach != "" {
		t.Fatalf("TimeExceeded responder annotated: %+v", a)
	}
	if b.Unreach != "admin-prohibited" {
		t.Fatalf("unreachable responder lost its annotation: %+v", b)
	}
}

// First non-empty label per responder wins across rounds — a flapping
// annotation must not erase the one that ended the first round.
func TestWalkRoundsKeepsFirstUnreachLabel(t *testing.T) {
	s := &scriptStep{replies: map[[2]int]ttlReply{
		{0, 1}: unreach("10.0.0.2", "host-unreachable"),
		{1, 1}: unreach("10.0.0.2", "admin-prohibited"),
	}}
	hops, _ := walkRounds(context.Background(), 2, 5, 0, s.step)
	if h := hopByIndex(t, hops, 1); h.Unreach != "host-unreachable" {
		t.Fatalf("label = %q, want the first round's host-unreachable", h.Unreach)
	}
}

func TestUnreachLabel(t *testing.T) {
	tests := []struct {
		isV6 bool
		code int
		want string
	}{
		{false, 0, "net-unreachable"},
		{false, 1, "host-unreachable"},
		{false, 2, "proto-unreachable"},
		{false, 3, "port-unreachable"},
		{false, 4, "frag-needed"},
		{false, 5, "source-route-failed"},
		{false, 9, "admin-prohibited"},
		{false, 10, "admin-prohibited"},
		{false, 13, "admin-prohibited"},
		{false, 7, "unreachable-other"},
		{true, 0, "no-route"},
		{true, 1, "admin-prohibited"},
		{true, 2, "beyond-scope"},
		{true, 3, "addr-unreachable"},
		{true, 4, "port-unreachable"},
		{true, 5, "policy-fail"},
		{true, 6, "reject-route"},
		{true, 200, "unreachable-other"},
	}
	for _, tc := range tests {
		if got := unreachLabel(tc.isV6, tc.code); got != tc.want {
			t.Errorf("unreachLabel(%v, %d) = %q, want %q", tc.isV6, tc.code, got, tc.want)
		}
	}
}

// embeddedEcho builds the quoted-packet bytes an ICMP error carries: an IP
// header followed by the first 8 bytes of the original echo request.
func embeddedEcho(isV6 bool, id, seq int) []byte {
	hdr := []byte{8, 0, 0, 0, byte(id >> 8), byte(id), byte(seq >> 8), byte(seq)}
	if isV6 {
		return append(make([]byte, 40), hdr...)
	}
	ip := make([]byte, 20)
	ip[0] = 0x45
	return append(ip, hdr...)
}

// classifyReply is the parser boundary for bytes any host on the network can
// put on a raw socket: it must match strictly on id+seq, classify only
// matched replies, and discriminate on reply.Type — an Echo REQUEST parses
// to the same *icmp.Echo body as a Reply, and treating a spoofed request as
// "reached" both falsely terminates the round and falsely marks the row.
func TestClassifyReply(t *testing.T) {
	const id, seq = 0x1234, 7
	tests := []struct {
		name        string
		msg         *icmp.Message
		isV6        bool
		wantMatch   bool
		wantKind    replyKind
		wantUnreach string
	}{
		{"echo reply match", &icmp.Message{Type: ipv4.ICMPTypeEchoReply, Body: &icmp.Echo{ID: id, Seq: seq}}, false, true, replyEcho, ""},
		{"echo REQUEST is not the target", &icmp.Message{Type: ipv4.ICMPTypeEcho, Body: &icmp.Echo{ID: id, Seq: seq}}, false, false, replyNone, ""},
		{"v6 echo REQUEST is not the target", &icmp.Message{Type: ipv6.ICMPTypeEchoRequest, Body: &icmp.Echo{ID: id, Seq: seq}}, true, false, replyNone, ""},
		{"echo wrong id", &icmp.Message{Type: ipv4.ICMPTypeEchoReply, Body: &icmp.Echo{ID: id + 1, Seq: seq}}, false, false, replyNone, ""},
		{"echo wrong seq", &icmp.Message{Type: ipv4.ICMPTypeEchoReply, Body: &icmp.Echo{ID: id, Seq: seq + 1}}, false, false, replyNone, ""},
		{"time exceeded match", &icmp.Message{Type: ipv4.ICMPTypeTimeExceeded, Body: &icmp.TimeExceeded{Data: embeddedEcho(false, id, seq)}}, false, true, replyTimeExceeded, ""},
		{"unreach v4 host", &icmp.Message{Type: ipv4.ICMPTypeDestinationUnreachable, Code: 1, Body: &icmp.DstUnreach{Data: embeddedEcho(false, id, seq)}}, false, true, replyUnreachable, "host-unreachable"},
		{"unreach v6 admin", &icmp.Message{Type: ipv6.ICMPTypeDestinationUnreachable, Code: 1, Body: &icmp.DstUnreach{Data: embeddedEcho(true, id, seq)}}, true, true, replyUnreachable, "admin-prohibited"},
		{"unreach wrong embedded id", &icmp.Message{Type: ipv4.ICMPTypeDestinationUnreachable, Code: 1, Body: &icmp.DstUnreach{Data: embeddedEcho(false, id+1, seq)}}, false, false, replyNone, ""},
		{"unreach truncated data", &icmp.Message{Type: ipv4.ICMPTypeDestinationUnreachable, Code: 1, Body: &icmp.DstUnreach{Data: []byte{0x45, 0}}}, false, false, replyNone, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			matched, kind, unreach := classifyReply(tc.msg, tc.isV6, id, seq)
			if matched != tc.wantMatch || kind != tc.wantKind || unreach != tc.wantUnreach {
				t.Fatalf("got (%v, %d, %q), want (%v, %d, %q)",
					matched, kind, unreach, tc.wantMatch, tc.wantKind, tc.wantUnreach)
			}
		})
	}
}
