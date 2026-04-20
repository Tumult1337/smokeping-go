package probe

import (
	"context"
	"errors"
	"time"
)

// MTR discovers the path to a target by sending ICMP echoes with increasing
// TTL and collecting intermediate routers' TimeExceeded replies. Each cycle
// runs `count` rounds, each round walking TTL=1..finalTTL once. finalTTL is
// pinned as soon as the target itself answers, so once the path is known we
// don't waste probes beyond it.
//
// MTR requires raw ICMP sockets (CAP_NET_RAW). Unprivileged UDP ping sockets
// don't reliably surface ICMP errors from intermediate hops on Linux, so we
// deliberately don't fall back to them here.
type MTR struct {
	name    string
	timeout time.Duration
	maxTTL  int
	spacing time.Duration
}

func NewMTR(name string, timeout time.Duration) *MTR {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return &MTR{name: name, timeout: timeout, maxTTL: 30, spacing: 50 * time.Millisecond}
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
	hops, reached, err := traceHops(ctx, t.Host, t.Family, count, m.maxTTL, m.timeout, m.spacing)
	if err != nil {
		return nil, err
	}

	result := &Result{Sent: count, Hops: hops}
	// Only mirror the last hop as "target" stats when we actually reached the
	// target. If we ran out of TTL without an EchoReply, the final hop is an
	// intermediate router and its latency/loss is not representative of the
	// target — in that case report full loss.
	if reached {
		if n := len(result.Hops); n > 0 {
			last := result.Hops[n-1]
			result.RTTs = last.RTTs
			result.Sent = last.Sent
			result.LossCount = last.Lost
		}
	} else {
		result.LossCount = count
	}
	return result, nil
}
