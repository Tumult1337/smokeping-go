package scheduler

import (
	"context"
	"log/slog"

	"github.com/tumult/gosmokeping/internal/config"
)

// Supervisor adapts RunLifecycle to a config.Store: it subscribes to the
// store and forwards reloads into the lifecycle helper. The concrete
// scheduler wiring (filtered target view, probe registry, source stamp,
// Sink fanout) stays with the caller via Build.
//
// OnReload, if set, fires once per reload — whether the scheduler was
// rebuilt or not — so dependants like the alert evaluator can re-parse
// conditions on the same thread that owns the lifecycle.
type Supervisor struct {
	Log      *slog.Logger
	Store    *config.Store
	Build    func(cfg *config.Config) (*Scheduler, error)
	OnReload func(cfg *config.Config)
}

// Run blocks until ctx is cancelled. Returns non-nil only if the initial
// Build fails.
func (s *Supervisor) Run(ctx context.Context) error {
	reloads := make(chan *config.Config, 1)
	s.Store.Subscribe(reloads)

	return RunLifecycle(ctx, LifecycleOptions{
		Log:      s.Log,
		Initial:  s.Store.Current(),
		Build:    s.Build,
		Reloads:  reloads,
		OnReload: s.OnReload,
	})
}
