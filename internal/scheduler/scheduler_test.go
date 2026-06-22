package scheduler

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/probe"
)

type fakeProbe struct {
	name string
	rtts []time.Duration
	loss int
}

func (f *fakeProbe) Name() string { return f.name }
func (f *fakeProbe) Probe(ctx context.Context, t probe.Target, count int) (*probe.Result, error) {
	return &probe.Result{Sent: count, LossCount: f.loss, RTTs: f.rtts}, nil
}

type recordingSink struct {
	mu     sync.Mutex
	cycles []Cycle
}

func (r *recordingSink) OnCycle(_ context.Context, c Cycle) {
	r.mu.Lock()
	r.cycles = append(r.cycles, c)
	r.mu.Unlock()
}

func (r *recordingSink) snapshot() []Cycle {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Cycle(nil), r.cycles...)
}

// panickingSink panics on its first OnCycle, then records calls. Used to prove
// runCycle's recover keeps the per-target goroutine alive — without it the
// first panic would crash the test binary (unrecovered goroutine panic).
type panickingSink struct {
	mu    sync.Mutex
	calls int
}

func (p *panickingSink) OnCycle(_ context.Context, _ Cycle) {
	p.mu.Lock()
	p.calls++
	first := p.calls == 1
	p.mu.Unlock()
	if first {
		panic("sink boom")
	}
}

func (p *panickingSink) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func TestSchedulerRecoversFromSinkPanic(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	reg := probe.NewRegistry()
	reg.Register(&fakeProbe{name: "fake", rtts: []time.Duration{10 * time.Millisecond}})

	cfg := &config.Config{
		Interval: 20 * time.Millisecond,
		Pings:    1,
		Probes:   map[string]config.Probe{"fake": {Type: "icmp", Timeout: time.Second}},
		Targets: []config.Group{{
			Group:   "g",
			Targets: []config.Target{{Name: "a", Host: "1.1.1.1", Probe: "fake"}},
		}},
	}

	sink := &panickingSink{}
	s := New(log, reg, sink, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	s.Run(ctx)

	if got := sink.count(); got < 2 {
		t.Fatalf("target stopped probing after a sink panic: got %d OnCycle calls, want ≥2", got)
	}
}

func TestSchedulerRunsAndStops(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	reg := probe.NewRegistry()
	reg.Register(&fakeProbe{
		name: "fake",
		rtts: []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond},
	})

	cfg := &config.Config{
		Interval: 50 * time.Millisecond,
		Pings:    3,
		Probes:   map[string]config.Probe{"fake": {Type: "icmp", Timeout: time.Second}},
		Targets: []config.Group{{
			Group: "g",
			Targets: []config.Target{
				{Name: "a", Host: "1.1.1.1", Probe: "fake"},
				{Name: "b", Host: "2.2.2.2", Probe: "fake"},
			},
		}},
	}

	sink := &recordingSink{}
	s := New(log, reg, sink, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	s.Run(ctx)

	got := sink.snapshot()
	if len(got) == 0 {
		t.Fatal("no cycles recorded")
	}
	seen := map[string]int{}
	for _, c := range got {
		seen[c.Target.ID()]++
		if c.Summary.Median == 0 {
			t.Errorf("cycle %s: median = 0", c.Target.ID())
		}
	}
	if seen["g/a"] == 0 || seen["g/b"] == 0 {
		t.Errorf("expected both targets to run, got %+v", seen)
	}
}

// alwaysPanicSink panics on every OnCycle — used to prove fanout isolation.
type alwaysPanicSink struct{}

func (alwaysPanicSink) OnCycle(context.Context, Cycle) { panic("boom") }

// TestFanoutIsolatesSinkPanic proves a panic in one sink does not stop later
// sinks from receiving the cycle. This guards the alert evaluator from being
// silently skipped when an earlier sink (e.g. the storage writer) panics, and
// protects the slave-inbound path which feeds the fanout with no outer recover.
func TestFanoutIsolatesSinkPanic(t *testing.T) {
	rec := &recordingSink{}
	fan := Fanout(slog.New(slog.NewTextHandler(io.Discard, nil)), alwaysPanicSink{}, rec)

	fan.OnCycle(context.Background(), Cycle{Source: "x"})

	if got := len(rec.snapshot()); got != 1 {
		t.Fatalf("downstream sink should still receive the cycle after an upstream panic; got %d cycles", got)
	}
}
