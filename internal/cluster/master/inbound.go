package master

import (
	"context"
	"net/http"

	"github.com/tumult/gosmokeping/internal/cluster"
	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/scheduler"
)

// sinkCycleBudget bounds one cycle's trip through the fanout; sinkBatchBudget
// bounds the whole POST's. Both are cluster.PushTimeout-derived, and both are
// inert today — see deliver, where no sink reads either deadline.
const (
	// sinkBatchBudget is the slave's own client timeout, not api's write
	// timeout: the slave abandons the request and requeues at
	// cluster.PushTimeout, so a handler still running past it is an orphan
	// holding a whole batch nobody will read the answer to. Deriving it from
	// the API's 120s write timeout was a false premise — it let an API-side
	// knob silently multiply ingest orphan concurrency.
	sinkBatchBudget = cluster.PushTimeout
	// sinkCycleBudget bounds one cycle's trip through the fanout. Derived from
	// the batch budget rather than written as its own duration, and that
	// derivation *is* the guard: context.WithTimeout on a parent with an
	// earlier deadline keeps the parent's, so an independently-chosen value
	// larger than the batch's would leave every cycle sharing one deadline
	// with no error and no warning. A third means at least three stalled
	// deliveries before one batch's window is gone — no value bounds one cycle
	// *and* leaves room for MaxCyclesPerBatch of them, which is why the batch
	// budget exists. A compile-time assertion here was deleted rather than
	// repaired: `a/3 < a` cannot fail, so it guarded nothing, and its operand
	// overflowed a 32-bit uint, breaking every GOARCH=386/arm build.
	sinkCycleBudget = sinkBatchBudget / 3
)

// ingestBatch turns each wire-format CyclePayload back into a scheduler.Cycle
// and feeds it through the master's sink. Returns the number of cycles ingested
// and the number recognised as redeliveries; silently drops any whose
// group/name no longer resolves (stale slave config vs. fresh master config).
// That's acceptable — the slave will refresh and stop sending within 60s.
func (s *Server) ingestBatch(_ *http.Request, batch cluster.CycleBatch) (int, int) {
	cfg := s.store.Current()
	all := cfg.AllTargets()
	targets := make(map[string]config.TargetRef, len(all))
	for _, t := range all {
		targets[t.ID()] = t
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
				ref := config.TargetRef{Group: g.Group, Target: t}
				targets[ref.ID()] = ref
			}
		}
	}

	// One budget for the whole batch, with each cycle bounded inside it. Per
	// cycle alone bounded nothing in aggregate: MaxCyclesPerBatch × the
	// per-cycle budget is ~8.5 hours of handler, and the write timeout that
	// would close the connection does not stop the goroutine, so
	// PushSink.Requeue resends and each retry starts another one.
	batchCtx, cancelBatch := context.WithTimeout(context.Background(), sinkBatchBudget)
	defer cancelBatch()

	accepted, duplicates := 0, 0
	for _, p := range batch.Cycles {
		ref, ok := targets[p.Group+"/"+p.Name]
		if !ok {
			s.log.Debug("cluster cycle for unknown target, dropping",
				"target", p.Group+"/"+p.Name, "source", p.Source)
			continue
		}
		// The resolved target's own id, not the wire spelling that found it:
		// neither group nor name has a character class, so one target is
		// reachable under several flattened keys, and keying the dedup window on
		// the caller's choice let the same measurement be admitted once per
		// spelling.
		key := ref.ID()
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
		p.ProbeName = ref.Target.Probe
		// Delivery is at-least-once — PushSink.Requeue resends a batch whose
		// ack was lost — and every storage table is a plain MergeTree that
		// keeps both copies, so a redelivery doubles sum(sent) and every loss
		// percentage derived from it. Guarding here rather than in a sink puts
		// one check upstream of the whole fanout.
		if !s.dedup.admit(batch.Source, key, p.Time.UnixNano()) {
			duplicates++
			continue
		}
		s.deliver(batchCtx, p.ToCycle(ref), batch.Source, key, p.Time.UnixNano())
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
//
// The context is detached from the request — a slave TCP disconnect mid-POST
// must not cancel cycles already being processed — and bounded twice: per
// cycle, so a few stalled deliveries cannot spend the batch and starve
// everything behind them, and by the batch's own budget, so their sum is
// bounded too. Today no sink reads either deadline (the fanout does not check
// it, and both writer and evaluator hand off without blocking), so neither
// bounds anything a caller can observe; they exist for the sink CLAUDE.md
// invites anyone to append.
func (s *Server) deliver(batchCtx context.Context, cycle scheduler.Cycle, source, key string, nano int64) {
	ctx, cancel := context.WithTimeout(batchCtx, sinkCycleBudget)
	defer cancel()
	delivered := false
	defer func() {
		if !delivered {
			s.dedup.forget(source, key, nano)
		}
	}()
	s.sink.OnCycle(ctx, cycle)
	delivered = true
}
