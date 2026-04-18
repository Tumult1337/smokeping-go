package scheduler

import (
	"context"
	"log/slog"
)

// LogSink is a Sink that logs every completed cycle. Useful as a placeholder
// before storage/alerting are wired in.
type LogSink struct {
	Log *slog.Logger
}

func (l *LogSink) OnCycle(_ context.Context, c Cycle) {
	l.Log.Info("cycle",
		"target", c.Target.ID(),
		"probe", c.ProbeName,
		"sent", c.Sent,
		"lost", c.LossCount,
		"median_ms", c.Summary.Median.Seconds()*1000,
		"p95_ms", c.Summary.P95.Seconds()*1000,
	)
}
