package master

import (
	"net/http"

	"github.com/tumult/gosmokeping/internal/cluster"
	"github.com/tumult/gosmokeping/internal/config"
)

// ingestBatch turns each wire-format CyclePayload back into a scheduler.Cycle
// and feeds it through the master's sink. Returns the number of accepted
// cycles; silently drops any whose group/name no longer resolves (stale slave
// config vs. fresh master config). That's acceptable — the slave will refresh
// and stop sending within 60s.
func (s *Server) ingestBatch(r *http.Request, batch cluster.CycleBatch) int {
	cfg := s.store.Current()
	targets := make(map[string]config.Target, len(cfg.AllTargets()))
	for _, t := range cfg.AllTargets() {
		targets[t.ID()] = t.Target
	}

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
		s.sink.OnCycle(r.Context(), cycle)
		accepted++
	}
	return accepted
}
