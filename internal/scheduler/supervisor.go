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

	// OnRebuilt is forwarded to LifecycleOptions. See the field there: it is
	// where anything destructive keyed on a departed target belongs.
	OnRebuilt func(cfg *config.Config)

	// Signals, when non-nil, is used as the store subscription channel so
	// other producers — notably cluster registry changes — can request a
	// rebuild through the same coalescing path as a config reload. Must be
	// buffered; sends from other producers must be non-blocking.
	Signals chan struct{}

	// ExtraFingerprint is forwarded to LifecycleOptions. See the field there.
	ExtraFingerprint func() string
}

// Run blocks until ctx is cancelled. Returns non-nil only if the initial
// Build fails.
func (s *Supervisor) Run(ctx context.Context) error {
	reloads := s.Signals
	if reloads == nil {
		reloads = make(chan struct{}, 1)
	}
	s.Store.Subscribe(reloads)

	return RunLifecycle(ctx, LifecycleOptions{
		Log:              s.Log,
		Initial:          s.Store.Current(),
		Current:          s.Store.Current,
		Build:            s.Build,
		Reloads:          reloads,
		OnReload:         s.OnReload,
		OnRebuilt:        s.OnRebuilt,
		ExtraFingerprint: s.ExtraFingerprint,
	})
}
