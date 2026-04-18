package scheduler

import "context"

// Fanout returns a Sink that delivers each cycle to every underlying sink in
// order. Sinks must be safe for concurrent use (Run spawns one goroutine per
// target, so OnCycle can be called concurrently).
func Fanout(sinks ...Sink) Sink {
	if len(sinks) == 1 {
		return sinks[0]
	}
	return fanoutSink(sinks)
}

type fanoutSink []Sink

func (f fanoutSink) OnCycle(ctx context.Context, c Cycle) {
	for _, s := range f {
		s.OnCycle(ctx, c)
	}
}
