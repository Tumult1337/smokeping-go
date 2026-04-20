package scheduler

import (
	"context"
	"log/slog"

	"github.com/tumult/gosmokeping/internal/config"
)

// LifecycleOptions wires a scheduler's runtime lifecycle. Callers supply
// the initial config, a Build closure, and a Reloads channel; the helper
// handles the fingerprint compare / cancel / rebuild dance.
//
// Build-first, cancel-on-success: a rebuild error leaves the previous
// scheduler running instead of going dark — the slave used to cancel
// first and briefly lose coverage on a transient rebuild failure.
type LifecycleOptions struct {
	Log      *slog.Logger
	Initial  *config.Config
	Build    func(cfg *config.Config) (*Scheduler, error)
	Reloads  <-chan *config.Config
	OnReload func(cfg *config.Config)
}

// RunLifecycle blocks until ctx is cancelled. On each receive from Reloads:
//
//   - OnReload (if set) fires first, unconditionally.
//   - Fingerprint unchanged → keep the running scheduler.
//   - Fingerprint changed → Build; on error keep the old one; on success
//     cancel old, wait, and swap in the new.
//
// Returns the Build error if the initial config cannot be built; otherwise
// returns nil once ctx is done and the scheduler has exited.
func RunLifecycle(ctx context.Context, opts LifecycleOptions) error {
	sched, err := opts.Build(opts.Initial)
	if err != nil {
		return err
	}
	fp := Fingerprint(opts.Initial)

	run := func(sch *Scheduler) (context.CancelFunc, chan struct{}) {
		sctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			sch.Run(sctx)
			close(done)
		}()
		return cancel, done
	}

	schedCancel, schedDone := run(sched)

	for {
		select {
		case <-ctx.Done():
			schedCancel()
			<-schedDone
			return nil

		case newCfg := <-opts.Reloads:
			if opts.OnReload != nil {
				opts.OnReload(newCfg)
			}
			newFP := Fingerprint(newCfg)
			if newFP == fp {
				opts.Log.Debug("config reload: scheduler fingerprint unchanged, keeping existing goroutines")
				continue
			}
			newSched, err := opts.Build(newCfg)
			if err != nil {
				opts.Log.Error("config reload: rebuild scheduler failed, keeping previous targets", "err", err)
				continue
			}
			opts.Log.Info("config reload: target/probe shape changed, restarting scheduler")
			schedCancel()
			<-schedDone
			fp = newFP
			schedCancel, schedDone = run(newSched)
		}
	}
}
