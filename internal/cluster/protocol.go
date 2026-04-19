// Package cluster defines the wire protocol and helpers that both the master
// and slave runners use to talk to each other. Implementations of the HTTP
// endpoints live under internal/cluster/master; the slave-side runner lives
// under internal/cluster/slave.
package cluster

import (
	"time"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/probe"
	"github.com/tumult/gosmokeping/internal/scheduler"
	"github.com/tumult/gosmokeping/internal/stats"
)

// RegisterReq is posted by a slave on boot and repeated as a heartbeat. The
// master records the last-seen time and the reported version so the UI can
// surface slaves that have gone silent.
type RegisterReq struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// RegisterResp is the ack returned from POST /cluster/register.
type RegisterResp struct {
	Ack bool `json:"ack"`
}

// ClusterConfigResp is the scrubbed subset of the master config that a slave
// needs to start probing. Influx/alerts/actions are deliberately excluded —
// slaves never write storage or dispatch alerts.
type ClusterConfigResp struct {
	Interval time.Duration           `json:"interval"`
	Pings    int                     `json:"pings"`
	Probes   map[string]ProbeDTO     `json:"probes"`
	Targets  []config.Group          `json:"targets"`
}

// ProbeDTO mirrors config.Probe on the wire. Duplicated here so the cluster
// package owns the shape slaves consume — changing config.Probe shouldn't
// silently reshape the protocol.
type ProbeDTO struct {
	Type     string        `json:"type"`
	Timeout  time.Duration `json:"timeout,omitempty"`
	Insecure bool          `json:"insecure,omitempty"`
}

// CycleBatch is posted by a slave on POST /cluster/cycles. The wrapper lets
// the master attribute every cycle to the pushing slave even if individual
// payloads lack the source field.
type CycleBatch struct {
	Source string         `json:"source"`
	Cycles []CyclePayload `json:"cycles"`
}

// CyclePayload is a single scheduler.Cycle serialized for the wire. RTTs and
// hop latencies are nanoseconds (int64) — JSON-marshaling time.Duration
// directly yields the same integer, which is stable and needs no decoding
// on the master side.
type CyclePayload struct {
	Time        time.Time        `json:"time"`
	Group       string           `json:"group"`
	Name        string           `json:"name"`
	ProbeName   string           `json:"probe"`
	Source      string           `json:"source"`
	RTTs        []time.Duration  `json:"rtts,omitempty"`
	Sent        int              `json:"sent"`
	LossCount   int              `json:"loss_count"`
	Summary     stats.Summary    `json:"summary"`
	Hops        []HopDTO         `json:"hops,omitempty"`
	HTTPSamples []HTTPSampleDTO  `json:"http_samples,omitempty"`
}

// HopDTO mirrors probe.Hop. Kept separate from the domain type so adding a
// new internal field on probe.Hop doesn't silently change the wire shape.
type HopDTO struct {
	Index int             `json:"index"`
	IP    string          `json:"ip,omitempty"`
	RTTs  []time.Duration `json:"rtts,omitempty"`
	Sent  int             `json:"sent"`
	Lost  int             `json:"lost"`
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
		hops[i] = probe.Hop{Index: h.Index, IP: h.IP, RTTs: h.RTTs, Sent: h.Sent, Lost: h.Lost}
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

// FromCycle is the slave-side companion to ToCycle.
func FromCycle(c scheduler.Cycle) CyclePayload {
	hops := make([]HopDTO, len(c.Hops))
	for i, h := range c.Hops {
		hops[i] = HopDTO{Index: h.Index, IP: h.IP, RTTs: h.RTTs, Sent: h.Sent, Lost: h.Lost}
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
