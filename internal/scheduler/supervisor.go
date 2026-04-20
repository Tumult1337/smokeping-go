package scheduler

import (
	"context"
	"log/slog"

	"github.com/tumult/gosmokeping/internal/config"
)

// Supervisor owns the scheduler goroutine's lifecycle across config reloads.
// It subscribes to a config.Store, and when a SIGHUP-triggered reload produces
// a config whose Fingerprint differs from the running one, it cancels the
// current scheduler, waits for it to exit, then spins up a fresh one built
// from the new config.
//
// Build is supplied by the caller because the concrete wiring (filtered
// target view, probe registry, source stamp, Sink fanout) lives at the
// composition root and the scheduler package shouldn't know about any of it.
//
// OnReload, if set, is invoked with the new config after every successful
// reload — whether the scheduler was rebuilt or not — so dependants like the
// alert evaluator can re-parse conditions on the same thread that owns the
// lifecycle (no extra subscriber required).
type Supervisor struct {
	Log      *slog.Logger
	Store    *config.Store
	Build    func(cfg *config.Config) (*Scheduler, error)
	OnReload func(cfg *config.Config)
}

// Run blocks until ctx is cancelled. It starts an initial scheduler from
// store.Current(), subscribes to the store, and reacts to every reload:
//
//   - Fingerprint unchanged → leave the scheduler alone, still call OnReload.
//   - Fingerprint changed   → cancel, wait, rebuild, restart.
//   - Rebuild errors        → log, keep the previous scheduler running so the
//                             node doesn't go dark on a transient config bug.
//
// If the initial Build fails Run returns the error without starting anything.
func (s *Supervisor) Run(ctx context.Context) error {
	cur := s.Store.Current()
	sched, err := s.Build(cur)
	if err != nil {
		return err
	}
	fp := Fingerprint(cur)

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

	reloads := make(chan *config.Config, 1)
	s.Store.Subscribe(reloads)

	for {
		select {
		case <-ctx.Done():
			schedCancel()
			<-schedDone
			return nil

		case newCfg := <-reloads:
			if s.OnReload != nil {
				s.OnReload(newCfg)
			}
			newFP := Fingerprint(newCfg)
			if newFP == fp {
				s.Log.Debug("config reload: scheduler fingerprint unchanged, keeping existing goroutines")
				continue
			}

			newSched, err := s.Build(newCfg)
			if err != nil {
				s.Log.Error("config reload: rebuild scheduler failed, keeping previous targets", "err", err)
				continue
			}

			s.Log.Info("config reload: target/probe shape changed, restarting scheduler")
			schedCancel()
			<-schedDone
			fp = newFP
			schedCancel, schedDone = run(newSched)
		}
	}
}
