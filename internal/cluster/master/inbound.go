package master

import (
	"context"
	"net/http"
	"time"

	"github.com/tumult/gosmokeping/internal/cluster"
	"github.com/tumult/gosmokeping/internal/config"
)

// ingestBatch turns each wire-format CyclePayload back into a scheduler.Cycle
// and feeds it through the master's sink. Returns the number of accepted
// cycles; silently drops any whose group/name no longer resolves (stale slave
// config vs. fresh master config). That's acceptable — the slave will refresh
// and stop sending within 60s.
func (s *Server) ingestBatch(_ *http.Request, batch cluster.CycleBatch) int {
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

	accepted := 0
	for _, p := range batch.Cycles {
		key := p.Group + "/" + p.Name
		target, ok := targets[key]
		if !ok {
			s.log.Debug("cluster cycle for unknown target, dropping", "target", key, "source", p.Source)
			continue
		}
		// Trust the batch-level Source when a cycle doesn't carry its own —
		// older slave payloads may omit it per-cycle.
		if p.Source == "" {
			p.Source = batch.Source
		}
		cycle := p.ToCycle(target)
		s.sink.OnCycle(sinkCtx, cycle)
		accepted++
	}
	return accepted
}
