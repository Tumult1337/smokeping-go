package slave

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/tumult/gosmokeping/internal/cluster"
	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/probe"
	"github.com/tumult/gosmokeping/internal/scheduler"
)

// Runner owns the slave lifecycle: register loop, config refresh, scheduler
// (re)start on target-set change, and the push loop. Built from the slave's
// local config.Config (minimal shape) — the master provides the target list.
type Runner struct {
	log    *slog.Logger
	local  *config.Config
	client *Client
	sink   *PushSink

	pushEvery  time.Duration
	batchLimit int
}

// NewRunner builds a Runner from the slave's local minimal config. Caller is
// expected to have already validated it with Config.ValidateMinimal.
func NewRunner(log *slog.Logger, local *config.Config, version string) *Runner {
	pushEvery := 5 * time.Second
	if local.Cluster.PushEvery != "" {
		if d, err := time.ParseDuration(local.Cluster.PushEvery); err == nil && d > 0 {
			pushEvery = d
		}
	}
	return &Runner{
		log:        log,
		local:      local,
		client:     NewClient(local.Cluster.MasterURL, local.Cluster.Token, local.Cluster.Name, version),
		sink:       NewPushSink(log, 600),
		pushEvery:  pushEvery,
		batchLimit: 100,
	}
}

// Run blocks until ctx is cancelled. On the happy path it registers with the
// master, pulls an initial config, then runs the scheduler + push loop + the
// periodic config refresher concurrently. On SIGINT/SIGTERM the outer
// context is cancelled and Run does a best-effort final flush before returning.
//
// If the master rejects our token mid-flight (either a refresh or a push)
// we treat that as fatal and return ErrAuth so the caller can exit non-zero.
func (r *Runner) Run(ctx context.Context) error {
	r.log.Info("slave starting",
		"name", r.local.Cluster.Name,
		"master", r.local.Cluster.MasterURL,
		"push_every", r.pushEvery)

	if err := r.registerForever(ctx); err != nil {
		return err
	}

	resp, etag, err := r.pullConfigInitial(ctx)
	if err != nil {
		return err
	}

	// runCtx gates both the scheduler and the push loop. We cancel it with a
	// cause of ErrAuth when either loop sees a 401, so Run returns that error
	// verbatim to main() which exits non-zero.
	runCtx, cancelRun := context.WithCancelCause(ctx)
	defer cancelRun(nil)

	sched, err := r.buildScheduler(buildShim(resp, r.local.Cluster))
	if err != nil {
		return fmt.Errorf("build scheduler: %w", err)
	}
	fingerprint := scheduler.Fingerprint(buildShim(resp, r.local.Cluster))

	schedCtx, schedCancel := context.WithCancel(runCtx)
	schedDone := make(chan struct{})
	go func() {
		sched.Run(schedCtx)
		close(schedDone)
	}()

	pushDone := make(chan struct{})
	go func() {
		if err := r.pushLoop(runCtx); err != nil {
			cancelRun(err)
		}
		close(pushDone)
	}()

	refreshT := time.NewTicker(60 * time.Second)
	defer refreshT.Stop()

	for {
		select {
		case <-runCtx.Done():
			schedCancel()
			<-schedDone
			<-pushDone
			r.finalFlush()
			if cause := context.Cause(runCtx); cause != nil && !errors.Is(cause, context.Canceled) {
				return cause
			}
			return nil

		case <-refreshT.C:
			newResp, newEtag, err := r.client.PullConfig(runCtx, etag)
			if errors.Is(err, ErrNotModified) {
				continue
			}
			if errors.Is(err, ErrAuth) {
				r.log.Error("token rejected, exiting")
				cancelRun(err)
				continue
			}
			if err != nil {
				r.log.Warn("config refresh failed, keeping stale", "err", err)
				continue
			}
			etag = newEtag
			newShim := buildShim(newResp, r.local.Cluster)
			newFingerprint := scheduler.Fingerprint(newShim)
			if newFingerprint == fingerprint {
				continue
			}
			r.log.Info("config changed, restarting scheduler")
			schedCancel()
			<-schedDone

			newSched, err := r.buildScheduler(newShim)
			if err != nil {
				r.log.Error("rebuild scheduler failed, keeping previous target set", "err", err)
				// Restart the previous scheduler so we don't go dark.
				schedCtx, schedCancel = context.WithCancel(runCtx)
				schedDone = make(chan struct{})
				go func() { sched.Run(schedCtx); close(schedDone) }()
				continue
			}
			sched, fingerprint = newSched, newFingerprint
			schedCtx, schedCancel = context.WithCancel(runCtx)
			schedDone = make(chan struct{})
			go func() { sched.Run(schedCtx); close(schedDone) }()
		}
	}
}

// registerForever retries /register with exponential backoff capped at 30s.
// Returns non-nil only when ctx is cancelled before the first success, or
// when the master returns 401 (fatal — operator must rotate the token).
func (r *Runner) registerForever(ctx context.Context) error {
	backoff := time.Second
	for {
		err := r.client.Register(ctx)
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrAuth) {
			return err
		}
		r.log.Warn("register failed, will retry", "err", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

// pullConfigInitial keeps trying until a non-304 /config comes back. Matches
// the "do not probe before first successful config pull" rule from the plan.
func (r *Runner) pullConfigInitial(ctx context.Context) (cluster.ClusterConfigResp, string, error) {
	backoff := time.Second
	for {
		resp, etag, err := r.client.PullConfig(ctx, "")
		if err == nil {
			return resp, etag, nil
		}
		if errors.Is(err, ErrAuth) {
			return cluster.ClusterConfigResp{}, "", err
		}
		r.log.Warn("initial config pull failed, will retry", "err", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return cluster.ClusterConfigResp{}, "", ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

func (r *Runner) buildScheduler(shim *config.Config) (*scheduler.Scheduler, error) {
	registry, err := probe.Build(shim.Probes)
	if err != nil {
		return nil, err
	}
	return scheduler.NewWithSource(r.log, registry, r.sink, shim, r.local.Cluster.Name), nil
}

// pushLoop flushes the buffer on every pushEvery tick. Returns ErrAuth on
// fatal auth failure so Run can propagate it to main(); nil on normal
// ctx-cancelled shutdown.
func (r *Runner) pushLoop(ctx context.Context) error {
	t := time.NewTicker(r.pushEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := r.flushOnce(ctx); err != nil {
				return err
			}
		}
	}
}

// flushOnce drains up to batchLimit cycles and ships them. Error semantics:
//   - ErrAuth: returned up so Run exits non-zero (token rotation required)
//   - ErrNotFound: master lost our state; drop the batch (next /register
//     re-establishes us)
//   - anything else (5xx, network, timeout): requeue for the next tick
//   - ctx cancellation during shutdown: returns nil so finalFlush can run
func (r *Runner) flushOnce(ctx context.Context) error {
	batch := r.sink.Drain(r.batchLimit)
	if len(batch) == 0 {
		return nil
	}
	err := r.client.PushCycles(ctx, cluster.CycleBatch{
		Source: r.local.Cluster.Name,
		Cycles: batch,
	})
	if err == nil {
		r.log.Debug("pushed cycle batch", "count", len(batch))
		return nil
	}
	if errors.Is(err, ErrAuth) {
		r.log.Error("push auth failed, exiting", "count", len(batch))
		return err
	}
	if errors.Is(err, ErrNotFound) {
		r.log.Warn("master returned 404, dropping batch", "count", len(batch))
		return nil
	}
	r.log.Warn("push failed, requeueing", "count", len(batch), "err", err)
	r.sink.Requeue(batch)
	return nil
}

// finalFlush is a single best-effort flush attempt on shutdown with a short
// deadline — we do not want shutdown to hang on a dead master.
func (r *Runner) finalFlush() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = r.flushOnce(ctx)
}
