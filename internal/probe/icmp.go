package probe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// ICMP sends count echo requests spaced by interProbeDelay and collects replies.
// Uses unprivileged UDP ping sockets on Linux (net="udp4"/"udp6") when available,
// falling back to raw ICMP (net="ip4:icmp") which requires CAP_NET_RAW.
//
// After the echo batch completes, ICMP opportunistically runs a short MTR-style
// path trace (traceRounds rounds over at most traceMaxTTL hops) so every icmp
// target gets a hops view for free. The trace needs a raw socket — when that
// fails (e.g., no CAP_NET_RAW), the probe still returns normal ping stats and
// just leaves Hops unset.
type ICMP struct {
	name    string
	timeout time.Duration
	// inter-probe spacing within a cycle
	spacing time.Duration
	// trace parameters — small rounds count keeps the trace well under one
	// cycle even when many hops time out.
	traceRounds  int
	traceMaxTTL  int
	traceTimeout time.Duration
	traceSpacing time.Duration
}

func NewICMP(name string, timeout time.Duration) *ICMP {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &ICMP{
		name:         name,
		timeout:      timeout,
		spacing:      200 * time.Millisecond,
		traceRounds:  3,
		traceMaxTTL:  30,
		traceTimeout: time.Second,
		traceSpacing: 50 * time.Millisecond,
	}
}

func (i *ICMP) Name() string { return i.name }

func (i *ICMP) Probe(ctx context.Context, t Target, count int) (*Result, error) {
	if t.Host == "" {
		return nil, errors.New("icmp: host required")
	}
	ip, err := net.ResolveIPAddr(familyNetwork("ip", t.Family), t.Host)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", t.Host, err)
	}

	isV6 := ip.IP.To4() == nil
	conn, err := listen(isV6)
	if err != nil {
		return nil, fmt.Errorf("listen icmp: %w", err)
	}
	defer conn.Close()

	// Each cycle uses a fresh id/base-seq to avoid cross-cycle reply confusion.
	id := int(rand.Uint32() & 0xffff)
	baseSeq := int(rand.Uint32() & 0xffff)
	// Sent counts actual attempts, not the requested count, so that a
	// context-cancelled mid-cycle (shutdown, reload) reports LossPct truthfully
	// instead of e.g. 11/20 = 55% when in reality 11 of 11 attempts failed.
	result := &Result{RTTs: make([]time.Duration, 0, count)}

	for n := range count {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		seq := (baseSeq + n) & 0xffff
		result.Sent++
		rtt, err := sendOne(ctx, conn, ip, isV6, id, seq, i.timeout)
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

	// Opportunistic path trace. If we can't get a raw socket (no CAP_NET_RAW)
	// we skip — the caller only loses the hops view, not pings. We log the
	// raw-unavailable error once globally so a deploy without setcap is
	// visible at startup rather than silently missing MTR for every target.
	// The "reached" signal is irrelevant here: echo latency is already measured
	// from the target; we only want the hops list.
	if hops, _, terr := traceHops(ctx, t.Host, t.Family, i.traceRounds, i.traceMaxTTL, i.traceTimeout, i.traceSpacing); terr == nil {
		result.Hops = hops
	} else if errors.Is(terr, errRawUnavailable) {
		logRawUnavailableOnce(terr)
	}
	return result, nil
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
		msg = icmp.Message{Type: ipv6.ICMPTypeEchoRequest, Body: &icmp.Echo{ID: id, Seq: seq, Data: payload()}}
	} else {
		msg = icmp.Message{Type: ipv4.ICMPTypeEcho, Body: &icmp.Echo{ID: id, Seq: seq, Data: payload()}}
	}
	wire, err := msg.Marshal(nil)
	if err != nil {
		return 0, err
	}

	// Destination: for unprivileged UDP sockets we need a UDPAddr; for raw ICMP an IPAddr.
	var addr net.Addr = dst
	if ua, ok := asUDPAddr(conn); ok {
		addr = &net.UDPAddr{IP: dst.IP, Zone: ua.Zone}
	}

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

	buf := make([]byte, 1500)
	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) || isTimeout(err) {
				return 0, err
			}
			return 0, err
		}
		proto := 1 // ICMPv4
		if isV6 {
			proto = 58
		}
		reply, err := icmp.ParseMessage(proto, buf[:n])
		if err != nil {
			continue
		}
		echo, ok := reply.Body.(*icmp.Echo)
		if !ok {
			continue
		}
		// On unprivileged (UDP) sockets the kernel rewrites ID to the source port,
		// so id may not match what we sent — gate on seq only in that case.
		if echo.Seq != seq {
			continue
		}
		return time.Since(start), nil
	}
}

func asUDPAddr(conn *icmp.PacketConn) (*net.UDPAddr, bool) {
	la, ok := conn.LocalAddr().(*net.UDPAddr)
	return la, ok
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func payload() []byte {
	// Fixed 56 bytes like ping(8); content arbitrary but stable aids debugging.
	b := make([]byte, 56)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}
