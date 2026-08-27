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

// HeaderRefusal marks a 4xx the master's own handlers produced for a request
// whose bytes can never succeed. A status code cannot carry that: nginx maps
// its internal 494 (header buffer exceeded) and 497 to a plain 400, and
// HAProxy and Envoy answer 400 for a malformed request line — and the slave
// sends X-Slave-Name/Version/Advertise on every request, so a proxy header
// limit below maxSlaveFieldLen would 400 the whole fleet into a crash loop.
// A master that predates this header is simply never fatal, which is the safe
// direction: the slave retries with backoff instead of exiting.
const HeaderRefusal = "X-Cluster-Refusal"

// RefusalPermanent is HeaderRefusal's only value.
const RefusalPermanent = "permanent"

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
	// maxHTTPStatus bounds probe_http.status, which is UInt16 and wraps on a
	// larger value. net/http parses a three-digit status line and the probe
	// reports 0 when the request never completed, so [0, 999] is the producer's
	// whole range.
	maxHTTPStatus = 999
	// maxInterfaceNameLen is the producer's ceiling for a zone: Go fills one
	// from net.Interface.Name, or the decimal interface index when the name is
	// unknown, and IFNAMSIZ is 16 on every platform this binary ships for
	// (Linux, macOS, the BSDs) against 10 digits for an int32 index.
	maxInterfaceNameLen = 15
	// MaxHopZoneLen is twice that, the same headroom MaxHopsPerCycle takes
	// over its own producer ceiling. It is not wider because hop_addr's width
	// is what turns clickhouse.maxHopRows — a bound derived in rows — into a
	// byte ceiling on an unauthenticated /hops.
	MaxHopZoneLen = 2 * maxInterfaceNameLen
	// maxIPv6TextLen is RFC 4291 section 2.2 form 3, the longest textual
	// address netip.ParseAddr accepts: six groups of four hex digits, five
	// colons, and a dotted-quad tail.
	maxIPv6TextLen = 45
	// MaxHopAddrLen bounds the complete encoded hop address, which is what
	// lands in probe_hop.hop_addr.
	MaxHopAddrLen = maxIPv6TextLen + len("%") + MaxHopZoneLen
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
	// An empty address is the producer's "nothing answered at this TTL".
	if h.IP != "" {
		if _, err := parseHopAddr(h.IP); err != nil {
			return fmt.Errorf("ip %w", err)
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
		if d < 0 || d > config.MaxSampleRTT {
			return fmt.Errorf("%s rtt %d is %s, outside [0, %s]", what, i, d, config.MaxSampleRTT)
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
		if d := spec.Get(s); d < 0 || d > config.MaxSampleRTT {
			return fmt.Errorf("summary %s is %s, outside [0, %s]", spec.Name, d, config.MaxSampleRTT)
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
		samples[i] = probe.HTTPSample{
			Time: s.Time, RTT: s.RTT, Status: s.Status,
			// Truncated rather than refused: Err carries a url.Error whose URL
			// config never bounds, and refusing drops every other cycle in the
			// batch with it.
			Err: probe.TruncateHTTPErr(s.Err),
		}
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
	addr, err := parseHopAddr(ip)
	if err != nil {
		return ""
	}
	return addr.String()
}

// parseHopAddr is the single reading of a slave-supplied hop address — the one
// validate refuses on and the one ToCycle stores. hop_addr is a
// LowCardinality dictionary an unauthenticated /hops serves back, so the whole
// encoded value is bounded before it is parsed: netip.ParseAddr accepts a zone
// of any length, which put megabytes of attacker text in that column behind an
// address that parsed.
func parseHopAddr(ip string) (netip.Addr, error) {
	if len(ip) > MaxHopAddrLen {
		return netip.Addr{}, fmt.Errorf("is %d bytes, limit %d", len(ip), MaxHopAddrLen)
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("%q is not an address", truncate(ip))
	}
	if z := addr.Zone(); z != "" && !interfaceZoneShaped(z) {
		return netip.Addr{}, fmt.Errorf("%q carries a zone that is not an interface name", truncate(ip))
	}
	return addr, nil
}

// interfaceZoneShaped reports whether a zone is short enough to be the
// interface name or decimal index Go fills one from, and free of the bytes
// that make slave-supplied text dangerous downstream: ASCII control bytes and
// the "/" a path or devfs node would take. RFC 4007 section 11.2 leaves the
// zone implementation-defined and the character class is not a survey of what
// each kernel accepts, so "%" passes — refusing a character some platform can
// name an interface costs the producer's whole batch.
func interfaceZoneShaped(zone string) bool {
	if len(zone) > MaxHopZoneLen {
		return false
	}
	for i := range len(zone) {
		switch c := zone[i]; {
		case c < 0x20, c == 0x7f, c == '/':
			return false
		}
	}
	return true
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
