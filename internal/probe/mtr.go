package probe

import (
	"context"
	"errors"
	"time"
)

// MTR discovers the path to a target by sending ICMP echoes with increasing
// TTL and collecting intermediate routers' TimeExceeded replies. Each cycle
// runs `count` rounds, and every round walks TTL=1..maxTTL until it hits its
// own terminal, so a route that changes mid-cycle is followed instead of being
// clamped to the shortest path an earlier round saw.
//
// MTR requires raw ICMP sockets (CAP_NET_RAW). Unprivileged UDP ping sockets
// don't reliably surface ICMP errors from intermediate hops on Linux, so we
// deliberately don't fall back to them here.
type MTR struct {
	name    string
	timeout time.Duration
	maxTTL  int
	spacing time.Duration
	// trace is the injectable seam over traceHops, mirroring ICMP's.
	trace traceFunc
}

func NewMTR(name string, timeout time.Duration) *MTR {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return &MTR{name: name, timeout: timeout, maxTTL: 30, spacing: 50 * time.Millisecond, trace: traceHops}
}

func (m *MTR) Name() string { return m.name }

// maxRounds caps `count` for MTR cycles. Each round walks up to maxTTL hops;
// with cfg.Pings=20 and an unresponsive path that's 20 × 30 × timeout, which
// can blow past the cycle interval. 10 rounds is plenty for loss/latency
// estimates and stays well under a 5m interval in the worst case.
const maxRounds = 10

func (m *MTR) Probe(ctx context.Context, t Target, count int) (*Result, error) {
	if t.Host == "" {
		return nil, errors.New("mtr: host required")
	}
	if count > maxRounds {
		count = maxRounds
	}
	hops, reached, err := m.trace(ctx, t.Host, t.Family, count, m.maxTTL, m.timeout, m.spacing)
	if err != nil {
		return nil, err
	}

	result := &Result{Sent: count, Hops: hops}
	// Mirror the rows the target itself answered, never the deepest row: a
	// per-round walk can leave a silent intermediate below the target's echo,
	// and without an EchoReply anywhere the path's last hop is an intermediate
	// router whose latency and loss say nothing about the target.
	if reached {
		var rtts []time.Duration
		sent, lost := 0, 0
		for _, h := range hops {
			if !h.TargetReply {
				continue
			}
			rtts = append(rtts, h.RTTs...)
			sent += h.Sent
			lost += h.Lost
		}
		result.RTTs, result.Sent, result.LossCount = rtts, sent, lost
	} else {
		result.LossCount = count
	}
	return result, nil
}
