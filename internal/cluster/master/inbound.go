package master

import (
	"context"
	"net/http"
	"time"

	"github.com/tumult/gosmokeping/internal/cluster"
	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/scheduler"
)

// ingestBatch turns each wire-format CyclePayload back into a scheduler.Cycle
// and feeds it through the master's sink. Returns the number of cycles ingested
// and the number recognised as redeliveries; silently drops any whose
// group/name no longer resolves (stale slave config vs. fresh master config).
// That's acceptable — the slave will refresh and stop sending within 60s.
func (s *Server) ingestBatch(_ *http.Request, batch cluster.CycleBatch) (int, int) {
	cfg := s.store.Current()
	targets := make(map[string]config.Target, len(cfg.AllTargets()))
	for _, t := range cfg.AllTargets() {
		targets[t.ID()] = t.Target
	}
	// Health targets are synthesized at scheduler-build time and never stored
	// in config, so AllTargets cannot see them — without this the mesh is
	// one-directional: every peer-health cycle a slave pushes would resolve to
	// nothing and be dropped, leaving the master the only observer and quorum
	// permanently unreachable.
	//
	// Probe("") rather than Probe(slaveName): a slave never pushes a cycle for
	// itself, so excluding self buys nothing, and the unfiltered set keeps
	// resolution independent of who is posting.
	if hs := s.healthSet(); hs != nil {
		for _, g := range hs.Probe("") {
			for _, t := range g.Targets {
				targets[g.Group+"/"+t.Name] = t
			}
		}
	}

	// Use a detached context for sink delivery so a slave TCP disconnect
	// mid-POST doesn't cancel cycles that are already being processed.
	sinkCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	accepted, duplicates := 0, 0
	for _, p := range batch.Cycles {
		key := p.Group + "/" + p.Name
		target, ok := targets[key]
		if !ok {
			s.log.Debug("cluster cycle for unknown target, dropping", "target", key, "source", p.Source)
			continue
		}
		// Unconditionally overwrite with the batch-level Source, which
		// handleCycles already pinned to the authenticated X-Slave-Name
		// header. A per-cycle Source is wire-provided and untrusted — a
		// slave could populate it with "master" or another slave's name to
		// forge alert-quorum votes (manufacture phantom healthy sources to
		// mask a real outage, or phantom firing ones to trigger a false
		// page). The scheduler always stamps a slave's own name on cycles
		// it produces locally, so nothing legitimate depends on trusting
		// the wire value here.
		p.Source = batch.Source
		// probe_type is LowCardinality too, and the resolved target already
		// names its probe, so the wire value is discarded for the same reason
		// Source is: it is free text a token holder chooses.
		p.ProbeName = target.Probe
		// Delivery is at-least-once — PushSink.Requeue resends a batch whose
		// ack was lost — and every storage table is a plain MergeTree that
		// keeps both copies, so a redelivery doubles sum(sent) and every loss
		// percentage derived from it. Guarding here rather than in a sink puts
		// one check upstream of the whole fanout.
		if !s.dedup.admit(batch.Source, key, p.Time.UnixNano()) {
			duplicates++
			continue
		}
		s.deliver(sinkCtx, p.ToCycle(target), batch.Source, key, p.Time.UnixNano())
		accepted++
	}
	return accepted, duplicates
}

// deliver hands one cycle to the fanout, releasing the window slot admit
// reserved if the call never returns. The reservation is taken first because
// it is what makes two copies arriving at once resolve to one delivery;
// releasing it on the way out is what keeps a measurement no sink took from
// being remembered as delivered, since the repair copy would then be refused.
// It reaches as far as the fanout and no further: OnCycle reports nothing, so
// a row the writer drops on a full channel is delivered as far as this can
// see.
func (s *Server) deliver(ctx context.Context, cycle scheduler.Cycle, source, key string, nano int64) {
	delivered := false
	defer func() {
		if !delivered {
			s.dedup.forget(source, key, nano)
		}
	}()
	s.sink.OnCycle(ctx, cycle)
	delivered = true
}
