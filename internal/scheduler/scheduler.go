package scheduler

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/probe"
	"github.com/tumult/gosmokeping/internal/stats"
)

// Cycle is a single completed cycle of measurement for one target.
type Cycle struct {
	Time      time.Time
	Target    config.TargetRef
	ProbeName string
	RTTs      []time.Duration
	Sent      int
	LossCount int
	Summary   stats.Summary
	// Hops is populated for MTR cycles only; nil for every other probe type.
	Hops []probe.Hop
	// HTTPSamples is populated for HTTP cycles only.
	HTTPSamples []probe.HTTPSample
}

// Sink receives completed cycles. Implementations write to storage, evaluate
// alerts, etc. Must be safe for concurrent use.
type Sink interface {
	OnCycle(ctx context.Context, c Cycle)
}

type Scheduler struct {
	log      *slog.Logger
	registry *probe.Registry
	sink     Sink
	cfg      *config.Config
	now      func() time.Time
}

func New(log *slog.Logger, registry *probe.Registry, sink Sink, cfg *config.Config) *Scheduler {
	return &Scheduler{
		log:      log,
		registry: registry,
		sink:     sink,
		cfg:      cfg,
		now:      time.Now,
	}
}

// Run fires a probe cycle for every target every cfg.Interval. Each target has
// its own goroutine and its first cycle is jittered within [0, Interval) to
// avoid synchronized bursts. Returns when ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, t := range s.cfg.AllTargets() {
		p, ok := s.registry.Get(t.Target.Probe)
		if !ok {
			s.log.Warn("probe not found for target", "target", t.ID(), "probe", t.Target.Probe)
			continue
		}
		wg.Add(1)
		go func(ref config.TargetRef, pr probe.Probe) {
			defer wg.Done()
			s.loopTarget(ctx, ref, pr)
		}(t, p)
	}
	wg.Wait()
}

func (s *Scheduler) loopTarget(ctx context.Context, ref config.TargetRef, pr probe.Probe) {
	interval := s.cfg.Interval
	// Initial jitter so targets don't fire simultaneously.
	jitter := time.Duration(rand.Int64N(int64(interval)))
	select {
	case <-ctx.Done():
		return
	case <-time.After(jitter):
	}

	t := time.NewTicker(interval)
	defer t.Stop()

	// Fire once immediately after jitter, then on each tick.
	s.runCycle(ctx, ref, pr)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.runCycle(ctx, ref, pr)
		}
	}
}

func (s *Scheduler) runCycle(ctx context.Context, ref config.TargetRef, pr probe.Probe) {
	target := probe.Target{
		Name:  ref.Target.Name,
		Group: ref.Group,
		Host:  ref.Target.Host,
		URL:   ref.Target.URL,
	}

	cycleCtx, cancel := context.WithTimeout(ctx, s.cfg.Interval)
	defer cancel()

	res, err := pr.Probe(cycleCtx, target, s.cfg.Pings)
	if err != nil {
		s.log.Warn("probe error", "target", ref.ID(), "err", err)
		if res == nil {
			res = &probe.Result{Sent: s.cfg.Pings, LossCount: s.cfg.Pings}
		}
	}

	c := Cycle{
		Time:        s.now(),
		Target:      ref,
		ProbeName:   ref.Target.Probe,
		RTTs:        res.RTTs,
		Sent:        res.Sent,
		LossCount:   res.LossCount,
		Summary:     stats.Compute(res.RTTs),
		Hops:        res.Hops,
		HTTPSamples: res.HTTPSamples,
	}
	s.sink.OnCycle(ctx, c)
}
