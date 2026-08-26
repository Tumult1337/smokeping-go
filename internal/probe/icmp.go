package probe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// traceFunc matches traceHops' signature. Injected on ICMP so tests can
// substitute a spy and assert the noTrace gate actually calls (or doesn't
// call) tracing — without needing CAP_NET_RAW.
type traceFunc func(ctx context.Context, host, family string, rounds, maxTTL int, timeout, spacing time.Duration) ([]Hop, roundStats, error)

// The TTL walk's default bounds, named so the sequence space it reserves is
// derived in one place rather than restated.
const (
	defaultTraceRounds = 3
	defaultTraceMaxTTL = 30
)

// sendFunc is the injectable seam over sendOne, mirroring trace, so tests can
// drive the echo loop's budget arithmetic without a live socket.
type sendFunc func(ctx context.Context, conn *icmp.PacketConn, dst *net.IPAddr, isV6 bool, id, seq int, timeout time.Duration) (time.Duration, error)

// ICMP sends count echo requests spaced by spacing and collects replies.
// Uses unprivileged UDP ping sockets on Linux (net="udp4"/"udp6") when available,
// falling back to raw ICMP (net="ip4:icmp") which requires CAP_NET_RAW.
//
// Concurrently with the echo batch, ICMP opportunistically runs a short MTR-style
// path trace (traceRounds rounds over at most traceMaxTTL hops) so every icmp
// target gets a hops view for free. The trace needs a raw socket — when that
// fails (e.g., no CAP_NET_RAW), the probe still returns normal ping stats and
// just leaves Hops unset.
type ICMP struct {
	name    string
	timeout time.Duration
	// inter-probe spacing within a cycle
	spacing time.Duration
	// noTrace disables the opportunistic TTL walk below, from config.Probe.NoTrace.
	noTrace bool
	// trace parameters — small rounds count keeps the trace well under one
	// cycle even when many hops time out.
	traceRounds  int
	traceMaxTTL  int
	traceTimeout time.Duration
	traceSpacing time.Duration
	// trace is the injectable seam over traceHops; defaults to the real
	// implementation and is only overridden in tests.
	trace traceFunc
	send  sendFunc
}

func NewICMP(name string, timeout time.Duration, noTrace bool) *ICMP {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &ICMP{
		name:         name,
		timeout:      timeout,
		spacing:      config.ICMPPingSpacing,
		noTrace:      noTrace,
		traceRounds:  defaultTraceRounds,
		traceMaxTTL:  defaultTraceMaxTTL,
		traceTimeout: time.Second,
		traceSpacing: 50 * time.Millisecond,
		trace:        traceHops,
		send:         sendOne,
	}
}

func (i *ICMP) Name() string { return i.name }

// pingBudget divides what is left of the cycle among the pings left so a ping
// that answers fast returns its unused share to the ones after it, capped by
// the configured timeout and 0 when no further ping fits.
func pingBudget(remainingCycle time.Duration, remainingPings int, spacingLeft, configured time.Duration) time.Duration {
	if remainingPings <= 0 {
		return 0
	}
	budget := remainingCycle - spacingLeft
	if budget <= 0 {
		return 0
	}
	if per := budget / time.Duration(remainingPings); per < configured {
		return per
	}
	return configured
}

// echoTimeout is pingBudget for echo n of count, accounting for the spacing
// still owed to the pings after it.
func (i *ICMP) echoTimeout(remainingCycle time.Duration, count, n int) time.Duration {
	return pingBudget(remainingCycle, count-n, time.Duration(count-1-n)*i.spacing, i.timeout)
}

// traceResult carries the TTL walk's outcome back from its goroutine. The
// channel is buffered so the goroutine never blocks even if a join is missed.
type traceResult struct {
	hops []Hop
	err  error
}

// errTracePanic wraps a panic recovered inside the TTL walk's goroutine, which
// Go's per-goroutine recovery puts out of reach of the scheduler's per-cycle
// recover — left alone it kills the process.
var errTracePanic = errors.New("trace goroutine panicked")

func (i *ICMP) startTrace(ctx context.Context, t Target) <-chan traceResult {
	ch := make(chan traceResult, 1)
	go func() {
		defer func() {
			if v := recover(); v != nil {
				ch <- traceResult{err: fmt.Errorf("%w: %v", errTracePanic, v)}
			}
		}()
		hops, _, err := i.trace(ctx, t.Host, t.Family, i.traceRounds, i.traceMaxTTL, i.traceTimeout, i.traceSpacing)
		ch <- traceResult{hops: hops, err: err}
	}()
	return ch
}

// The echo batch is placed above the walk's sequence window, so config's ping
// ceiling must fit what the walk leaves; a deeper default walk makes this
// constant negative and fails the build rather than silently folding echo
// sequence numbers back into the walk's range.
const _ uint = 1<<16 - defaultTraceRounds*(defaultTraceMaxTTL+1) - config.MaxPingsPerCycle

// traceSeqCeil is one past the largest sequence number the TTL walk can emit,
// derived from the walk's own round and TTL bounds in trace.go.
func (i *ICMP) traceSeqCeil() int { return i.traceRounds * (i.traceMaxTTL + 1) }

// echoDestination picks the address form the socket accepts: an unprivileged
// ping socket wants a UDPAddr, a raw one an IPAddr. Both carry dst.Zone.
// Taking the zone from the socket instead dropped it, because listen binds the
// ping socket to the wildcard address — sendto then answered EINVAL on every
// ping to a link-local target and it read 100% loss, while the walk's sendAddr
// and matchEchoReply's comparison both used the zoned form.
func echoDestination(isUDP bool, dst *net.IPAddr) net.Addr {
	if isUDP {
		return &net.UDPAddr{IP: dst.IP, Zone: dst.Zone}
	}
	return dst
}

// echoBaseSeq keeps the batch's window [base, base+count) above the walk's
// sequence range and clear of the 16-bit wrap that would fold it back in — the
// two now run at once, and a colliding seq makes sendTTL report the target
// reached at that TTL and truncates the hop list.
func (i *ICMP) echoBaseSeq(count int) int {
	lo := i.traceSeqCeil()
	span := (1 << 16) - lo - count + 1
	if span <= 0 {
		return lo
	}
	return lo + int(rand.Uint32()%uint32(span))
}

func (i *ICMP) Probe(ctx context.Context, t Target, count int) (*Result, error) {
	if t.Host == "" {
		return nil, errors.New("icmp: host required")
	}
	ip, err := resolveIPAddr(ctx, familyNetwork("ip", t.Family), t.Host)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", t.Host, err)
	}

	isV6 := ip.IP.To4() == nil
	conn, err := listen(isV6)
	if err != nil {
		return nil, fmt.Errorf("listen icmp: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// Each cycle uses a fresh id/base-seq to avoid cross-cycle reply confusion.
	id := int(rand.Uint32() & 0xffff)
	baseSeq := i.echoBaseSeq(count)
	if err := ctx.Err(); err != nil {
		return &Result{}, err
	}
	// Sent counts actual attempts, not the requested count, so that a
	// context-cancelled mid-cycle (shutdown, reload) reports LossPct truthfully
	// instead of e.g. 11/20 = 55% when in reality 11 of 11 attempts failed.
	result := &Result{RTTs: make([]time.Duration, 0, count)}

	// The walk shares the cycle deadline with the echo batch. Run after it, a
	// loss-saturated batch left nothing behind and the walk returned zero hops
	// on exactly the cycles it exists for; concurrent, the cycle costs
	// max(echo, trace) and the walk keeps the whole interval.
	var traced <-chan traceResult
	if !i.noTrace {
		traced = i.startTrace(ctx, t)
	}
	// Deferred so the early return below still joins: an orphaned trace
	// goroutine outlives its cycle holding a raw ICMP socket.
	defer func() {
		if traced == nil {
			return
		}
		tr := <-traced
		switch {
		case tr.err == nil:
			result.Hops = tr.hops
		case errors.Is(tr.err, errTracePanic):
			slog.Error("icmp trace panic recovered", "probe", i.name, "host", t.Host, "panic", tr.err)
		case errors.Is(tr.err, errRawUnavailable):
			logRawUnavailableOnce(tr.err)
		default:
			if ok, suppressed := traceErrLogAllowed(time.Since(traceErrEpoch)); ok {
				slog.Warn("icmp trace error", "probe", i.name, "host", t.Host,
					"suppressed", suppressed, "err", tr.err)
			}
		}
	}()

	cycleDeadline, bounded := ctx.Deadline()

	for n := range count {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		timeout := i.timeout
		if bounded {
			// Sent counts attempts, so stopping short here under-reports
			// truthfully rather than recording a loss the cycle never had the
			// budget to send.
			if timeout = i.echoTimeout(time.Until(cycleDeadline), count, n); timeout <= 0 {
				break
			}
		}
		seq := (baseSeq + n) & 0xffff
		result.Sent++
		rtt, err := i.send(ctx, conn, ip, isV6, id, seq, timeout)
		if err != nil {
			result.LossCount++
		} else {
			result.RTTs = append(result.RTTs, rtt)
		}
		if n < count-1 {
			select {
			case <-ctx.Done():
				return result, ctx.Err()
			case <-time.After(i.spacing):
			}
		}
	}

	return result, nil
}

// traceErrLogEvery throttles transient trace-failure warns process-wide; at
// the deployed 122-target/20s install an unthrottled fd-exhaustion emits ~6
// lines a second and buries the incident it reports.
const traceErrLogEvery = 1 * time.Minute

var (
	traceErrEpoch      = time.Now()
	traceErrLastLog    atomic.Int64
	traceErrSuppressed atomic.Uint64
)

// traceErrLogAllowed reports whether a warn may be emitted at sinceStart
// elapsed (monotonic, from traceErrEpoch — never wall clock; time.Since is
// strictly positive, and 0 is the never-logged sentinel), admitting at most
// one per traceErrLogEvery. When allowed it returns and resets the count of
// warns the window swallowed, so the emitted line can carry suppressed=N —
// a throttle without a count is a blind window.
func traceErrLogAllowed(sinceStart time.Duration) (bool, uint64) {
	for {
		last := traceErrLastLog.Load()
		if last != 0 && sinceStart-time.Duration(last) < traceErrLogEvery {
			traceErrSuppressed.Add(1)
			return false, 0
		}
		if traceErrLastLog.CompareAndSwap(last, int64(sinceStart)) {
			return true, traceErrSuppressed.Swap(0)
		}
	}
}

var rawUnavailableOnce sync.Once

func logRawUnavailableOnce(err error) {
	rawUnavailableOnce.Do(func() {
		slog.Warn("icmp trace disabled — raw socket unavailable; run `make setcap` for MTR hops",
			"err", err)
	})
}

func listen(isV6 bool) (*icmp.PacketConn, error) {
	// Prefer unprivileged ping sockets; fall back to raw.
	if isV6 {
		if c, err := icmp.ListenPacket("udp6", "::"); err == nil {
			return c, nil
		}
		return icmp.ListenPacket("ip6:ipv6-icmp", "::")
	}
	if c, err := icmp.ListenPacket("udp4", "0.0.0.0"); err == nil {
		return c, nil
	}
	return icmp.ListenPacket("ip4:icmp", "0.0.0.0")
}

func sendOne(ctx context.Context, conn *icmp.PacketConn, dst *net.IPAddr, isV6 bool, id, seq int, timeout time.Duration) (time.Duration, error) {
	var msg icmp.Message
	if isV6 {
		msg = icmp.Message{Type: ipv6.ICMPTypeEchoRequest, Body: &icmp.Echo{ID: id, Seq: seq, Data: icmpPayload}}
	} else {
		msg = icmp.Message{Type: ipv4.ICMPTypeEcho, Body: &icmp.Echo{ID: id, Seq: seq, Data: icmpPayload}}
	}
	wire, err := msg.Marshal(nil)
	if err != nil {
		return 0, err
	}

	_, isUDP := asUDPAddr(conn)
	addr := echoDestination(isUDP, dst)

	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > timeout {
		deadline = time.Now().Add(timeout)
	}
	if err := conn.SetReadDeadline(deadline); err != nil {
		return 0, err
	}

	start := time.Now()
	if _, err := conn.WriteTo(wire, addr); err != nil {
		return 0, err
	}

	bufp := icmpBufPool.Get().(*[]byte)
	defer icmpBufPool.Put(bufp)
	buf := *bufp
	proto := 1 // ICMPv4
	if isV6 {
		proto = 58
	}
	want := peerAddr(dst)
	for {
		n, peer, err := conn.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) || isTimeout(err) {
				return 0, err
			}
			return 0, err
		}
		if matchEchoReply(buf[:n], proto, peer, isUDP, id, seq, want) {
			return time.Since(start), nil
		}
	}
}

// matchEchoReply decides whether one received datagram is the reply to our
// echo at (id, seq) from dst itself. Like matchDatagram it is the read path's
// trust boundary — bytes and source address both come off the wire — and it
// sits apart from sendOne's loop so hostile input can drive it without a
// socket.
func matchEchoReply(datagram []byte, proto int, peer net.Addr, isUDP bool, id, seq int, dst netip.Addr) bool {
	reply, err := icmp.ParseMessage(proto, datagram)
	if err != nil {
		return false
	}
	// An Echo Request parses to the same body as a Reply, so a spoofed
	// request with a matching seq would read as a successful ping on the
	// raw-socket path (UDP ping sockets are kernel-demuxed).
	if reply.Type != ipv4.ICMPTypeEchoReply && reply.Type != ipv6.ICMPTypeEchoReply {
		return false
	}
	echo, ok := reply.Body.(*icmp.Echo)
	if !ok {
		return false
	}
	// On unprivileged (UDP) sockets the kernel rewrites ID to the source
	// port and demuxes replies per-socket, so id won't match what we sent —
	// gate on seq only. On raw sockets the kernel delivers every ICMP echo
	// reply to every raw socket, so a concurrent target's reply with a
	// colliding seq would be mis-accepted; there id is the discriminator
	// (matching sendTTL), so enforce it.
	if echo.Seq != seq || (!isUDP && echo.ID != id) {
		return false
	}
	// Seq (and id) are visible to any on-path router, so one that answers
	// echoes from its own address made a fully-down target read 0% loss;
	// only a reply from the destination itself is the target's. An invalid
	// dst matches nothing — fail closed, exactly as classifyReply does.
	return dst.IsValid() && peerAddr(peer) == dst
}

func asUDPAddr(conn *icmp.PacketConn) (*net.UDPAddr, bool) {
	la, ok := conn.LocalAddr().(*net.UDPAddr)
	return la, ok
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// icmpPayload is the fixed 56-byte echo body (like ping(8); content arbitrary
// but stable aids debugging). Built once and shared read-only across all sends
// — icmp.Message.Marshal copies the body into its own buffer, so concurrent
// sends never mutate it. Hoisted out of the per-ping hot path.
var icmpPayload = func() []byte {
	b := make([]byte, 56)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}()

// icmpBufPool recycles 1500-byte receive buffers across probes to avoid a
// heap allocation per ping / per TTL-step. 1500 covers the largest reply we
// parse (an ICMP error quoting an IPv6 header + 8 bytes). The buffer never
// escapes the send/receive call, so pooling is safe.
var icmpBufPool = sync.Pool{New: func() any { b := make([]byte, 1500); return &b }}
