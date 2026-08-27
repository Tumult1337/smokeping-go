package scheduler

import (
	"context"
	"log/slog"
)

// LogSink is a Sink that logs every completed cycle at debug level.
type LogSink struct {
	Log *slog.Logger
}

func (l *LogSink) OnCycle(ctx context.Context, c Cycle) {
	// Checked before the args are built: this is the first sink in the fanout
	// and runs for every local and every slave-pushed cycle, so at the deployed
	// shape it was allocating a 12-element []any and an ID() concat ~35 times a
	// second for a line the default level discards.
	if !l.Log.Enabled(ctx, slog.LevelDebug) {
		return
	}
	l.Log.Debug("cycle",
		"target", c.Target.ID(),
		"probe", c.ProbeName,
		"sent", c.Sent,
		"lost", c.LossCount,
		"median_ms", c.Summary.Median.Seconds()*1000,
		"p95_ms", c.Summary.P95.Seconds()*1000,
	)
}
