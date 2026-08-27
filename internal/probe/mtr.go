package probe

import (
	"context"
	"errors"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
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
	return &MTR{name: name, timeout: timeout, maxTTL: maxTTL, spacing: 50 * time.Millisecond, trace: traceHops}
}

func (m *MTR) Name() string { return m.name }

// maxRounds caps `count` for MTR cycles. Each round walks up to maxTTL hops;
// with cfg.Pings=20 and an unresponsive path that's 20 × 30 × timeout, which
// can blow past the cycle interval. 10 rounds is plenty for loss/latency
// estimates and stays well under a 5m interval in the worst case.
const maxRounds = 10

// maxTTL is the deepest TTL one round walks. A const rather than the literal it
// was, so the assertions below can name it.
const maxTTL = 30

// config.MaxHopRowsPerCycle is derived from these two, and cluster ingest
// refuses a batch above twice it — so a walk that outgrew the bound would have
// its own legitimate output refused by the master. Negative fails the build,
// which is what the AST-parsing mirror test in internal/config was standing in
// for. The icmp probe's opportunistic walk feeds the same probe_hop rows, so it
// is pinned here too.
const (
	_ uint = config.MaxTraceRounds - maxRounds
	_ uint = config.MaxTraceTTL - maxTTL
	_ uint = config.MaxTraceRounds - defaultTraceRounds
	_ uint = config.MaxTraceTTL - defaultTraceMaxTTL
)

func (m *MTR) Probe(ctx context.Context, t Target, count int) (*Result, error) {
	if t.Host == "" {
		return nil, errors.New("mtr: host required")
	}
	if count > maxRounds {
		count = maxRounds
	}
	hops, stats, err := m.trace(ctx, t.Host, t.Family, count, m.maxTTL, m.timeout, m.spacing)
	if err != nil {
		// A missing CAP_NET_RAW is a measurement never taken, not a target that
		// did not answer: returning nil lets the scheduler stamp Sent = pings,
		// which writes a fabricated full-loss cycle every interval and pages
		// every mtr target. &Result{} leaves the gap instead, and the hint is
		// the one the icmp probe already prints for the same condition.
		if errors.Is(err, errRawUnavailable) {
			logRawUnavailableMTROnce(err)
			return &Result{}, err
		}
		return nil, err
	}

	// Sent and lost are counted in rounds, not in hop rows: a round that walks
	// past the target's old TTL folds its loss onto the marked row there, so
	// summing marked rows counts one round once per TTL the target ever
	// answered at — a lengthening route then reads as loss it never suffered.
	result := &Result{Sent: stats.attempted, LossCount: stats.attempted - stats.reached, Hops: hops}
	// The RTTs still come from the rows the target itself answered, never the
	// deepest row: a per-round walk can leave a silent intermediate below the
	// target's echo, whose latency says nothing about the target.
	for _, h := range hops {
		if h.TargetReply {
			result.RTTs = append(result.RTTs, h.RTTs...)
		}
	}
	return result, nil
}
