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
//
// Reloads carries a struct{} signal (not the config itself). On each signal
// the lifecycle calls Current() to get the authoritative latest config —
// this avoids a race where rapid reloads drop the second channel send, leaving
// the scheduler on a stale config while the store holds the newer one.
type LifecycleOptions struct {
	Log      *slog.Logger
	Initial  *config.Config
	Current  func() *config.Config
	Build    func(cfg *config.Config) (*Scheduler, error)
	Reloads  <-chan struct{}
	OnReload func(cfg *config.Config)

	// ExtraFingerprint contributes to the rebuild decision from outside the
	// stored config. Cluster health targets are injected inside Build and
	// never appear in the config, so Fingerprint(cfg) cannot see a mesh
	// membership change on its own. Optional; nil contributes nothing.
	ExtraFingerprint func() string
}

// fingerprint combines the config fingerprint with any external contribution.
func (o LifecycleOptions) fingerprint(cfg *config.Config) string {
	fp := Fingerprint(cfg)
	if o.ExtraFingerprint != nil {
		fp += "\x1d" + o.ExtraFingerprint()
	}
	return fp
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
	// Fingerprint before Build, not after: both read live mesh state via
	// ExtraFingerprint, and the API server starts accepting slave
	// registrations before this runs. Fingerprinting second would let a
	// slave register in between, recording a fingerprint for a mesh
	// smaller than what Build actually picked up — the next wakeup would
	// then see no change and skip the rebuild, silently never probing
	// that slave. Fingerprinting first means a mid-window registration
	// makes the running scheduler a superset of the recorded fingerprint,
	// so the next wakeup sees a difference and rebuilds: extra work
	// instead of a silent gap.
	fp := opts.fingerprint(opts.Initial)
	sched, err := opts.Build(opts.Initial)
	if err != nil {
		return err
	}

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

		case <-opts.Reloads:
			// Always fetch the authoritative latest config from the store so
			// rapid back-to-back reloads (burst SIGHUP) don't leave us on a
			// stale snapshot that was dropped from the channel.
			newCfg := opts.Current()
			if opts.OnReload != nil {
				opts.OnReload(newCfg)
			}
			newFP := opts.fingerprint(newCfg)
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
