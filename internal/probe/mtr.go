package probe

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
)

type echoFunc func(ctx context.Context, target Target, count int) (*Result, error)

// MTR discovers the path to a target by sending ICMP echoes with increasing
// TTL and collecting intermediate routers' TimeExceeded replies. Each cycle
// runs `count` rounds, and every round walks TTL=1..maxTTL until it hits its
// own terminal, so a route that changes mid-cycle is followed instead of being
// clamped to the shortest path an earlier round saw.
//
// Path discovery requires raw ICMP sockets (CAP_NET_RAW); UDP ping sockets
// don't reliably surface intermediate ICMP errors on Linux. The concurrent
// direct target batch can use UDP ping sockets even when the walk cannot run.
type MTR struct {
	name    string
	timeout time.Duration
	maxTTL  int
	spacing time.Duration
	// echo is the direct target measurement. It deliberately disables ICMP's
	// own opportunistic trace because MTR's walk below is the sole hop source.
	echo echoFunc
	// trace is the injectable seam over traceHops, mirroring ICMP's.
	trace traceFunc
}

func NewMTR(name string, timeout time.Duration) *MTR {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	echo := newMTRDirectICMP(name, timeout)
	return &MTR{name: name, timeout: timeout, maxTTL: maxTTL, spacing: 50 * time.Millisecond, echo: echo.Probe, trace: traceHops}
}

func newMTRDirectICMP(name string, timeout time.Duration) *ICMP {
	echo := NewICMP(name, timeout, true)
	// NoTrace disables its own walk, but sequence allocation must still
	// leave room for MTR's concurrent full walk if raw identifiers collide.
	echo.traceRounds = maxRounds
	echo.traceMaxTTL = maxTTL
	return echo
}

func (m *MTR) Name() string { return m.name }

type mtrTraceResult struct {
	hops []Hop
	err  error
}

func (m *MTR) startTrace(ctx context.Context, t Target, count int) <-chan mtrTraceResult {
	ch := make(chan mtrTraceResult, 1)
	go func() {
		defer func() {
			if v := recover(); v != nil {
				ch <- mtrTraceResult{err: fmt.Errorf("%w: %v", errTracePanic, v)}
			}
		}()
		hops, _, err := m.trace(ctx, t.Host, t.Family, count, m.maxTTL, m.timeout, m.spacing)
		ch <- mtrTraceResult{hops: hops, err: err}
	}()
	return ch
}

// maxRounds caps both MTR's direct echo count and its trace rounds. Each round
// walks up to maxTTL hops; the shared cycle deadline also bounds duration.
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
	_ uint = maxRounds - config.MaxTraceRounds // schedule validation uses this cap
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

	traced := m.startTrace(ctx, t, count)
	traceJoined := false
	defer func() {
		if !traceJoined {
			<-traced
		}
	}()
	directResult, directErr := m.echo(ctx, t, count)
	traceResult := <-traced
	traceJoined = true

	if errors.Is(traceResult.err, errRawUnavailable) {
		logRawUnavailableMTROnce(traceResult.err)
	}

	result := &Result{Hops: traceResult.hops}
	if directResult != nil {
		result.RTTs = directResult.RTTs
		result.Sent = directResult.Sent
		result.LossCount = directResult.LossCount
	}

	if directErr != nil {
		directErr = fmt.Errorf("mtr direct echo: %w", directErr)
	}
	if traceResult.err != nil {
		traceResult.err = fmt.Errorf("mtr trace: %w", traceResult.err)
	}
	return result, errors.Join(directErr, traceResult.err)
}
