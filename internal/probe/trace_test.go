package probe

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
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

// addrOrZero maps a test case's "" onto the invalid zero Addr matchDatagram
// reports for a non-match.
func addrOrZero(s string) netip.Addr {
	if s == "" {
		return netip.Addr{}
	}
	return netip.MustParseAddr(s)
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
	return ttlReply{addr: netip.MustParseAddr(addr), rtt: time.Millisecond, kind: replyTimeExceeded}
}
func ech(addr string) ttlReply {
	return ttlReply{addr: netip.MustParseAddr(addr), rtt: time.Millisecond, kind: replyEcho}
}

// A route that lengthens mid-cycle must have its new downstream hops probed:
// the old cross-round finalTTL clamp froze the walk at the shortest path yet
// seen, so hops added by a reroute never appeared at all.
func TestWalkRoundsFollowsRouteLengthening(t *testing.T) {
	s := &scriptStep{replies: map[[2]int]ttlReply{
		{0, 1}: te("10.0.0.1"), {0, 2}: ech("192.0.2.9"),
		{1, 1}: te("10.0.0.1"), {1, 2}: te("10.0.1.2"), {1, 3}: te("10.0.1.3"), {1, 4}: ech("192.0.2.9"),
	}}
	hops, stats := walkRounds(context.Background(), 2, 10, 0, s.step)
	if stats.reached == 0 {
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
	hops, stats := walkRounds(context.Background(), 2, 4, 0, s.step)
	if stats.reached == 0 {
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
	hops, stats := walkRounds(ctx, 3, 10, 0, step)
	if stats.reached > 0 {
		t.Fatal("nothing echoed; reached must be false")
	}
	if stats.attempted != 1 {
		t.Fatalf("attempted = %d, want only the round that ran before cancel", stats.attempted)
	}
	if len(hops) != 2 {
		t.Fatalf("got %d hops, want the 2 collected before cancel: %+v", len(hops), hops)
	}
}

func teRTT(addr string, rtt time.Duration) ttlReply {
	return ttlReply{addr: netip.MustParseAddr(addr), rtt: rtt, kind: replyTimeExceeded}
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
	hops, stats := walkRounds(context.Background(), 3, 1, 0, s.step)
	if stats.reached > 0 {
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
		{0, 1}: te("10.0.0.1"), {0, 2}: {addr: netip.MustParseAddr("192.0.2.9"), rtt: 2 * time.Millisecond, kind: replyEcho},
		{1, 1}: te("10.0.0.1"), {1, 2}: {addr: netip.MustParseAddr("192.0.2.10"), rtt: 3 * time.Millisecond, kind: replyEcho},
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
	return ttlReply{addr: netip.MustParseAddr(addr), rtt: time.Millisecond, kind: replyUnreachable, unreach: label}
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
	hops, stats := walkRounds(context.Background(), 3, 30, 0, s.step)
	if stats.reached > 0 {
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

// mixedTerminalScript is the shape a rate-limiting firewall produces: the
// target echoes once, then rejects from its own address with a slow
// admin-prohibited. Mixed onto one row, the unreachable's error-generation
// time became the target's percentiles and len(RTTs) disagreed with
// Sent-LossCount.
func mixedTerminalScript(echoRTT, rejectRTT time.Duration) *scriptStep {
	rej := ttlReply{addr: netip.MustParseAddr("10.0.0.9"), rtt: rejectRTT, kind: replyUnreachable, unreach: "admin-prohibited"}
	return &scriptStep{replies: map[[2]int]ttlReply{
		{0, 1}: te("10.0.0.1"), {0, 2}: {addr: netip.MustParseAddr("10.0.0.9"), rtt: echoRTT, kind: replyEcho},
		{1, 1}: te("10.0.0.1"), {1, 2}: rej,
		{2, 1}: te("10.0.0.1"), {2, 2}: rej,
	}}
}

func TestWalkRoundsSplitsEchoFromErrorOnOneAddress(t *testing.T) {
	s := mixedTerminalScript(5*time.Millisecond, 900*time.Millisecond)
	hops, stats := walkRounds(context.Background(), 3, 5, 0, s.step)
	if stats.attempted != 3 || stats.reached != 1 {
		t.Fatalf("stats = %+v, want attempted 3 reached 1", stats)
	}
	var marked, errRow *Hop
	for i := range hops {
		if hops[i].Index != 2 {
			continue
		}
		if hops[i].TargetReply {
			marked = &hops[i]
		} else {
			errRow = &hops[i]
		}
	}
	if marked == nil || errRow == nil {
		t.Fatalf("want an echo row and an error row at ttl 2, got %+v", hops)
	}
	if len(marked.RTTs) != 1 || marked.RTTs[0] != 5*time.Millisecond || marked.Sent != 1 {
		t.Fatalf("marked row mixed in unreachable RTTs: %+v", marked)
	}
	if marked.Unreach != "" {
		t.Fatalf("echo row carries the unreachable label: %+v", marked)
	}
	if errRow.Unreach != "admin-prohibited" || errRow.Sent != 2 || len(errRow.RTTs) != 2 {
		t.Fatalf("error row wrong: %+v", errRow)
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
// matched replies, discriminate on reply.Type — an Echo REQUEST parses to the
// same *icmp.Echo body as a Reply, and treating a spoofed request as
// "reached" both falsely terminates the round and falsely marks the row —
// and require an echo reply to come from the destination itself.
func TestClassifyReply(t *testing.T) {
	const id, seq = 0x1234, 7
	v4dst := netip.MustParseAddr("192.0.2.9")
	v6dst := netip.MustParseAddr("2001:db8::1")
	router := netip.MustParseAddr("10.0.0.1")

	type testCase struct {
		name        string
		msg         *icmp.Message
		isV6        bool
		peer, dst   netip.Addr
		wantMatch   bool
		wantKind    replyKind
		wantUnreach string
	}
	tests := []testCase{
		{name: "echo reply match", msg: &icmp.Message{Type: ipv4.ICMPTypeEchoReply, Body: &icmp.Echo{ID: id, Seq: seq}},
			peer: v4dst, dst: v4dst, wantMatch: true, wantKind: replyEcho},
		{name: "echo REQUEST is not the target", msg: &icmp.Message{Type: ipv4.ICMPTypeEcho, Body: &icmp.Echo{ID: id, Seq: seq}},
			peer: v4dst, dst: v4dst},
		{name: "v6 echo REQUEST is not the target", msg: &icmp.Message{Type: ipv6.ICMPTypeEchoRequest, Body: &icmp.Echo{ID: id, Seq: seq}},
			isV6: true, peer: v6dst, dst: v6dst},
		{name: "echo wrong id", msg: &icmp.Message{Type: ipv4.ICMPTypeEchoReply, Body: &icmp.Echo{ID: id + 1, Seq: seq}},
			peer: v4dst, dst: v4dst},
		{name: "echo wrong seq", msg: &icmp.Message{Type: ipv4.ICMPTypeEchoReply, Body: &icmp.Echo{ID: id, Seq: seq + 1}},
			peer: v4dst, dst: v4dst},

		// An on-path router that can see our id and seq can answer from its
		// own address. Nothing in type, id or seq tells it apart from the
		// target's own reply, so the walk marked that router TargetReply,
		// stopped the round, and reported the target as reached.
		{name: "echo reply from a peer that is not the destination",
			msg:  &icmp.Message{Type: ipv4.ICMPTypeEchoReply, Body: &icmp.Echo{ID: id, Seq: seq}},
			peer: router, dst: v4dst},
		{name: "v6 echo reply from a peer that is not the destination",
			msg:  &icmp.Message{Type: ipv6.ICMPTypeEchoReply, Body: &icmp.Echo{ID: id, Seq: seq}},
			isV6: true, peer: netip.MustParseAddr("2001:db8::99"), dst: v6dst},
		{name: "echo reply with no resolvable peer",
			msg:  &icmp.Message{Type: ipv4.ICMPTypeEchoReply, Body: &icmp.Echo{ID: id, Seq: seq}},
			peer: netip.Addr{}, dst: v4dst},
		// The v4-mapped spelling of the destination is the destination.
		{name: "echo reply from the v4-mapped destination",
			msg:  &icmp.Message{Type: ipv4.ICMPTypeEchoReply, Body: &icmp.Echo{ID: id, Seq: seq}},
			peer: netip.MustParseAddr("::ffff:192.0.2.9").Unmap(), dst: v4dst,
			wantMatch: true, wantKind: replyEcho},

		// Errors legitimately come from routers along the path, so the
		// destination check must not reach them.
		{name: "time exceeded match", msg: &icmp.Message{Type: ipv4.ICMPTypeTimeExceeded, Body: &icmp.TimeExceeded{Data: embeddedEcho(false, id, seq)}},
			peer: router, dst: v4dst, wantMatch: true, wantKind: replyTimeExceeded},
		{name: "unreach v4 host", msg: &icmp.Message{Type: ipv4.ICMPTypeDestinationUnreachable, Code: 1, Body: &icmp.DstUnreach{Data: embeddedEcho(false, id, seq)}},
			peer: router, dst: v4dst, wantMatch: true, wantKind: replyUnreachable, wantUnreach: "host-unreachable"},
		{name: "unreach v6 admin", msg: &icmp.Message{Type: ipv6.ICMPTypeDestinationUnreachable, Code: 1, Body: &icmp.DstUnreach{Data: embeddedEcho(true, id, seq)}},
			isV6: true, peer: netip.MustParseAddr("2001:db8::99"), dst: v6dst, wantMatch: true, wantKind: replyUnreachable, wantUnreach: "admin-prohibited"},
		{name: "unreach wrong embedded id", msg: &icmp.Message{Type: ipv4.ICMPTypeDestinationUnreachable, Code: 1, Body: &icmp.DstUnreach{Data: embeddedEcho(false, id+1, seq)}},
			peer: router, dst: v4dst},
		{name: "unreach truncated data", msg: &icmp.Message{Type: ipv4.ICMPTypeDestinationUnreachable, Code: 1, Body: &icmp.DstUnreach{Data: []byte{0x45, 0}}},
			peer: router, dst: v4dst},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			matched, kind, unreach := classifyReply(tc.msg, tc.isV6, id, seq, tc.peer, tc.dst)
			if matched != tc.wantMatch || kind != tc.wantKind || unreach != tc.wantUnreach {
				t.Fatalf("got (%v, %d, %q), want (%v, %d, %q)",
					matched, kind, unreach, tc.wantMatch, tc.wantKind, tc.wantUnreach)
			}
		})
	}
}

// The peer address arrives as *net.UDPAddr on an unprivileged ping socket
// (the kernel rewrites the ICMP id to the source port) and as *net.IPAddr on
// a raw one. Both must reduce to the same comparable value, or the
// destination check holds on one socket type and not the other.
func TestPeerAddrHandlesBothSocketTypes(t *testing.T) {
	want := netip.MustParseAddr("192.0.2.9")
	cases := map[string]net.Addr{
		"udp ping socket": &net.UDPAddr{IP: net.ParseIP("192.0.2.9")},
		"raw socket":      &net.IPAddr{IP: net.ParseIP("192.0.2.9")},
		"raw 4-byte":      &net.IPAddr{IP: net.IPv4(192, 0, 2, 9).To4()},
	}
	for name, a := range cases {
		if got := peerAddr(a); got != want {
			t.Errorf("%s: peerAddr = %v, want %v", name, got, want)
		}
	}
	if got := peerAddr(&net.TCPAddr{IP: net.ParseIP("192.0.2.9")}); got.IsValid() {
		t.Errorf("unexpected address type resolved to %v, want the invalid zero Addr", got)
	}
}

// MTR's loss is round-based, so a round that reached the target counts once
// however many TTLs the target answered at: here a lengthening route marks it
// at three, and the marked rows sum to more probes than there were rounds.
func TestWalkRoundsCountsRoundsNotEchoRows(t *testing.T) {
	s := &scriptStep{replies: map[[2]int]ttlReply{
		{0, 1}: te("10.0.0.1"), {0, 2}: ech("192.0.2.9"),
		{1, 1}: te("10.0.0.1"), {1, 3}: ech("192.0.2.9"),
		{2, 1}: te("10.0.0.1"), {2, 4}: ech("192.0.2.9"),
	}}
	hops, stats := walkRounds(context.Background(), 3, 10, 0, s.step)
	if stats.attempted != 3 || stats.reached != 3 {
		t.Fatalf("stats = %+v, want 3 attempted and 3 reached", stats)
	}
	rowSent := 0
	for _, h := range hops {
		if h.TargetReply {
			rowSent += h.Sent
		}
	}
	if rowSent <= stats.reached {
		t.Fatalf("fixture no longer reproduces row-summed inflation: rowSent=%d", rowSent)
	}
}

// matchDatagram is the whole read path's trust boundary: bytes and source
// address both come off the wire. classifyReply's own table cannot reach the
// wiring here — passing the destination in place of the peer, or resolving an
// unknown address shape to something valid, leaves that table green.
func TestMatchDatagramRequiresEchoFromDestination(t *testing.T) {
	const id, seq = 0x1234, 7
	dst := netip.MustParseAddr("192.0.2.9")
	echoReply, err := (&icmp.Message{
		Type: ipv4.ICMPTypeEchoReply, Body: &icmp.Echo{ID: id, Seq: seq, Data: []byte("x")},
	}).Marshal(nil)
	if err != nil {
		t.Fatal(err)
	}
	timeExceeded, err := (&icmp.Message{
		Type: ipv4.ICMPTypeTimeExceeded, Body: &icmp.TimeExceeded{Data: embeddedEcho(false, id, seq)},
	}).Marshal(nil)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		datagram []byte
		peer     net.Addr
		wantOK   bool
		wantAddr string
		wantKind replyKind
	}{
		{"echo from the destination on a raw socket", echoReply,
			&net.IPAddr{IP: net.ParseIP("192.0.2.9")}, true, "192.0.2.9", replyEcho},
		{"echo from the destination on a ping socket", echoReply,
			&net.UDPAddr{IP: net.ParseIP("192.0.2.9")}, true, "192.0.2.9", replyEcho},
		{"echo forged by an on-path router", echoReply,
			&net.IPAddr{IP: net.ParseIP("10.0.0.1")}, false, "", replyNone},
		{"echo forged by an on-path router on a ping socket", echoReply,
			&net.UDPAddr{IP: net.ParseIP("10.0.0.1")}, false, "", replyNone},
		{"echo from an unresolvable source", echoReply,
			&net.TCPAddr{IP: net.ParseIP("192.0.2.9")}, false, "", replyNone},
		{"time exceeded from a router still answers", timeExceeded,
			&net.IPAddr{IP: net.ParseIP("10.0.0.1")}, true, "10.0.0.1", replyTimeExceeded},
		// The destination check does not cover errors, so only the peer guard
		// keeps an unresolvable source from being written as a hop address.
		{"time exceeded from an unresolvable source", timeExceeded,
			&net.TCPAddr{IP: net.ParseIP("10.0.0.1")}, false, "", replyNone},
		{"unparseable bytes", []byte{0xff}, &net.IPAddr{IP: net.ParseIP("192.0.2.9")}, false, "", replyNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := matchDatagram(tc.datagram, 1, tc.peer, false, id, seq, dst)
			if ok != tc.wantOK {
				t.Fatalf("matched = %v, want %v (%+v)", ok, tc.wantOK, got)
			}
			if got.addr != addrOrZero(tc.wantAddr) || got.kind != tc.wantKind {
				t.Fatalf("got addr %q kind %d, want %q %d", got.addr, got.kind, tc.wantAddr, tc.wantKind)
			}
		})
	}
}

// dst is always resolved before the walk starts today, so peer != dst carries
// the check on its own. It stops carrying it the moment both sides are the
// invalid zero Addr, which is what an unresolved destination reaching here
// would look like — the guard denies that rather than matching everything.
func TestClassifyReplyDeniesAnInvalidDestination(t *testing.T) {
	const id, seq = 0x1234, 7
	msg := &icmp.Message{Type: ipv4.ICMPTypeEchoReply, Body: &icmp.Echo{ID: id, Seq: seq}}
	if matched, kind, _ := classifyReply(msg, false, id, seq, netip.Addr{}, netip.Addr{}); matched {
		t.Fatalf("an unresolved destination matched an echo reply as %d", kind)
	}
}

// A link-local address is unique only inside its zone, so fe80::1%eth0 and
// fe80::1%eth1 name different hosts on different links.
func TestPeerAddrKeepsIPv6Zone(t *testing.T) {
	want := netip.MustParseAddr("fe80::1%eth0")
	cases := map[string]net.Addr{
		"udp ping socket": &net.UDPAddr{IP: net.ParseIP("fe80::1"), Zone: "eth0"},
		"raw socket":      &net.IPAddr{IP: net.ParseIP("fe80::1"), Zone: "eth0"},
	}
	for name, a := range cases {
		if got := peerAddr(a); got != want {
			t.Errorf("%s: peerAddr = %v, want %v", name, got, want)
		}
	}
	if got := peerAddr(&net.IPAddr{IP: net.ParseIP("fe80::1"), Zone: "eth1"}); got == want {
		t.Errorf("a reply off another link reduced to %v, the same value as %v", got, want)
	}
}

// The destination side of the comparison comes from net.ResolveIPAddr, so the
// zone check is inert unless resolution keeps the zone the operator wrote.
func TestResolvedDestinationKeepsItsZone(t *testing.T) {
	ip, err := net.ResolveIPAddr("ip6", "fe80::1%eth0")
	if err != nil {
		t.Skipf("resolve fe80::1%%eth0: %v", err)
	}
	if got, want := peerAddr(ip), netip.MustParseAddr("fe80::1%eth0"); got != want {
		t.Fatalf("resolved destination reduced to %v, want %v", got, want)
	}
}

// The walk holds one destination and expands it again to send. A zone dropped
// here probes a different link than the one the reply is checked against.
func TestSendAddrKeepsIPv6Zone(t *testing.T) {
	dst := peerAddr(&net.IPAddr{IP: net.ParseIP("fe80::1"), Zone: "eth0"})
	got := sendAddr(dst)
	if got.Zone != "eth0" || !got.IP.Equal(net.ParseIP("fe80::1")) {
		t.Fatalf("sendAddr = %v, want fe80::1%%eth0", got)
	}
	if back := peerAddr(got); back != dst {
		t.Fatalf("send address reduces to %v, not the destination %v it was built from", back, dst)
	}
}

// An echo reply is accepted only from the destination, and a link-local
// destination is not identified by its address alone: without the zone, a
// reply arriving over another interface answered for a host on a different
// link. Errors stay exempt, as they do for the address itself.
func TestMatchDatagramRequiresTheDestinationZone(t *testing.T) {
	const id, seq = 0x1234, 7
	dst := peerAddr(&net.IPAddr{IP: net.ParseIP("fe80::1"), Zone: "eth0"})
	echoReply, err := (&icmp.Message{
		Type: ipv6.ICMPTypeEchoReply, Body: &icmp.Echo{ID: id, Seq: seq, Data: []byte("x")},
	}).Marshal(nil)
	if err != nil {
		t.Fatal(err)
	}
	timeExceeded, err := (&icmp.Message{
		Type: ipv6.ICMPTypeTimeExceeded, Body: &icmp.TimeExceeded{Data: embeddedEcho(true, id, seq)},
	}).Marshal(nil)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		datagram []byte
		peer     net.Addr
		wantOK   bool
		wantAddr string
		wantKind replyKind
	}{
		{"echo from the destination's own zone", echoReply,
			&net.IPAddr{IP: net.ParseIP("fe80::1"), Zone: "eth0"}, true, "fe80::1%eth0", replyEcho},
		{"echo from the same address on another link", echoReply,
			&net.IPAddr{IP: net.ParseIP("fe80::1"), Zone: "eth1"}, false, "", replyNone},
		{"echo from the same address on another link, ping socket", echoReply,
			&net.UDPAddr{IP: net.ParseIP("fe80::1"), Zone: "eth1"}, false, "", replyNone},
		{"echo with no zone at all", echoReply,
			&net.IPAddr{IP: net.ParseIP("fe80::1")}, false, "", replyNone},
		{"time exceeded from another link still answers", timeExceeded,
			&net.IPAddr{IP: net.ParseIP("fe80::2"), Zone: "eth1"}, true, "fe80::2%eth1", replyTimeExceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := matchDatagram(tc.datagram, 58, tc.peer, true, id, seq, dst)
			if ok != tc.wantOK {
				t.Fatalf("matched = %v, want %v (%+v)", ok, tc.wantOK, got)
			}
			if got.addr != addrOrZero(tc.wantAddr) || got.kind != tc.wantKind {
				t.Fatalf("got addr %q kind %d, want %q %d", got.addr, got.kind, tc.wantAddr, tc.wantKind)
			}
		})
	}
}
