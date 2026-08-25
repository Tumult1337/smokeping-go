// Package cluster defines the wire protocol and helpers that both the master
// and slave runners use to talk to each other. Implementations of the HTTP
// endpoints live under internal/cluster/master; the slave-side runner lives
// under internal/cluster/slave.
package cluster

import (
	"fmt"
	"net/netip"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/probe"
	"github.com/tumult/gosmokeping/internal/scheduler"
	"github.com/tumult/gosmokeping/internal/stats"
)

// HeaderAdvertise re-asserts a slave's health address on every request, so a
// master restart re-learns the mesh from ordinary traffic instead of waiting
// for the next /register.
const HeaderAdvertise = "X-Slave-Advertise"

// RegisterReq is posted by a slave on boot and repeated as a heartbeat. The
// master records the last-seen time and the reported version so the UI can
// surface slaves that have gone silent.
type RegisterReq struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`

	// Advertise is the IP peers should health-probe this slave at. Empty
	// opts the slave out of the health mesh.
	Advertise string `json:"advertise,omitempty"`
}

// RegisterResp is the ack returned from POST /cluster/register.
type RegisterResp struct {
	Ack bool `json:"ack"`
}

// ClusterConfigResp is the scrubbed subset of the master config that a slave
// needs to start probing. Storage/alerts/actions are deliberately excluded —
// slaves never write storage or dispatch alerts.
type ClusterConfigResp struct {
	Interval time.Duration       `json:"interval"`
	Pings    int                 `json:"pings"`
	Probes   map[string]ProbeDTO `json:"probes"`
	Targets  []config.Group      `json:"targets"`

	// HopMarkers advertises that this master redacts health-target hops by
	// the TargetReply marker rather than by position. A master predating the
	// marker omits the field, and the slave reads that absence as "cannot
	// redact a per-round walk" and withholds its health hops.
	HopMarkers bool `json:"hop_markers,omitempty"`
}

// ProbeDTO mirrors config.Probe on the wire. Duplicated here so the cluster
// package owns the shape slaves consume — changing config.Probe shouldn't
// silently reshape the protocol.
type ProbeDTO struct {
	Type     string        `json:"type"`
	Timeout  time.Duration `json:"timeout,omitempty"`
	Insecure bool          `json:"insecure,omitempty"`
	NoTrace  bool          `json:"no_trace,omitempty"`
}

// CycleBatch is posted by a slave on POST /cluster/cycles. The wrapper lets
// the master attribute every cycle to the pushing slave even if individual
// payloads lack the source field.
type CycleBatch struct {
	Source string         `json:"source"`
	Cycles []CyclePayload `json:"cycles"`
}

// Ingest bounds. Every field below arrives from a slave holding the shared
// cluster token, and until these existed the only limit on a POST /cycles was
// the 100 MiB body cap. Each value is stated against the deployed reference
// shape — 122 targets, 6 sources, a 20s interval, an mtr walk of 30 TTLs over
// 10 rounds — which sits at or below 10% of every one of them.
const (
	// MaxCyclesPerBatch bounds one POST /cycles. slave.Runner drains at most
	// batchLimit (100) cycles per push, so this is 10× the shipped flush.
	MaxCyclesPerBatch = 1024
	// MaxHopsPerCycle bounds hop rows in one cycle at twice
	// config.MaxHopRowsPerCycle — the producer's own exact ceiling, 300 rows
	// for an mtr walk whose path diverges on all 10 rounds of all 30 TTLs.
	// Derived rather than picked so it cannot drift back below what walkRounds
	// legitimately emits; the doubling is headroom for a deeper walk, and the
	// deployed 122-target/6-source/20s install runs the icmp walk, whose
	// ceiling is 3 × 30 = 90.
	MaxHopsPerCycle = 2 * config.MaxHopRowsPerCycle
	// MaxRTTsPerHop bounds samples on one hop row: one per round that
	// reached that responder, and mtr runs 10 rounds to icmp's 3.
	MaxRTTsPerHop = 128
	// MaxHTTPSamplesPerCycle bounds http samples in one cycle. The http
	// probe issues at most maxHTTPRequests (2) per cycle.
	MaxHTTPSamplesPerCycle = 64
	// MaxSampleRTT bounds one latency value — a cycle RTT, a hop RTT, an http
	// sample's RTT, and every stats.Summary field. Negative is not a latency,
	// and the writer's durUS saturates at MaxUint32 microseconds (71m35s), so
	// an hour is under the clamp: anything accepted is stored as itself rather
	// than silently pinned to the ceiling. Every probe's timeout is bounded by
	// the cycle interval, so a real value is milliseconds.
	MaxSampleRTT = time.Hour
	// maxHTTPStatus bounds probe_http.status, which is UInt16 and wraps on a
	// larger value. net/http parses a three-digit status line and the probe
	// reports 0 when the request never completed, so [0, 999] is the producer's
	// whole range.
	maxHTTPStatus = 999
	// MaxHTTPErrLen bounds HTTPSample.Err, which lands verbatim in
	// probe_http.error — a plain String, so this bounds row size rather than
	// dictionary cardinality. An error reads `Get "<url>": dial tcp …`, and
	// 4096 is twice the 2048-byte de-facto URL ceiling, so a maximal operator
	// URL plus its wrapper fits. At MaxHTTPSamplesPerCycle that is ≤256 KiB of
	// error text per cycle.
	MaxHTTPErrLen = 4096
	// Ceilings of the storage columns these land in: probe_hop.ttl is UInt8,
	// every sent/lost counter is UInt16. A negative or oversized value wraps
	// on the way in rather than failing, so it is refused here.
	maxHopIndex = 1<<8 - 1
	maxCounter  = 1<<16 - 1
)

// Validate bounds a received batch against the ingest limits. It rejects the
// whole batch rather than dropping offending cycles: a legitimate slave never
// produces one, so a violation is a protocol disagreement, and quietly
// ingesting the rest of a batch means two peers disagreeing about what was
// stored. now is the master's clock at ingest.
func (b CycleBatch) Validate(now time.Time) error {
	if len(b.Cycles) > MaxCyclesPerBatch {
		return fmt.Errorf("batch carries %d cycles, limit %d", len(b.Cycles), MaxCyclesPerBatch)
	}
	oldest, newest := now.Add(-config.MaxCycleAge), now.Add(config.MaxFutureSkew)
	for i, c := range b.Cycles {
		if err := c.validate(oldest, newest); err != nil {
			return fmt.Errorf("cycle %d (%s/%s): %w", i, c.Group, c.Name, err)
		}
	}
	return nil
}

func (p CyclePayload) validate(oldest, newest time.Time) error {
	if p.Time.Before(oldest) || p.Time.After(newest) {
		return fmt.Errorf("timestamp %s outside [%s, %s]", p.Time, oldest, newest)
	}
	for _, l := range [][2]string{
		{"group", p.Group}, {"name", p.Name}, {"probe", p.ProbeName}, {"source", p.Source},
	} {
		if len(l[1]) > config.MaxLabelLen {
			return fmt.Errorf("%s is %d bytes, limit %d", l[0], len(l[1]), config.MaxLabelLen)
		}
	}
	if err := boundCounters("cycle", p.Sent, p.LossCount); err != nil {
		return err
	}
	if len(p.RTTs) > config.MaxPingsPerCycle {
		return fmt.Errorf("%d rtts, limit %d", len(p.RTTs), config.MaxPingsPerCycle)
	}
	if err := boundRTTs("cycle", p.RTTs); err != nil {
		return err
	}
	if err := boundSummary(p.Summary); err != nil {
		return err
	}
	if len(p.HTTPSamples) > MaxHTTPSamplesPerCycle {
		return fmt.Errorf("%d http samples, limit %d", len(p.HTTPSamples), MaxHTTPSamplesPerCycle)
	}
	for i, s := range p.HTTPSamples {
		if err := s.validate(oldest, newest); err != nil {
			return fmt.Errorf("http sample %d: %w", i, err)
		}
	}
	if len(p.Hops) > MaxHopsPerCycle {
		return fmt.Errorf("%d hops, limit %d", len(p.Hops), MaxHopsPerCycle)
	}
	for _, h := range p.Hops {
		if err := h.validate(); err != nil {
			return fmt.Errorf("hop %d: %w", h.Index, err)
		}
	}
	return nil
}

func (h HopDTO) validate() error {
	if h.Index < 0 || h.Index > maxHopIndex {
		return fmt.Errorf("index %d outside [0, %d]", h.Index, maxHopIndex)
	}
	// An empty address is the producer's "nothing answered at this TTL". Any
	// other value must parse: hop_addr is a LowCardinality dictionary served
	// back by an unauthenticated /hops, so free text there is both unbounded
	// cardinality and unbounded response size.
	if h.IP != "" {
		if _, err := netip.ParseAddr(h.IP); err != nil {
			return fmt.Errorf("ip %q is not an address", truncate(h.IP))
		}
	}
	if err := boundCounters("hop", h.Sent, h.Lost); err != nil {
		return err
	}
	if len(h.RTTs) > MaxRTTsPerHop {
		return fmt.Errorf("carries %d rtts, limit %d", len(h.RTTs), MaxRTTsPerHop)
	}
	return boundRTTs("hop", h.RTTs)
}

func (s HTTPSampleDTO) validate(oldest, newest time.Time) error {
	if s.Time.Before(oldest) || s.Time.After(newest) {
		return fmt.Errorf("timestamp %s outside [%s, %s]", s.Time, oldest, newest)
	}
	if s.Status < 0 || s.Status > maxHTTPStatus {
		return fmt.Errorf("status %d outside [0, %d]", s.Status, maxHTTPStatus)
	}
	if len(s.Err) > MaxHTTPErrLen {
		return fmt.Errorf("err is %d bytes, limit %d", len(s.Err), MaxHTTPErrLen)
	}
	return boundRTTs("http sample", []time.Duration{s.RTT})
}

// boundCounters also refuses lost > sent: no probe can lose more probes than
// it sent, and the pair drives loss_pct and the alert conditions.
func boundCounters(what string, sent, lost int) error {
	if sent < 0 || sent > maxCounter {
		return fmt.Errorf("%s sent %d outside [0, %d]", what, sent, maxCounter)
	}
	if lost < 0 || lost > maxCounter {
		return fmt.Errorf("%s lost %d outside [0, %d]", what, lost, maxCounter)
	}
	if lost > sent {
		return fmt.Errorf("%s lost %d exceeds sent %d", what, lost, sent)
	}
	return nil
}

func boundRTTs(what string, rtts []time.Duration) error {
	for i, d := range rtts {
		if d < 0 || d > MaxSampleRTT {
			return fmt.Errorf("%s rtt %d is %s, outside [0, %s]", what, i, d, MaxSampleRTT)
		}
	}
	return nil
}

// boundSummary walks stats.PercentileSet rather than naming fields, so a new
// percentile is covered the day it is added.
func boundSummary(s stats.Summary) error {
	fixed := []time.Duration{s.Min, s.Max, s.Mean, s.Median, s.StdDev}
	if err := boundRTTs("summary", fixed); err != nil {
		return err
	}
	for _, spec := range stats.PercentileSet {
		if d := spec.Get(s); d < 0 || d > MaxSampleRTT {
			return fmt.Errorf("summary %s is %s, outside [0, %s]", spec.Name, d, MaxSampleRTT)
		}
	}
	return nil
}

// truncate keeps a rejected value short enough to log.
func truncate(s string) string {
	const max = 64
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// CyclePayload is a single scheduler.Cycle serialized for the wire. RTTs and
// hop latencies are nanoseconds (int64) — JSON-marshaling time.Duration
// directly yields the same integer, which is stable and needs no decoding
// on the master side.
type CyclePayload struct {
	Time        time.Time       `json:"time"`
	Group       string          `json:"group"`
	Name        string          `json:"name"`
	ProbeName   string          `json:"probe"`
	Source      string          `json:"source"`
	RTTs        []time.Duration `json:"rtts,omitempty"`
	Sent        int             `json:"sent"`
	LossCount   int             `json:"loss_count"`
	Summary     stats.Summary   `json:"summary"`
	Hops        []HopDTO        `json:"hops,omitempty"`
	HTTPSamples []HTTPSampleDTO `json:"http_samples,omitempty"`
}

// HopDTO mirrors probe.Hop. Kept separate from the domain type so adding a
// new internal field on probe.Hop doesn't silently change the wire shape.
type HopDTO struct {
	Index int    `json:"index"`
	IP    string `json:"ip,omitempty"`
	// Unreach is normalized at ingest (ToCycle): a slave's value is untrusted.
	Unreach     string          `json:"unreach,omitempty"`
	TargetReply bool            `json:"target_reply,omitempty"`
	RTTs        []time.Duration `json:"rtts,omitempty"`
	Sent        int             `json:"sent"`
	Lost        int             `json:"lost"`
}

// HTTPSampleDTO mirrors probe.HTTPSample.
type HTTPSampleDTO struct {
	Time   time.Time     `json:"time"`
	RTT    time.Duration `json:"rtt"`
	Status int           `json:"status"`
	Err    string        `json:"err,omitempty"`
}

// ToCycle rebuilds a scheduler.Cycle from a received payload. TargetRef is
// reconstructed from the cycle's group/name plus the probe definition pulled
// from the current master config (the slave's probe map is authoritative for
// type/timeout on the probe-execution side — the master only needs enough to
// route the write).
func (p CyclePayload) ToCycle(target config.Target) scheduler.Cycle {
	hops := make([]probe.Hop, len(p.Hops))
	for i, h := range p.Hops {
		hops[i] = probe.Hop{
			Index:       h.Index,
			IP:          canonicalHopAddr(h.IP),
			TargetReply: h.TargetReply,
			Unreach:     probe.CanonicalUnreach(h.Unreach),
			RTTs:        h.RTTs,
			Sent:        h.Sent,
			Lost:        h.Lost,
		}
	}
	samples := make([]probe.HTTPSample, len(p.HTTPSamples))
	for i, s := range p.HTTPSamples {
		samples[i] = probe.HTTPSample{Time: s.Time, RTT: s.RTT, Status: s.Status, Err: s.Err}
	}
	return scheduler.Cycle{
		Time:        p.Time,
		Target:      config.TargetRef{Group: p.Group, Target: target},
		ProbeName:   p.ProbeName,
		Source:      p.Source,
		RTTs:        p.RTTs,
		Sent:        p.Sent,
		LossCount:   p.LossCount,
		Summary:     p.Summary,
		Hops:        hops,
		HTTPSamples: samples,
	}
}

// canonicalHopAddr collapses the spellings of one address to the single form
// hop_addr's LowCardinality dictionary should hold, and drops anything
// validate would have refused rather than storing text as an address.
func canonicalHopAddr(ip string) string {
	if ip == "" {
		return ""
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ""
	}
	return addr.String()
}

// FromCycle is the slave-side companion to ToCycle.
func FromCycle(c scheduler.Cycle) CyclePayload {
	hops := make([]HopDTO, len(c.Hops))
	for i, h := range c.Hops {
		hops[i] = HopDTO{
			Index:       h.Index,
			IP:          h.IP,
			Unreach:     h.Unreach,
			TargetReply: h.TargetReply,
			RTTs:        h.RTTs,
			Sent:        h.Sent,
			Lost:        h.Lost,
		}
	}
	samples := make([]HTTPSampleDTO, len(c.HTTPSamples))
	for i, s := range c.HTTPSamples {
		samples[i] = HTTPSampleDTO{Time: s.Time, RTT: s.RTT, Status: s.Status, Err: s.Err}
	}
	return CyclePayload{
		Time:        c.Time,
		Group:       c.Target.Group,
		Name:        c.Target.Target.Name,
		ProbeName:   c.ProbeName,
		Source:      c.Source,
		RTTs:        c.RTTs,
		Sent:        c.Sent,
		LossCount:   c.LossCount,
		Summary:     c.Summary,
		Hops:        hops,
		HTTPSamples: samples,
	}
}
