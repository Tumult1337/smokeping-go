package probe

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// traceHops runs an MTR-style path discovery: `rounds` passes of TTL-limited
// echoes from 1 to maxTTL. Aggregates each hop's RTTs, sent and lost counts.
// Requires a raw ICMP socket (CAP_NET_RAW). Returns an error if the raw socket
// cannot be opened — callers use errors.Is(err, errRawUnavailable) to distinguish
// permission failures from actual probe errors and skip trace gracefully.
//
// The second return value is true iff the target itself replied at least once
// during the trace. Callers that mirror per-hop stats as "target" stats (mtr.go)
// use this to avoid passing off an intermediate hop's numbers when the target
// was silent all the way to maxTTL.
func traceHops(ctx context.Context, host string, rounds, maxTTL int, timeout, spacing time.Duration) ([]Hop, bool, error) {
	if host == "" {
		return nil, false, errors.New("trace: host required")
	}
	ip, err := net.ResolveIPAddr("ip", host)
	if err != nil {
		return nil, false, fmt.Errorf("resolve %q: %w", host, err)
	}
	isV6 := ip.IP.To4() == nil
	conn, err := listenRaw(isV6)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", errRawUnavailable, err)
	}
	defer conn.Close()
	return traceOnConn(ctx, conn, ip, isV6, rounds, maxTTL, timeout, spacing)
}

// errRawUnavailable wraps the underlying OS error when a raw ICMP socket
// can't be opened (typically EPERM without CAP_NET_RAW). Callers that want to
// degrade gracefully (e.g., icmp probe) check this with errors.Is.
var errRawUnavailable = errors.New("raw icmp socket unavailable")

// traceOnConn is the core TTL-walk loop, separated from socket setup so the
// caller can supply a shared conn if it already has one open.
func traceOnConn(ctx context.Context, conn *icmp.PacketConn, ip *net.IPAddr, isV6 bool, rounds, maxTTL int, timeout, spacing time.Duration) ([]Hop, bool, error) {
	id := int(rand.Uint32() & 0xffff)

	type hopAgg struct {
		ip   string
		rtts []time.Duration
		sent int
		lost int
	}
	agg := make([]hopAgg, maxTTL+1)
	finalTTL := maxTTL
	reachedAny := false

	for round := range rounds {
		if ctx.Err() != nil {
			break
		}
		for ttl := 1; ttl <= finalTTL; ttl++ {
			if ctx.Err() != nil {
				break
			}
			seq := ((round * (maxTTL + 1)) + ttl) & 0xffff
			srcIP, rtt, reached, err := sendTTL(conn, ip, isV6, id, seq, ttl, timeout)
			agg[ttl].sent++
			if err != nil || srcIP == "" {
				agg[ttl].lost++
			} else {
				agg[ttl].rtts = append(agg[ttl].rtts, rtt)
				if agg[ttl].ip == "" {
					agg[ttl].ip = srcIP
				}
			}
			if reached && ttl < finalTTL {
				finalTTL = ttl
			}
			if reached {
				reachedAny = true
				break
			}
			if ttl < finalTTL {
				select {
				case <-ctx.Done():
				case <-time.After(spacing):
				}
			}
		}
	}

	var hops []Hop
	for ttl := 1; ttl <= finalTTL; ttl++ {
		h := agg[ttl]
		if h.sent == 0 {
			continue
		}
		hops = append(hops, Hop{
			Index: ttl,
			IP:    h.ip,
			RTTs:  h.rtts,
			Sent:  h.sent,
			Lost:  h.lost,
		})
	}
	return hops, reachedAny, nil
}

// sendTTL sends one echo at the given TTL and waits for either an EchoReply
// (we reached the target) or a TimeExceeded whose embedded packet matches our
// seq (an intermediate router). Replies for other sequences are ignored and
// reading continues until the per-probe deadline.
func sendTTL(conn *icmp.PacketConn, dst *net.IPAddr, isV6 bool, id, seq, ttl int, timeout time.Duration) (string, time.Duration, bool, error) {
	var msg icmp.Message
	if isV6 {
		msg = icmp.Message{Type: ipv6.ICMPTypeEchoRequest, Body: &icmp.Echo{ID: id, Seq: seq, Data: payload()}}
	} else {
		msg = icmp.Message{Type: ipv4.ICMPTypeEcho, Body: &icmp.Echo{ID: id, Seq: seq, Data: payload()}}
	}
	wire, err := msg.Marshal(nil)
	if err != nil {
		return "", 0, false, err
	}

	if isV6 {
		if p6 := conn.IPv6PacketConn(); p6 != nil {
			_ = p6.SetHopLimit(ttl)
		}
	} else {
		if p4 := conn.IPv4PacketConn(); p4 != nil {
			_ = p4.SetTTL(ttl)
		}
	}

	deadline := time.Now().Add(timeout)
	if err := conn.SetReadDeadline(deadline); err != nil {
		return "", 0, false, err
	}

	start := time.Now()
	if _, err := conn.WriteTo(wire, dst); err != nil {
		return "", 0, false, err
	}

	buf := make([]byte, 1500)
	proto := 1
	if isV6 {
		proto = 58
	}
	for {
		n, peer, err := conn.ReadFrom(buf)
		if err != nil {
			return "", 0, false, err
		}
		reply, perr := icmp.ParseMessage(proto, buf[:n])
		if perr != nil {
			continue
		}
		elapsed := time.Since(start)

		var peerIP string
		switch pa := peer.(type) {
		case *net.UDPAddr:
			peerIP = pa.IP.String()
		case *net.IPAddr:
			peerIP = pa.IP.String()
		}

		switch body := reply.Body.(type) {
		case *icmp.Echo:
			if body.Seq != seq {
				continue
			}
			return peerIP, elapsed, true, nil
		case *icmp.TimeExceeded:
			if embeddedSeq(body.Data, isV6) == seq {
				return peerIP, elapsed, false, nil
			}
		case *icmp.DstUnreach:
			if embeddedSeq(body.Data, isV6) == seq {
				return peerIP, elapsed, false, nil
			}
		}
	}
}

// embeddedSeq extracts the sequence number of the original echo request out of
// the IP+ICMP header quoted in an ICMP error message. Returns -1 if the data
// is too short to contain one.
func embeddedSeq(data []byte, isV6 bool) int {
	ihl := 40
	if !isV6 {
		if len(data) < 1 {
			return -1
		}
		ihl = max(int(data[0]&0x0f)*4, 20)
	}
	if len(data) < ihl+8 {
		return -1
	}
	hdr := data[ihl:]
	return int(hdr[6])<<8 | int(hdr[7])
}

func listenRaw(isV6 bool) (*icmp.PacketConn, error) {
	if isV6 {
		return icmp.ListenPacket("ip6:ipv6-icmp", "::")
	}
	return icmp.ListenPacket("ip4:icmp", "0.0.0.0")
}
