package slave

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
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

	// currentShim holds the latest config pulled from the master. Updated
	// atomically by refreshLoop; read by the lifecycle via the Current closure.
	currentShim atomic.Pointer[config.Config]

	pushEvery  time.Duration
	pullEvery  time.Duration // 0 = poll once on startup only
	batchLimit int
}

const defaultPullEvery = 60 * time.Second

// NewRunner builds a Runner from the slave's local minimal config. Caller is
// expected to have already validated it with Config.ValidateMinimal.
func NewRunner(log *slog.Logger, local *config.Config, version string) *Runner {
	pushEvery := 5 * time.Second
	if local.Cluster.PushEvery != "" {
		if d, err := time.ParseDuration(local.Cluster.PushEvery); err == nil && d > 0 {
			pushEvery = d
		}
	}
	pullEvery := defaultPullEvery
	if raw := local.Cluster.PullEvery; raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d >= 0 {
			// d == 0 is meaningful: disables periodic refresh. Only negatives
			// (and unparseable strings) fall back to the default.
			pullEvery = d
		} else {
			log.Warn("invalid cluster.pull_every, using default",
				"value", raw, "default", defaultPullEvery, "err", err)
		}
	}
	return &Runner{
		log:        log,
		local:      local,
		client:     NewClient(local.Cluster.MasterURL, local.Cluster.Token, local.Cluster.Name, version, local.Cluster.Advertise),
		sink:       NewPushSink(log, 600),
		pushEvery:  pushEvery,
		pullEvery:  pullEvery,
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
	pullEveryLog := any(r.pullEvery)
	if r.pullEvery == 0 {
		pullEveryLog = "disabled"
	}
	r.log.Info("slave starting",
		"name", r.local.Cluster.Name,
		"master", r.local.Cluster.MasterURL,
		"push_every", r.pushEvery,
		"pull_every", pullEveryLog)

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

	reloads := make(chan struct{}, 1)

	pushDone := make(chan struct{})
	go func() {
		defer close(pushDone)
		if err := r.pushLoop(runCtx); err != nil {
			cancelRun(err)
		}
	}()

	initial := r.applyConfig(resp)

	refreshDone := make(chan struct{})
	go func() {
		defer close(refreshDone)
		r.refreshLoop(runCtx, cancelRun, etag, reloads)
	}()

	lifecycleErr := scheduler.RunLifecycle(runCtx, scheduler.LifecycleOptions{
		Log:     r.log,
		Initial: initial,
		Current: r.currentShim.Load,
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

// applyConfig fans a freshly pulled master config out to everything that
// consumes it, so a pull site cannot update one consumer and forget another.
func (r *Runner) applyConfig(resp cluster.ClusterConfigResp) *config.Config {
	r.sink.SetHopMarkers(resp.HopMarkers)
	shim := buildShim(resp, r.local.Cluster)
	r.currentShim.Store(shim)
	return shim
}

// refreshLoop pulls config from the master every r.pullEvery. On a successful
// non-304 response it sends a rebuilt shim to reloads — the lifecycle helper's
// fingerprint check decides whether a restart is actually needed. A 401
// cancels runCtx with cause ErrAuth and returns.
//
// When r.pullEvery == 0 the loop returns immediately: the initial /config
// pull (done before Run's goroutines start) stays authoritative for this
// slave's lifetime and the operator must restart the process — or rotate
// the target list on the master and trigger a slave restart — to pick up
// changes. No goroutine is kept alive in that mode.
func (r *Runner) refreshLoop(ctx context.Context, cancelRun context.CancelCauseFunc, etag string, reloads chan<- struct{}) {
	if r.pullEvery == 0 {
		return
	}
	t := time.NewTicker(r.pullEvery)
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
			// applyConfig stores the shim before we signal, so the
			// lifecycle's Current() call always sees the latest config.
			r.applyConfig(newResp)
			select {
			case reloads <- struct{}{}:
			case <-ctx.Done():
				return
			}
		}
	}
}

// registerForever retries /register with exponential backoff capped at 30s.
// Returns non-nil only when ctx is cancelled before the first success, or on
// a verdict retrying cannot change: 401 (operator must rotate the token) and
// ErrRejected — the master answers the same bytes identically forever (an
// invalid cluster.name, an oversized advertise), so retrying leaves a
// "running" slave that never registers and probes nothing, where exiting
// non-zero with the master's message tells the operator what to fix.
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
		if errors.Is(err, ErrRejected) {
			r.log.Error("master permanently rejected registration, exiting", "err", err)
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
// ErrRejected is fatal like in registerForever: with no config ever pulled
// there is nothing stale to keep running on, so a permanent 4xx retried
// forever is a slave that never probes.
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
		if errors.Is(err, ErrRejected) {
			r.log.Error("master permanently rejected the initial config pull, exiting", "err", err)
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
	registry, err := probe.Build(shim.Probes, shim.Interval, shim.Pings)
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
//   - ErrRejected: a permanent 4xx; drop the batch loudly, because retrying it
//     blocks every cycle queued behind it until drop-oldest eats them
//   - ErrUnregistered: master's registry has no entry for us; re-register and
//     requeue. Registration is otherwise only attempted at boot, so with
//     cluster.pull_every "0" (no /config refresh to heartbeat through) a
//     master restart would leave the slave refused for the life of the
//     process.
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
	if errors.Is(err, ErrRejected) {
		r.log.Error("master permanently rejected the batch, dropping it", "count", len(batch), "err", err)
		return nil
	}
	if errors.Is(err, ErrUnregistered) {
		r.log.Warn("master does not know this slave, re-registering", "count", len(batch))
		if rerr := r.client.Register(ctx); rerr != nil {
			// ErrRejected is fatal here for the reason it is in
			// registerForever: the master answers these bytes identically
			// forever, so retrying leaves a slave that pushes nothing while
			// the ring head-of-line blocks and drop-oldest eats the live
			// cycles behind this batch — silent data loss under a process
			// reporting healthy. Requeued because the process is exiting.
			if errors.Is(rerr, ErrAuth) {
				r.sink.Requeue(batch)
				return rerr
			}
			if errors.Is(rerr, ErrRejected) {
				r.log.Error("master permanently rejected re-registration, exiting", "err", rerr)
				r.sink.Requeue(batch)
				return rerr
			}
			r.log.Warn("re-register failed", "err", rerr)
		}
		r.sink.Requeue(batch)
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
