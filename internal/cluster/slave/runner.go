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

	// runCtx gates the scheduler, refresh, and push loops. Any of them can
	// cancel it with cause = ErrAuth on a 401, so Run returns that error
	// verbatim to main() which exits non-zero.
	runCtx, cancelRun := context.WithCancelCause(ctx)
	defer cancelRun(nil)

	reloads := make(chan *config.Config, 1)

	pushDone := make(chan struct{})
	go func() {
		defer close(pushDone)
		if err := r.pushLoop(runCtx); err != nil {
			cancelRun(err)
		}
	}()

	refreshDone := make(chan struct{})
	go func() {
		defer close(refreshDone)
		r.refreshLoop(runCtx, cancelRun, etag, reloads)
	}()

	initial := buildShim(resp, r.local.Cluster)
	lifecycleErr := scheduler.RunLifecycle(runCtx, scheduler.LifecycleOptions{
		Log:     r.log,
		Initial: initial,
		Build:   func(c *config.Config) (*scheduler.Scheduler, error) { return r.buildScheduler(c) },
		Reloads: reloads,
	})
	if lifecycleErr != nil {
		// Initial build failure — surface it so main() exits non-zero.
		cancelRun(fmt.Errorf("build scheduler: %w", lifecycleErr))
	}

	<-pushDone
	<-refreshDone
	r.finalFlush()

	if cause := context.Cause(runCtx); cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	return nil
}

// refreshLoop pulls config from the master every 60s. On a successful non-304
// response it sends a rebuilt shim to reloads — the lifecycle helper's
// fingerprint check decides whether a restart is actually needed. A 401
// cancels runCtx with cause ErrAuth and returns.
func (r *Runner) refreshLoop(ctx context.Context, cancelRun context.CancelCauseFunc, etag string, reloads chan<- *config.Config) {
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			newResp, newEtag, err := r.client.PullConfig(ctx, etag)
			if errors.Is(err, ErrNotModified) {
				continue
			}
			if errors.Is(err, ErrAuth) {
				r.log.Error("token rejected, exiting")
				cancelRun(err)
				return
			}
			if err != nil {
				r.log.Warn("config refresh failed, keeping stale", "err", err)
				continue
			}
			etag = newEtag
			shim := buildShim(newResp, r.local.Cluster)
			select {
			case reloads <- shim:
			case <-ctx.Done():
				return
			}
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
