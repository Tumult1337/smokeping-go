package scheduler

import (
	"context"
	"log/slog"
)

// Fanout returns a Sink that delivers each cycle to every underlying sink in
// order. Sinks must be safe for concurrent use (Run spawns one goroutine per
// target, so OnCycle can be called concurrently).
//
// Each sink is isolated: a panic in one sink is recovered and logged so the
// remaining sinks still receive the cycle. This matters because slave-inbound
// cycles feed this fanout directly (no per-cycle recover wraps them) and
// because a panic in an early sink (e.g. the storage writer) must not silently
// skip a later one (e.g. the alert evaluator).
func Fanout(log *slog.Logger, sinks ...Sink) Sink {
	return fanoutSink{log: log, sinks: sinks}
}

type fanoutSink struct {
	log   *slog.Logger
	sinks []Sink
}

func (f fanoutSink) OnCycle(ctx context.Context, c Cycle) {
	for _, s := range f.sinks {
		f.deliver(ctx, s, c)
	}
}

func (f fanoutSink) deliver(ctx context.Context, s Sink, c Cycle) {
	defer func() {
		if v := recover(); v != nil && f.log != nil {
			f.log.Error("sink panic recovered", "target", c.Target.ID(), "source", c.Source, "panic", v)
		}
	}()
	s.OnCycle(ctx, c)
}
