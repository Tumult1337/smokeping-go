package probe

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"net"
	"net/netip"
	"slices"
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
// The second return value counts the rounds the walk ran and the subset of
// them the target itself echoed in. Callers that mirror per-hop stats as
// "target" stats (mtr.go) report loss from those counts: a round is the unit
// of a trace, and hop rows cannot answer how many rounds reached the target
// once each round stops at its own terminal.
func traceHops(ctx context.Context, host, family string, rounds, maxTTL int, timeout, spacing time.Duration) ([]Hop, roundStats, error) {
	if host == "" {
		return nil, roundStats{}, errors.New("trace: host required")
	}
	ip, err := net.ResolveIPAddr(familyNetwork("ip", family), host)
	if err != nil {
		return nil, roundStats{}, fmt.Errorf("resolve %q: %w", host, err)
	}
	dst := peerAddr(ip)
	if !dst.IsValid() {
		return nil, roundStats{}, fmt.Errorf("resolve %q: unusable address %q", host, ip)
	}
	isV6 := dst.Is6()
	conn, err := listenRawFn(isV6)
	if err != nil {
		return nil, roundStats{}, classifyListenErr(err)
	}
	defer func() { _ = conn.Close() }()
	return traceOnConn(ctx, conn, dst, isV6, rounds, maxTTL, timeout, spacing)
}

// errRawUnavailable wraps the underlying OS error when a raw ICMP socket is
// refused for lack of permission (EPERM/EACCES without CAP_NET_RAW). Callers
// that want to degrade gracefully (e.g., icmp probe) check this with
// errors.Is; transient failures deliberately never carry it.
var errRawUnavailable = errors.New("raw icmp socket unavailable")

// listenRawFn is the injectable seam over listenRaw so tests can drive the
// error classification below without a socket.
var listenRawFn = listenRaw

// classifyListenErr reserves errRawUnavailable for permission failures: that
// class is logged once per process, so folding a transient EMFILE into it
// silences every later raw-socket failure.
func classifyListenErr(err error) error {
	if errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("%w: %w", errRawUnavailable, err)
	}
	return fmt.Errorf("open trace socket: %w", err)
}

const unreachOther = "unreachable-other"

// unreachLabels is the closed set the pipeline stores and serves; the wire
// ingest folds anything else to unreachOther so a hostile slave cannot mint
// dictionary entries or ship arbitrary text to the UI.
var unreachLabels = map[string]struct{}{
	"net-unreachable": {}, "host-unreachable": {}, "proto-unreachable": {},
	"port-unreachable": {}, "frag-needed": {}, "source-route-failed": {},
	"admin-prohibited": {}, "no-route": {}, "beyond-scope": {},
	"addr-unreachable": {}, "policy-fail": {}, "reject-route": {},
	unreachOther: {},
}

func CanonicalUnreach(s string) string {
	if s == "" {
		return ""
	}
	if _, ok := unreachLabels[s]; ok {
		return s
	}
	return unreachOther
}

// unreachLabel maps RFC 792 type-3 and RFC 4443 type-1 codes onto the closed
// set, normalized across families.
func unreachLabel(isV6 bool, code int) string {
	if isV6 {
		switch code {
		case 0:
			return "no-route"
		case 1:
			return "admin-prohibited"
		case 2:
			return "beyond-scope"
		case 3:
			return "addr-unreachable"
		case 4:
			return "port-unreachable"
		case 5:
			return "policy-fail"
		case 6:
			return "reject-route"
		}
		return unreachOther
	}
	switch code {
	case 0:
		return "net-unreachable"
	case 1:
		return "host-unreachable"
	case 2:
		return "proto-unreachable"
	case 3:
		return "port-unreachable"
	case 4:
		return "frag-needed"
	case 5:
		return "source-route-failed"
	case 9, 10, 13:
		return "admin-prohibited"
	}
	return unreachOther
}

// replyKind classifies what answered a TTL probe: nothing, an intermediate
// router's TimeExceeded, a gateway's unreachable, or the target's own echo
// reply.
type replyKind uint8

const (
	replyNone replyKind = iota
	replyTimeExceeded
	replyUnreachable
	replyEcho
)

// ttlReply is one TTL probe's outcome; addr is empty unless something matched.
type ttlReply struct {
	addr    string
	rtt     time.Duration
	kind    replyKind
	unreach string
	err     error
}

// stepFunc sends one probe at ttl during round and reports what answered.
type stepFunc func(ctx context.Context, round, ttl int) ttlReply

// roundStats counts the trace rounds that sent at least one probe and the
// subset of them the target echoed in. Attempted trails the requested round
// count when the cycle deadline cuts the walk short.
type roundStats struct {
	attempted int
	reached   int
}

// walkRounds runs the TTL walk over an injected per-probe step so tests drive
// this exact loop. Each round walks 1..maxTTL and stops at its own terminal —
// an echo reply or a gateway's unreachable, which is the real end of the path
// — so a route that changes mid-cycle is followed rather than clamped to the
// shortest path seen. Every responder at a TTL gets its own row, and each row
// that echoed is marked TargetReply because the target's row is no longer
// guaranteed to be the deepest.
func walkRounds(ctx context.Context, rounds, maxTTL int, spacing time.Duration, step stepFunc) ([]Hop, roundStats) {
	type respondent struct {
		addr        string
		targetReply bool
		unreach     string
		rtts        []time.Duration
		sent        int
	}
	type ttlAgg struct {
		rows   []respondent
		losses int
	}
	agg := make([]ttlAgg, maxTTL+1)
	var stats roundStats

	for round := range rounds {
		if ctx.Err() != nil {
			break
		}
		for ttl := 1; ttl <= maxTTL; ttl++ {
			if ctx.Err() != nil {
				break
			}
			r := step(ctx, round, ttl)
			stats.attempted = round + 1
			if r.err != nil || r.addr == "" {
				agg[ttl].losses++
			} else {
				i := slices.IndexFunc(agg[ttl].rows, func(row respondent) bool { return row.addr == r.addr })
				if i < 0 {
					agg[ttl].rows = append(agg[ttl].rows, respondent{addr: r.addr})
					i = len(agg[ttl].rows) - 1
				}
				row := &agg[ttl].rows[i]
				row.rtts = append(row.rtts, r.rtt)
				row.sent++
				if r.kind == replyEcho {
					row.targetReply = true
				}
				if row.unreach == "" {
					row.unreach = r.unreach
				}
			}
			if r.kind == replyEcho {
				stats.reached++
			}
			if r.kind == replyEcho || r.kind == replyUnreachable {
				break
			}
			if ttl < maxTTL {
				select {
				case <-ctx.Done():
				case <-time.After(spacing):
				}
			}
		}
	}

	var hops []Hop
	for ttl := 1; ttl <= maxTTL; ttl++ {
		a := agg[ttl]
		if len(a.rows) == 0 {
			if a.losses > 0 {
				hops = append(hops, Hop{Index: ttl, Sent: a.losses, Lost: a.losses})
			}
			continue
		}
		for i, row := range a.rows {
			h := Hop{
				Index:       ttl,
				IP:          row.addr,
				TargetReply: row.targetReply,
				Unreach:     row.unreach,
				RTTs:        row.rtts,
				Sent:        row.sent,
			}
			// Loss at a TTL has no responder to blame, so it stays on the
			// first-seen row and single-responder numbers keep their old shape.
			if i == 0 {
				h.Sent += a.losses
				h.Lost = a.losses
			}
			hops = append(hops, h)
		}
	}
	return hops, stats
}

// traceOnConn is the core TTL-walk loop, separated from socket setup so the
// caller can supply a shared conn if it already has one open.
func traceOnConn(ctx context.Context, conn *icmp.PacketConn, dst netip.Addr, isV6 bool, rounds, maxTTL int, timeout, spacing time.Duration) ([]Hop, roundStats, error) {
	id := int(rand.Uint32() & 0xffff)
	step := func(ctx context.Context, round, ttl int) ttlReply {
		seq := ((round * (maxTTL + 1)) + ttl) & 0xffff
		return sendTTL(ctx, conn, dst, isV6, id, seq, ttl, timeout)
	}
	hops, stats := walkRounds(ctx, rounds, maxTTL, spacing, step)
	return hops, stats, nil
}

// peerAddr reduces a socket address to a comparable value. The socket type
// decides which shape arrives: an unprivileged ping socket reports
// *net.UDPAddr, a raw one *net.IPAddr. An address of any other shape resolves
// to the invalid zero Addr, which matches no destination. Both sides of the
// destination check go through here so neither can drop a zone the other keeps.
func peerAddr(a net.Addr) netip.Addr {
	switch pa := a.(type) {
	case *net.UDPAddr:
		return addrFromIP(pa.IP, pa.Zone)
	case *net.IPAddr:
		return addrFromIP(pa.IP, pa.Zone)
	}
	return netip.Addr{}
}

// sendAddr expands the destination back into the shape conn.WriteTo needs.
// The walk sends to and compares against one netip.Addr, so a reply can never
// be matched against an address the probe never went to.
func sendAddr(dst netip.Addr) *net.IPAddr {
	return &net.IPAddr{IP: dst.AsSlice(), Zone: dst.Zone()}
}

// addrFromIP carries the zone because a link-local address names a different
// host per link: an echo from fe80::1%eth1 does not answer a probe of
// fe80::1%eth0. WithZone is a no-op on IPv4, where there are no zones.
func addrFromIP(ip net.IP, zone string) netip.Addr {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return netip.Addr{}
	}
	return addr.Unmap().WithZone(zone)
}

// classifyReply reports whether a reply is an answer to our own probe at
// (id, seq) and what kind it is. A raw ICMP socket receives every reply on the
// host, including anything an off-path attacker can spoof, so nothing is
// classified before id and seq match: concurrent traces reuse the short seq
// range, and an Echo Request parses to the same body as an Echo Reply, which
// would otherwise read as reaching the target.
//
// An echo reply must additionally come from dst. Type, id and seq are all
// visible to any router on the path, so one of them can answer from its own
// address; the round then stopped there, the row was marked TargetReply, and
// MTR reported a target that never replied. Errors are exempt: TimeExceeded
// and unreachable legitimately come from routers along the path.
func classifyReply(reply *icmp.Message, isV6 bool, id, seq int, peer, dst netip.Addr) (bool, replyKind, string) {
	switch body := reply.Body.(type) {
	case *icmp.Echo:
		if reply.Type != ipv4.ICMPTypeEchoReply && reply.Type != ipv6.ICMPTypeEchoReply {
			return false, replyNone, ""
		}
		if body.ID != id || body.Seq != seq {
			return false, replyNone, ""
		}
		if !dst.IsValid() || peer != dst {
			return false, replyNone, ""
		}
		return true, replyEcho, ""
	case *icmp.TimeExceeded:
		if eid, eseq := embeddedIDSeq(body.Data, isV6); eid == id && eseq == seq {
			return true, replyTimeExceeded, ""
		}
	case *icmp.DstUnreach:
		if eid, eseq := embeddedIDSeq(body.Data, isV6); eid == id && eseq == seq {
			return true, replyUnreachable, unreachLabel(isV6, reply.Code)
		}
	}
	return false, replyNone, ""
}

// sendTTL sends one echo at the given TTL and waits for the first reply
// classifyReply matches: the target's EchoReply, an intermediate router's
// TimeExceeded, or a gateway's unreachable. Everything else is ignored and
// reading continues until the per-probe deadline.
func sendTTL(ctx context.Context, conn *icmp.PacketConn, dst netip.Addr, isV6 bool, id, seq, ttl int, timeout time.Duration) ttlReply {
	var msg icmp.Message
	if isV6 {
		msg = icmp.Message{Type: ipv6.ICMPTypeEchoRequest, Body: &icmp.Echo{ID: id, Seq: seq, Data: icmpPayload}}
	} else {
		msg = icmp.Message{Type: ipv4.ICMPTypeEcho, Body: &icmp.Echo{ID: id, Seq: seq, Data: icmpPayload}}
	}
	wire, err := msg.Marshal(nil)
	if err != nil {
		return ttlReply{err: err}
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

	dl := time.Now().Add(timeout)
	if ctxDL, ok := ctx.Deadline(); ok && ctxDL.Before(dl) {
		dl = ctxDL
	}
	if err := conn.SetReadDeadline(dl); err != nil {
		return ttlReply{err: err}
	}

	start := time.Now()
	if _, err := conn.WriteTo(wire, sendAddr(dst)); err != nil {
		return ttlReply{err: err}
	}

	bufp := icmpBufPool.Get().(*[]byte)
	defer icmpBufPool.Put(bufp)
	buf := *bufp
	proto := 1
	if isV6 {
		proto = 58
	}
	for {
		n, peer, err := conn.ReadFrom(buf)
		if err != nil {
			return ttlReply{err: err}
		}
		elapsed := time.Since(start)
		if r, ok := matchDatagram(buf[:n], proto, peer, isV6, id, seq, dst); ok {
			r.rtt = elapsed
			return r
		}
	}
}

// matchDatagram decides whether one received datagram answers our probe at
// (id, seq) from dst. Every byte and the source address are attacker-supplied
// on a raw socket, so this is the trust boundary of the whole walk — it lives
// apart from sendTTL's read loop so it can be driven with hostile input.
func matchDatagram(datagram []byte, proto int, peer net.Addr, isV6 bool, id, seq int, dst netip.Addr) (ttlReply, bool) {
	reply, err := icmp.ParseMessage(proto, datagram)
	if err != nil {
		return ttlReply{}, false
	}
	// A datagram whose source we cannot resolve names no hop and matches no
	// destination, so it is not our answer.
	peerIP := peerAddr(peer)
	if !peerIP.IsValid() {
		return ttlReply{}, false
	}
	matched, kind, unreach := classifyReply(reply, isV6, id, seq, peerIP, dst)
	if !matched {
		return ttlReply{}, false
	}
	return ttlReply{addr: peerIP.String(), kind: kind, unreach: unreach}, true
}

// embeddedIDSeq extracts the ICMP id and sequence of the original echo request
// out of the IP+ICMP header quoted in an ICMP error message. Returns (-1, -1)
// if the data is too short to contain them.
func embeddedIDSeq(data []byte, isV6 bool) (int, int) {
	ihl := 40
	if !isV6 {
		if len(data) < 1 {
			return -1, -1
		}
		ihl = max(int(data[0]&0x0f)*4, 20)
	}
	if len(data) < ihl+8 {
		return -1, -1
	}
	hdr := data[ihl:]
	id := int(hdr[4])<<8 | int(hdr[5])
	seq := int(hdr[6])<<8 | int(hdr[7])
	return id, seq
}

func listenRaw(isV6 bool) (*icmp.PacketConn, error) {
	if isV6 {
		return icmp.ListenPacket("ip6:ipv6-icmp", "::")
	}
	return icmp.ListenPacket("ip4:icmp", "0.0.0.0")
}
