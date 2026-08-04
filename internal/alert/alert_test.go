package alert

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/probe"
	"github.com/tumult/gosmokeping/internal/scheduler"
	"github.com/tumult/gosmokeping/internal/slavehealth"
	"github.com/tumult/gosmokeping/internal/stats"
)

func TestParseConditionErrors(t *testing.T) {
	bad := []string{"", "loss_pct", "rtt_median 50ms", "unknown > 1", "loss_pct > abc"}
	for _, s := range bad {
		if _, err := ParseCondition(s); err == nil {
			t.Errorf("expected error for %q", s)
		}
	}
}

func TestParseConditionOK(t *testing.T) {
	cases := map[string]struct {
		field string
		op    Op
		value float64
	}{
		"loss_pct > 5":      {"loss_pct", OpGT, 5},
		"rtt_median > 50ms": {"rtt_median", OpGT, 50},
		"rtt_p95 >= 100":    {"rtt_p95", OpGE, 100},
		"loss_pct != 0":     {"loss_pct", OpNE, 0},
	}
	for in, want := range cases {
		c, err := ParseCondition(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if c.Field != want.field || c.Op != want.op || c.Value != want.value {
			t.Errorf("%q: got field=%s op=%s value=%v", in, c.Field, c.Op, c.Value)
		}
	}
}

func TestConditionEval(t *testing.T) {
	c, _ := ParseCondition("rtt_median > 50ms")
	cy := scheduler.Cycle{
		Summary: stats.Summary{Median: 100 * time.Millisecond},
	}
	if !c.Eval(cy) {
		t.Error("expected condition to fire")
	}
	cy.Summary.Median = 10 * time.Millisecond
	if c.Eval(cy) {
		t.Error("expected condition not to fire")
	}
}

type fakeDispatcher struct {
	mu     sync.Mutex
	events []Event
}

func (f *fakeDispatcher) Dispatch(_ context.Context, e Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
}

func (f *fakeDispatcher) snapshot() []Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Event(nil), f.events...)
}

func TestEvaluatorLifecycle(t *testing.T) {
	cfg := &config.Config{
		Interval: time.Minute,
		Pings:    10,
		Storage:  config.Storage{ClickHouse: config.ClickHouse{Addr: "ch:9000"}},
		Probes:   map[string]config.Probe{"icmp": {Type: "icmp", Timeout: time.Second}},
		Alerts: map[string]config.Alert{
			"high-latency": {Condition: "rtt_median > 50ms", Sustained: 2, Actions: []string{"log"}},
		},
		Actions: map[string]config.Action{"log": {Type: "log"}},
		Targets: []config.Group{{
			Group: "g",
			Targets: []config.Target{
				{Name: "a", Host: "1.1.1.1", Probe: "icmp", Alerts: []string{"high-latency"}},
			},
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("invalid config: %v", err)
	}
	store := config.NewStore("/dev/null", cfg)
	disp := &fakeDispatcher{}
	e, err := NewEvaluator(slog.New(slog.NewTextHandler(io.Discard, nil)), store, disp)
	if err != nil {
		t.Fatalf("new evaluator: %v", err)
	}

	ref := cfg.AllTargets()[0]
	highCycle := scheduler.Cycle{
		Target:    ref,
		ProbeName: "icmp",
		Sent:      10,
		Summary:   stats.Summary{Median: 100 * time.Millisecond},
	}
	okCycle := scheduler.Cycle{
		Target:    ref,
		ProbeName: "icmp",
		Sent:      10,
		Summary:   stats.Summary{Median: 10 * time.Millisecond},
	}

	ctx := context.Background()
	e.OnCycle(ctx, highCycle) // OK → PENDING
	e.OnCycle(ctx, highCycle) // PENDING → FIRING (sustained=2)
	e.OnCycle(ctx, highCycle) // FIRING → FIRING (no event)
	e.OnCycle(ctx, okCycle)   // FIRING → OK

	events := disp.snapshot()
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3: %+v", len(events), events)
	}
	if events[0].Next != StatePending || events[1].Next != StateFiring || events[2].Next != StateOK {
		t.Errorf("unexpected state progression: %v %v %v",
			events[0].Next, events[1].Next, events[2].Next)
	}
}

// TestEvaluatorPerSourceState verifies that two sources probing the same
// target keep independent sustained-hit counters, so a firing slave doesn't
// reset a pending master (or vice-versa).
func TestEvaluatorPerSourceState(t *testing.T) {
	cfg := &config.Config{
		Interval: time.Minute,
		Pings:    10,
		Storage:  config.Storage{ClickHouse: config.ClickHouse{Addr: "ch:9000"}},
		Probes:   map[string]config.Probe{"icmp": {Type: "icmp", Timeout: time.Second}},
		Alerts: map[string]config.Alert{
			"high-latency": {Condition: "rtt_median > 50ms", Sustained: 2, Actions: []string{"log"}},
		},
		Actions: map[string]config.Action{"log": {Type: "log"}},
		Targets: []config.Group{{
			Group: "g",
			Targets: []config.Target{
				{Name: "a", Host: "1.1.1.1", Probe: "icmp", Alerts: []string{"high-latency"}},
			},
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("invalid config: %v", err)
	}
	store := config.NewStore("/dev/null", cfg)
	disp := &fakeDispatcher{}
	e, err := NewEvaluator(slog.New(slog.NewTextHandler(io.Discard, nil)), store, disp)
	if err != nil {
		t.Fatalf("new evaluator: %v", err)
	}

	ref := cfg.AllTargets()[0]
	high := func(src string) scheduler.Cycle {
		return scheduler.Cycle{
			Target:    ref,
			ProbeName: "icmp",
			Source:    src,
			Sent:      10,
			Summary:   stats.Summary{Median: 100 * time.Millisecond},
		}
	}
	ok := func(src string) scheduler.Cycle {
		c := high(src)
		c.Summary.Median = 10 * time.Millisecond
		return c
	}

	ctx := context.Background()
	// Master trips first, then resolves. Slave climbs through PENDING.
	e.OnCycle(ctx, high("master"))  // master: OK → PENDING
	e.OnCycle(ctx, high("slave-a")) // slave: OK → PENDING (independent)
	e.OnCycle(ctx, ok("master"))    // master: PENDING → OK
	e.OnCycle(ctx, high("slave-a")) // slave: PENDING → FIRING (sustained=2)

	events := disp.snapshot()
	if len(events) != 4 {
		t.Fatalf("got %d events, want 4: %+v", len(events), events)
	}
	want := []struct {
		source string
		next   State
	}{
		{"master", StatePending},
		{"slave-a", StatePending},
		{"master", StateOK},
		{"slave-a", StateFiring},
	}
	for i, w := range want {
		if events[i].Cycle.Source != w.source || events[i].Next != w.next {
			t.Errorf("event[%d]: got source=%q next=%s, want source=%q next=%s",
				i, events[i].Cycle.Source, events[i].Next, w.source, w.next)
		}
	}
}

func TestDispatcherDiscord(t *testing.T) {
	var mu sync.Mutex
	var gotBodies []map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		mu.Lock()
		gotBodies = append(gotBodies, body)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	cfg := &config.Config{
		Interval: time.Minute,
		Pings:    5,
		Storage:  config.Storage{ClickHouse: config.ClickHouse{Addr: "ch:9000"}},
		Probes:   map[string]config.Probe{"icmp": {Type: "icmp", Timeout: time.Second}},
		Alerts:   map[string]config.Alert{"down": {Condition: "loss_pct > 0", Sustained: 1, Actions: []string{"discord"}}},
		Actions:  map[string]config.Action{"discord": {Type: "discord", URL: srv.URL}},
		Targets: []config.Group{{
			Group: "g", Targets: []config.Target{{Name: "a", Host: "1.1.1.1", Probe: "icmp"}},
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	store := config.NewStore("/dev/null", cfg)
	d := NewDispatcher(slog.New(slog.NewTextHandler(io.Discard, nil)), store)
	d.client = srv.Client()

	ref := cfg.AllTargets()[0]
	ev := Event{
		Time:      time.Unix(1_700_000_000, 0),
		Target:    ref,
		AlertName: "down",
		Alert:     cfg.Alerts["down"],
		Prev:      StatePending,
		Next:      StateFiring,
		Cycle: scheduler.Cycle{
			Target: ref, ProbeName: "icmp", Sent: 5, LossCount: 5,
			Summary: stats.Summary{Median: 42 * time.Millisecond},
			Hops: []probe.Hop{
				{Index: 1, IP: "192.168.1.1", Sent: 5, Lost: 0, RTTs: []time.Duration{2 * time.Millisecond, 2 * time.Millisecond}},
				{Index: 2, IP: "", Sent: 5, Lost: 5},
				{Index: 3, IP: "1.1.1.1", Sent: 5, Lost: 5},
			},
		},
	}

	snapshot := func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), gotBodies...)
	}

	d.Dispatch(context.Background(), ev)
	bodies := snapshot()
	if len(bodies) != 1 {
		t.Fatalf("got %d calls, want 1", len(bodies))
	}
	embeds, ok := bodies[0]["embeds"].([]any)
	if !ok || len(embeds) != 1 {
		t.Fatalf("embeds shape: %v", bodies[0])
	}
	embed := embeds[0].(map[string]any)
	if color, _ := embed["color"].(float64); int(color) != 0xE53935 {
		t.Errorf("firing color = %v, want red", embed["color"])
	}
	desc, _ := embed["description"].(string)
	if !strings.Contains(desc, "**Path**") {
		t.Errorf("description missing MTR path block:\n%s", desc)
	}
	if !strings.Contains(desc, "192.168.1.1") || !strings.Contains(desc, "*") {
		t.Errorf("description missing expected hop rows:\n%s", desc)
	}

	// Cycle without Hops → no MTR block.
	mu.Lock()
	gotBodies = nil
	mu.Unlock()
	ev.Cycle.Hops = nil
	d.Dispatch(context.Background(), ev)
	bodies = snapshot()
	if len(bodies) != 1 {
		t.Fatalf("got %d calls, want 1", len(bodies))
	}
	desc2, _ := bodies[0]["embeds"].([]any)[0].(map[string]any)["description"].(string)
	if strings.Contains(desc2, "**Path**") {
		t.Errorf("description should not contain MTR block when Hops is nil:\n%s", desc2)
	}
}

// Without quorum, behaviour is exactly as before: each source dispatches on
// its own transition.
func TestNoQuorumDispatchesPerSource(t *testing.T) {
	ev, disp := newTestEvaluator(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"},
	})

	ev.OnCycle(context.Background(), lossyCycle("master", 100))
	ev.OnCycle(context.Background(), lossyCycle("tokyo-1", 100))

	if got := len(disp.events()); got != 2 {
		t.Fatalf("got %d events, want 2 (one per source)", got)
	}
}

// A single firing source out of three must not reach a majority.
func TestQuorumSuppressesMinority(t *testing.T) {
	ev, disp := newTestEvaluator(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"},
		Quorum: config.Quorum{Majority: true},
	})

	ctx := context.Background()
	ev.OnCycle(ctx, healthyCycle("master"))
	ev.OnCycle(ctx, healthyCycle("tokyo-1"))
	ev.OnCycle(ctx, lossyCycle("frankfurt-1", 100))

	if got := len(disp.events()); got != 0 {
		t.Fatalf("got %d events, want 0 (1 of 3 is not a majority)", got)
	}
}

func TestQuorumDispatchesOnMajority(t *testing.T) {
	ev, disp := newTestEvaluator(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"},
		Quorum: config.Quorum{Majority: true},
	})

	ctx := context.Background()
	ev.OnCycle(ctx, healthyCycle("master"))
	ev.OnCycle(ctx, lossyCycle("tokyo-1", 100))
	ev.OnCycle(ctx, lossyCycle("frankfurt-1", 100))

	evs := disp.events()
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1 aggregate transition", len(evs))
	}
	if evs[0].Next != StateFiring {
		t.Fatalf("got next state %q, want %q", evs[0].Next, StateFiring)
	}
	if evs[0].Firing != 2 || evs[0].Live != 3 {
		t.Fatalf("got %d/%d firing/live, want 2/3", evs[0].Firing, evs[0].Live)
	}
}

// The aggregate must resolve exactly once, not once per recovering source.
func TestQuorumResolvesOnce(t *testing.T) {
	ev, disp := newTestEvaluator(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"},
		Quorum: config.Quorum{Majority: true},
	})

	ctx := context.Background()
	ev.OnCycle(ctx, lossyCycle("master", 100))
	ev.OnCycle(ctx, lossyCycle("tokyo-1", 100))
	disp.reset()

	ev.OnCycle(ctx, healthyCycle("master"))
	ev.OnCycle(ctx, healthyCycle("tokyo-1"))

	evs := disp.events()
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1 resolve", len(evs))
	}
	if evs[0].Next != StateOK {
		t.Fatalf("got next state %q, want %q", evs[0].Next, StateOK)
	}
}

// A dead slave must not hold the denominator up forever — otherwise one
// silent source permanently suppresses a real alert.
func TestQuorumPrunesStaleSources(t *testing.T) {
	ev, disp := newTestEvaluator(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"},
		Quorum: config.Quorum{Majority: true},
	})

	ctx := context.Background()
	base := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)

	// Three healthy sources, then two go silent.
	for _, src := range []string{"master", "tokyo-1", "frankfurt-1"} {
		c := healthyCycle(src)
		c.Time = base
		ev.OnCycle(ctx, c)
	}
	disp.reset()

	// Well past the staleness window (3x interval), only master reports.
	c := lossyCycle("master", 100)
	c.Time = base.Add(time.Hour)
	ev.OnCycle(ctx, c)

	evs := disp.events()
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1 (stale sources pruned, master is the majority of 1)", len(evs))
	}
	if evs[0].Live != 1 {
		t.Fatalf("got %d live sources, want 1", evs[0].Live)
	}
}

// An absolute threshold higher than the live source count can never fire.
// That is the operator's stated intent, not a bug — but it must not panic or
// dispatch.
func TestQuorumAbsoluteAboveLiveCount(t *testing.T) {
	ev, disp := newTestEvaluator(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"},
		Quorum: config.Quorum{Min: 5},
	})

	ctx := context.Background()
	ev.OnCycle(ctx, lossyCycle("master", 100))
	ev.OnCycle(ctx, lossyCycle("tokyo-1", 100))

	if got := len(disp.events()); got != 0 {
		t.Fatalf("got %d events, want 0", got)
	}
}

// Per-source sustained counters must stay independent under quorum: a laggy
// slave's hit streak must not advance the master's. The previous version of
// this test alternated lossy/healthy per source, which resets on every
// healthy cycle — a merged counter would also reset there and the test would
// pass either way. This sequence never sends a healthy cycle, so a merged
// counter (lossy, lossy, lossy = 3) and independent per-source counters
// (master=2, tokyo-1=1) diverge, and only a real bug (merging) fires.
func TestQuorumKeepsPerSourceSustainedIndependent(t *testing.T) {
	ev, disp := newTestEvaluator(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 3, Actions: []string{"log"},
		Quorum: config.Quorum{Majority: true},
	})

	ctx := context.Background()
	ev.OnCycle(ctx, lossyCycle("master", 100))  // master: 1
	ev.OnCycle(ctx, lossyCycle("tokyo-1", 100)) // tokyo-1: 1
	ev.OnCycle(ctx, lossyCycle("master", 100))  // master: 2 (still short of 3)

	if got := len(disp.events()); got != 0 {
		t.Fatalf("got %d events, want 0 (master=2, tokyo-1=1 consecutive hits; merged would be 3)", got)
	}
}

// An absolute quorum threshold met exactly (not just "never met") must
// dispatch. Only the never-fires case was covered end to end before this.
func TestQuorumAbsoluteDispatchesAtMin(t *testing.T) {
	ev, disp := newTestEvaluator(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"},
		Quorum: config.Quorum{Min: 2},
	})

	ctx := context.Background()
	ev.OnCycle(context.Background(), lossyCycle("master", 100))
	ev.OnCycle(ctx, lossyCycle("tokyo-1", 100))

	evs := disp.events()
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	if evs[0].Next != StateFiring {
		t.Fatalf("got next state %q, want %q", evs[0].Next, StateFiring)
	}
	if evs[0].Firing != 2 || evs[0].Live != 2 {
		t.Fatalf("got %d/%d firing/live, want 2/2", evs[0].Firing, evs[0].Live)
	}
}

// Finding 1: immediately after a restart, e.states is empty and only one
// source has reported. Threshold(1) == 1, so an ungated quorum would treat
// that lone source as a "majority" and dispatch FIRING, then dispatch OK the
// instant a peer's first (healthy) cycle arrives — a fire-then-resolve flap
// on every restart. Warm-up must suppress the FIRING dispatch until a second
// source has reported.
func TestQuorumWarmupSuppressesRestartFlap(t *testing.T) {
	ev, disp := newTestEvaluator(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"},
		Quorum: config.Quorum{Majority: true},
	})

	ctx := context.Background()
	ev.OnCycle(ctx, lossyCycle("master", 100))
	if got := len(disp.events()); got != 0 {
		t.Fatalf("got %d events after a single source's first cycle, want 0 (warm-up should hold)", got)
	}

	ev.OnCycle(ctx, healthyCycle("tokyo-1"))
	if got := len(disp.events()); got != 0 {
		t.Fatalf("got %d events, want 0 (never fired, so must not resolve either — that's the flap)", got)
	}
}

// The warm-up window keeps a genuinely single-source deployment working:
// once the staleness window elapses since the key's first cycle, quorum
// degrades to majority-of-1 rather than blocking forever.
func TestQuorumWarmupWindowElapsesForSingleSourceDeployment(t *testing.T) {
	ev, disp := newTestEvaluator(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"},
		Quorum: config.Quorum{Majority: true},
	})

	ctx := context.Background()
	base := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)

	c := lossyCycle("master", 100)
	c.Time = base
	ev.OnCycle(ctx, c)
	if got := len(disp.events()); got != 0 {
		t.Fatalf("got %d events, want 0 (still inside the warm-up window)", got)
	}

	c2 := lossyCycle("master", 100)
	c2.Time = base.Add(4 * time.Minute) // > 3x interval(1m) warm-up window
	ev.OnCycle(ctx, c2)

	evs := disp.events()
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1 (warm-up window elapsed; single source may fire)", len(evs))
	}
	if evs[0].Next != StateFiring {
		t.Fatalf("got next state %q, want %q", evs[0].Next, StateFiring)
	}
}

// Finding 2: e.agg must not survive an alert's Quorum.Enabled() flipping
// across a config reload. Sources recovering while quorum is off (and
// already reporting the resolve per-source) must not manufacture a stale
// duplicate aggregate "resolve" the moment quorum is switched back on.
func TestRefreshDropsAggOnQuorumToggle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	writeQuorumTestConfig(t, path, true)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	store := config.NewStore(path, cfg)
	disp := &recordingDispatcher{}
	ev, err := NewEvaluator(slog.New(slog.DiscardHandler), store, disp)
	if err != nil {
		t.Fatalf("NewEvaluator: %v", err)
	}

	ctx := context.Background()

	// Quorum on: both sources go lossy, majority-of-2 fires.
	ev.OnCycle(ctx, lossyCycle("master", 100))
	ev.OnCycle(ctx, lossyCycle("tokyo-1", 100))
	evs := disp.events()
	if len(evs) != 1 || evs[0].Next != StateFiring {
		t.Fatalf("setup: got %+v, want a single FIRING event", evs)
	}
	disp.reset()

	// Turn quorum off and reload.
	writeQuorumTestConfig(t, path, false)
	if err := store.Reload(); err != nil {
		t.Fatalf("reload (quorum off): %v", err)
	}
	if err := ev.Refresh(); err != nil {
		t.Fatalf("refresh (quorum off): %v", err)
	}

	// Both sources recover under per-source dispatch — each gets its own
	// resolve event, independent of the (now-inactive) aggregate.
	ev.OnCycle(ctx, healthyCycle("master"))
	ev.OnCycle(ctx, healthyCycle("tokyo-1"))
	evs = disp.events()
	if len(evs) != 2 {
		t.Fatalf("got %d per-source resolve events, want 2: %+v", len(evs), evs)
	}
	disp.reset()

	// Turn quorum back on and reload — nothing about the cycles has changed
	// since the per-source resolves above.
	writeQuorumTestConfig(t, path, true)
	if err := store.Reload(); err != nil {
		t.Fatalf("reload (quorum on): %v", err)
	}
	if err := ev.Refresh(); err != nil {
		t.Fatalf("refresh (quorum on): %v", err)
	}

	// One more healthy cycle to force the aggregate to re-evaluate. Both
	// sources are already OK, so a correct evaluator dispatches nothing.
	ev.OnCycle(ctx, healthyCycle("master"))
	if got := disp.events(); len(got) != 0 {
		t.Fatalf("got %d events, want 0 (no phantom resolve from stale pre-toggle aggregate): %+v", len(got), got)
	}
}

// writeQuorumTestConfig writes a config file with alert "quorum-test" on
// target core/gw, matching the TargetRef built by testCycle so the alert
// package's cycle helpers line up with a file-backed config.Store.
func writeQuorumTestConfig(t *testing.T, path string, quorum bool) {
	t.Helper()
	alertBody := `"condition":"loss_pct > 50","sustained":1,"actions":["log"]`
	if quorum {
		alertBody += `,"quorum":"majority"`
	}
	raw := `{
		"listen": ":8080",
		"interval": "1m",
		"pings": 20,
		"storage": {"clickhouse": {"addr": "ch:9000"}},
		"probes": {"icmp": {"type": "icmp", "timeout": "2s"}},
		"targets": [{"group":"core","targets":[
			{"name":"gw","host":"1.1.1.1","probe":"icmp","alerts":["quorum-test"]}
		]}],
		"alerts": {"quorum-test": {` + alertBody + `}},
		"actions": {"log": {"type":"log"}}
	}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

type recordingDispatcher struct {
	mu  sync.Mutex
	evs []Event
}

func (d *recordingDispatcher) Dispatch(_ context.Context, e Event) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.evs = append(d.evs, e)
}

func (d *recordingDispatcher) events() []Event {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Event, len(d.evs))
	copy(out, d.evs)
	return out
}

func (d *recordingDispatcher) reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.evs = nil
}

func newTestEvaluator(t *testing.T, a config.Alert) (*Evaluator, *recordingDispatcher) {
	t.Helper()
	cfg := &config.Config{
		Interval: time.Minute,
		Pings:    20,
		Alerts:   map[string]config.Alert{"quorum-test": a},
		Actions:  map[string]config.Action{"log": {Type: "log"}},
	}
	store := config.NewStore("", cfg)
	disp := &recordingDispatcher{}
	ev, err := NewEvaluator(slog.New(slog.DiscardHandler), store, disp)
	if err != nil {
		t.Fatalf("NewEvaluator: %v", err)
	}
	return ev, disp
}

// testCycle populates LossCount/Sent, the fields fieldGetter("loss_pct")
// actually reads (see condition.go) — not a "loss" field, which doesn't
// exist as a condition field.
func testCycle(source string, lossPct float64) scheduler.Cycle {
	return scheduler.Cycle{
		Time:   time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC),
		Source: source,
		Target: config.TargetRef{
			Group:  "core",
			Target: config.Target{Name: "gw", Probe: "icmp", Alerts: []string{"quorum-test"}},
		},
		Sent:      20,
		LossCount: int(lossPct / 100 * 20),
	}
}

func lossyCycle(source string, lossPct float64) scheduler.Cycle { return testCycle(source, lossPct) }
func healthyCycle(source string) scheduler.Cycle                { return testCycle(source, 0) }

// TestDispatchedHealthEventCarriesNoAddress guards the second egress the
// Probe/Public split does not cover. Event.Target comes from the scheduler's
// LocalTargets view, which holds the slave's real address, and
// ActionDispatcher renders operator-supplied templates directly over the
// Event — so an unscrubbed Event publishes the address to any webhook or exec
// action the moment cluster.health_alerts makes health alerts reachable.
func TestDispatchedHealthEventCarriesNoAddress(t *testing.T) {
	const slaveAddr = "10.44.0.7"
	cfg := &config.Config{
		Interval: time.Minute,
		Pings:    10,
		Storage:  config.Storage{ClickHouse: config.ClickHouse{Addr: "ch:9000"}},
		Probes:   map[string]config.Probe{"icmp": {Type: "icmp", Timeout: time.Second}},
		Alerts: map[string]config.Alert{
			"slave-unreachable": {Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"}},
		},
		Actions: map[string]config.Action{"log": {Type: "log"}},
	}
	store := config.NewStore("/dev/null", cfg)
	disp := &fakeDispatcher{}
	e, err := NewEvaluator(slog.New(slog.NewTextHandler(io.Discard, nil)), store, disp)
	if err != nil {
		t.Fatalf("new evaluator: %v", err)
	}

	// Exactly what master.LocalTargets hands the scheduler: the synthetic
	// health target with its real address still attached.
	ref := config.TargetRef{
		Group: slavehealth.Group,
		Target: config.Target{
			Name:   "tokyo-1",
			Title:  "tokyo-1",
			Host:   slaveAddr,
			Probe:  slavehealth.ProbeName,
			Family: "v4",
			Alerts: []string{"slave-unreachable"},
		},
	}
	cy := scheduler.Cycle{
		Target:    ref,
		ProbeName: slavehealth.ProbeName,
		Source:    "master",
		Sent:      10,
		LossCount: 10,
		Hops: []probe.Hop{
			{Index: 1, IP: "203.0.113.1"},
			{Index: 2, IP: slaveAddr},
		},
		// Never populated for an ICMP health probe in practice; set here so
		// the scrub is asserted to be exhaustive over scheduler.Cycle rather
		// than only over the fields that happen to carry an address today.
		HTTPSamples: []probe.HTTPSample{{Status: 200, Err: slaveAddr + ": refused"}},
	}
	e.OnCycle(context.Background(), cy)

	events := disp.snapshot()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(events), events)
	}
	ev := events[0]
	for name, got := range map[string]string{
		"Target.Target.Host":         ev.Target.Target.Host,
		"Target.Target.URL":          ev.Target.Target.URL,
		"Target.Target.Family":       ev.Target.Target.Family,
		"Cycle.Target.Target.Host":   ev.Cycle.Target.Target.Host,
		"Cycle.Target.Target.URL":    ev.Cycle.Target.Target.URL,
		"Cycle.Target.Target.Family": ev.Cycle.Target.Target.Family,
	} {
		if got != "" {
			t.Errorf("%s = %q on a dispatched health event, want empty", name, got)
		}
	}
	if len(ev.Cycle.Hops) != 0 {
		t.Errorf("Cycle.Hops survived on a health event: %+v", ev.Cycle.Hops)
	}
	if len(ev.Cycle.HTTPSamples) != 0 {
		t.Errorf("Cycle.HTTPSamples survived on a health event: %+v", ev.Cycle.HTTPSamples)
	}
	// The identifying fields must survive — a scrubbed event still has to say
	// which slave went down.
	if ev.Target.ID() != slavehealth.Group+"/tokyo-1" {
		t.Errorf("target identity lost: %q", ev.Target.ID())
	}
}

// TestDispatchedOrdinaryEventKeepsAddress is the inverted-guard counterpart:
// scrubbing must be scoped to the health group, or every alert template loses
// {{.Target.Target.Host}} — the field operators most rely on.
func TestDispatchedOrdinaryEventKeepsAddress(t *testing.T) {
	cfg := &config.Config{
		Interval: time.Minute,
		Pings:    10,
		Storage:  config.Storage{ClickHouse: config.ClickHouse{Addr: "ch:9000"}},
		Probes:   map[string]config.Probe{"icmp": {Type: "icmp", Timeout: time.Second}},
		Alerts: map[string]config.Alert{
			"high-loss": {Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"}},
		},
		Actions: map[string]config.Action{"log": {Type: "log"}},
		Targets: []config.Group{{
			Group: "core",
			Targets: []config.Target{
				{Name: "gw", Host: "192.0.2.1", Probe: "icmp", Family: "v4", Alerts: []string{"high-loss"}},
			},
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("invalid config: %v", err)
	}
	store := config.NewStore("/dev/null", cfg)
	disp := &fakeDispatcher{}
	e, err := NewEvaluator(slog.New(slog.NewTextHandler(io.Discard, nil)), store, disp)
	if err != nil {
		t.Fatalf("new evaluator: %v", err)
	}

	ref := cfg.AllTargets()[0]
	e.OnCycle(context.Background(), scheduler.Cycle{
		Target:    ref,
		ProbeName: "icmp",
		Source:    "master",
		Sent:      10,
		LossCount: 10,
		Hops:      []probe.Hop{{Index: 1, IP: "203.0.113.1"}},
	})

	events := disp.snapshot()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(events), events)
	}
	ev := events[0]
	if ev.Target.Target.Host != "192.0.2.1" || ev.Cycle.Target.Target.Host != "192.0.2.1" {
		t.Errorf("ordinary target lost its host: %q / %q",
			ev.Target.Target.Host, ev.Cycle.Target.Target.Host)
	}
	if ev.Target.Target.Family != "v4" {
		t.Errorf("ordinary target lost its family: %q", ev.Target.Target.Family)
	}
	if len(ev.Cycle.Hops) != 1 {
		t.Errorf("ordinary target lost its hops: %+v", ev.Cycle.Hops)
	}
}

// Under quorum the dispatched Event represents every source that saw the
// outage, not just whichever one's cycle happened to cross the threshold.
// Cycle.Source alone names one of the two here.
func TestQuorumEventNamesAllFiringSources(t *testing.T) {
	ev, disp := newTestEvaluator(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"},
		Quorum: config.Quorum{Majority: true},
	})

	ctx := context.Background()
	ev.OnCycle(ctx, healthyCycle("master"))
	ev.OnCycle(ctx, lossyCycle("tokyo-1", 100))
	ev.OnCycle(ctx, lossyCycle("frankfurt-1", 100))

	evs := disp.events()
	if len(evs) != 1 || evs[0].Next != StateFiring {
		t.Fatalf("setup: got %+v, want a single FIRING event", evs)
	}
	want := []string{"frankfurt-1", "tokyo-1"} // sorted, healthy master excluded
	if !slices.Equal(evs[0].FiringSources, want) {
		t.Fatalf("got firing sources %q, want %q", evs[0].FiringSources, want)
	}
}

// The list is the firing set at dispatch time, which on a quorum resolve is
// whoever is *still* seeing loss — the aggregate drops below threshold as
// soon as one source recovers, and an operator wants to know the outage
// hasn't fully cleared. Echoing the recovering source instead (Cycle.Source)
// would say the opposite.
func TestQuorumResolveNamesStillFiringSources(t *testing.T) {
	ev, disp := newTestEvaluator(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"},
		Quorum: config.Quorum{Majority: true},
	})

	ctx := context.Background()
	ev.OnCycle(ctx, lossyCycle("master", 100))
	ev.OnCycle(ctx, lossyCycle("tokyo-1", 100))
	disp.reset()
	ev.OnCycle(ctx, healthyCycle("master")) // 1 of 2 firing → aggregate resolves
	ev.OnCycle(ctx, healthyCycle("tokyo-1"))

	evs := disp.events()
	if len(evs) != 1 || evs[0].Next != StateOK {
		t.Fatalf("setup: got %+v, want a single resolve", evs)
	}
	if evs[0].Cycle.Source != "master" {
		t.Fatalf("setup: resolve was driven by %q, want master", evs[0].Cycle.Source)
	}
	if want := []string{"tokyo-1"}; !slices.Equal(evs[0].FiringSources, want) {
		t.Fatalf("got firing sources %q, want %q", evs[0].FiringSources, want)
	}
}

// A source that went silent while firing must drop off the list. The
// non-quorum path is the one at risk: it never prunes, so the filter has to
// do the work.
func TestFiringSourcesExcludesStaleSource(t *testing.T) {
	ev, disp := newTestEvaluator(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"},
	})

	ctx := context.Background()
	base := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)

	c := lossyCycle("tokyo-1", 100)
	c.Time = base
	ev.OnCycle(ctx, c)
	if evs := disp.events(); len(evs) != 1 || !slices.Equal(evs[0].FiringSources, []string{"tokyo-1"}) {
		t.Fatalf("setup: got %+v, want tokyo-1 firing", evs)
	}
	disp.reset()

	// Well past the staleness window (3x interval) — tokyo-1 has said nothing
	// since, so it can no longer be reported as currently firing.
	c2 := lossyCycle("master", 100)
	c2.Time = base.Add(time.Hour)
	ev.OnCycle(ctx, c2)

	evs := disp.events()
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	if want := []string{"master"}; !slices.Equal(evs[0].FiringSources, want) {
		t.Fatalf("got firing sources %q, want %q", evs[0].FiringSources, want)
	}
}

// Collecting the firing set on the non-quorum path must not prune per-source
// state the way tally does: a source that goes quiet while firing and comes
// back healthy still owes an operator its resolve. Pruning would reset it to
// StateOK, so prev == next and the resolve would never dispatch.
func TestNonQuorumResolveSurvivesSilentGap(t *testing.T) {
	ev, disp := newTestEvaluator(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"},
	})

	ctx := context.Background()
	base := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)

	c := lossyCycle("tokyo-1", 100)
	c.Time = base
	ev.OnCycle(ctx, c)
	if evs := disp.events(); len(evs) != 1 || evs[0].Next != StateFiring {
		t.Fatalf("setup: got %+v, want tokyo-1 FIRING", evs)
	}
	disp.reset()

	// A peer dispatches its own transition while tokyo-1 is silent past the
	// staleness window. The firing set is collected on exactly this cycle, so
	// a collector that pruned like tally would evict tokyo-1's state here.
	peer := lossyCycle("master", 100)
	peer.Time = base.Add(time.Hour)
	ev.OnCycle(ctx, peer)
	if evs := disp.events(); len(evs) != 1 || evs[0].Cycle.Source != "master" {
		t.Fatalf("setup: got %+v, want master's own FIRING event", evs)
	}
	disp.reset()

	c2 := healthyCycle("tokyo-1")
	c2.Time = base.Add(time.Hour + time.Minute)
	ev.OnCycle(ctx, c2)

	evs := disp.events()
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1 resolve after the gap: %+v", len(evs), evs)
	}
	if evs[0].Prev != StateFiring || evs[0].Next != StateOK {
		t.Fatalf("got %s → %s, want firing → ok", evs[0].Prev, evs[0].Next)
	}
	if want := []string{"master"}; !slices.Equal(evs[0].FiringSources, want) {
		t.Fatalf("got firing sources %q, want %q (master is still lossy)", evs[0].FiringSources, want)
	}
	disp.reset()

	// Once the last firing source recovers the set is empty, not a stale echo.
	c3 := healthyCycle("master")
	c3.Time = base.Add(time.Hour + 2*time.Minute)
	ev.OnCycle(ctx, c3)

	evs = disp.events()
	if len(evs) != 1 || evs[0].Next != StateOK {
		t.Fatalf("got %+v, want master's resolve", evs)
	}
	if len(evs[0].FiringSources) != 0 {
		t.Fatalf("got firing sources %q, want none once every source recovered", evs[0].FiringSources)
	}
}

// A standalone node stamps no source name; listing "" in a payload is noise.
// It must still be counted for quorum, which tally handles separately.
func TestFiringSourcesOmitsUnnamedSource(t *testing.T) {
	ev, disp := newTestEvaluator(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"},
	})

	ev.OnCycle(context.Background(), lossyCycle("", 100))

	evs := disp.events()
	if len(evs) != 1 || evs[0].Next != StateFiring {
		t.Fatalf("setup: got %+v, want one FIRING event", evs)
	}
	if len(evs[0].FiringSources) != 0 {
		t.Fatalf("got firing sources %q, want none for an unnamed source", evs[0].FiringSources)
	}
}

func TestSourcesField(t *testing.T) {
	cases := []struct {
		name      string
		ev        Event
		wantName  string
		wantValue string
	}{
		{
			name:      "multiple sources are all named",
			ev:        Event{FiringSources: []string{"frankfurt-1", "tokyo-1"}},
			wantName:  "Sources (2)",
			wantValue: "frankfurt-1, tokyo-1",
		},
		{
			name:      "single source keeps the singular label",
			ev:        Event{FiringSources: []string{"tokyo-1"}},
			wantName:  "Source",
			wantValue: "tokyo-1",
		},
		{
			name:      "resolve falls back to the triggering cycle",
			ev:        Event{Cycle: scheduler.Cycle{Source: "master"}},
			wantName:  "Source",
			wantValue: "master",
		},
		{
			name:      "standalone node yields no field",
			ev:        Event{},
			wantName:  "Source",
			wantValue: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, value := sourcesField(tc.ev)
			if name != tc.wantName || value != tc.wantValue {
				t.Fatalf("got (%q, %q), want (%q, %q)", name, value, tc.wantName, tc.wantValue)
			}
		})
	}
}

// Discord rejects the whole embed with a 400 if any field value exceeds 1024
// chars, so a large mesh must truncate rather than lose the alert entirely.
func TestSourcesFieldTruncatesLongList(t *testing.T) {
	var srcs []string
	for i := range 200 {
		srcs = append(srcs, fmt.Sprintf("slave-%03d", i))
	}
	name, value := sourcesField(Event{FiringSources: srcs})

	if want := "Sources (200)"; name != want {
		t.Fatalf("got name %q, want %q — the count must stay the true total", name, want)
	}
	if len(value) > 1024 {
		t.Fatalf("value is %d chars, want <= 1024", len(value))
	}
	if !strings.Contains(value, "more") {
		t.Fatalf("truncated value must say how many were dropped: %q", value)
	}
	if !strings.HasPrefix(value, "slave-000, slave-001") {
		t.Fatalf("truncation dropped the head of the list: %q", value)
	}
}

// The webhook payload carries the whole firing set, and always as an array so
// a consumer can range over it without a null check.
func TestWebhookPayloadCarriesFiringSources(t *testing.T) {
	var mu sync.Mutex
	var got []map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		mu.Lock()
		got = append(got, body)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &config.Config{
		Interval: time.Minute,
		Pings:    5,
		Storage:  config.Storage{ClickHouse: config.ClickHouse{Addr: "ch:9000"}},
		Probes:   map[string]config.Probe{"icmp": {Type: "icmp", Timeout: time.Second}},
		Alerts:   map[string]config.Alert{"down": {Condition: "loss_pct > 0", Sustained: 1, Actions: []string{"hook"}}},
		Actions:  map[string]config.Action{"hook": {Type: "webhook", URL: srv.URL}},
		Targets: []config.Group{{
			Group: "g", Targets: []config.Target{{Name: "a", Host: "1.1.1.1", Probe: "icmp"}},
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	store := config.NewStore("/dev/null", cfg)
	d := NewDispatcher(slog.New(slog.DiscardHandler), store)
	d.client = srv.Client()

	ref := cfg.AllTargets()[0]
	d.Dispatch(context.Background(), Event{
		Time:          time.Unix(1_700_000_000, 0),
		Target:        ref,
		AlertName:     "down",
		Alert:         cfg.Alerts["down"],
		Prev:          StatePending,
		Next:          StateFiring,
		Cycle:         scheduler.Cycle{Target: ref, Source: "tokyo-1", Sent: 5, LossCount: 5},
		Firing:        2,
		Live:          3,
		FiringSources: []string{"frankfurt-1", "tokyo-1"},
	})

	snapshot := func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), got...)
	}

	calls := snapshot()
	if len(calls) != 1 {
		t.Fatalf("got %d webhook calls, want 1", len(calls))
	}
	if src, _ := calls[0]["source"].(string); src != "tokyo-1" {
		t.Errorf("got source %q, want tokyo-1 (unchanged for existing consumers)", src)
	}
	sources, ok := calls[0]["sources"].([]any)
	if !ok {
		t.Fatalf("sources is %T, want an array: %v", calls[0]["sources"], calls[0])
	}
	if len(sources) != 2 || sources[0] != "frankfurt-1" || sources[1] != "tokyo-1" {
		t.Fatalf("got sources %v, want [frankfurt-1 tokyo-1]", sources)
	}

	// A resolve has no firing sources — the key must still be an empty array.
	d.Dispatch(context.Background(), Event{
		Time: time.Unix(1_700_000_060, 0), Target: ref, AlertName: "down",
		Alert: cfg.Alerts["down"], Prev: StateFiring, Next: StateOK,
		Cycle: scheduler.Cycle{Target: ref, Source: "tokyo-1"},
	})
	calls = snapshot()
	if len(calls) != 2 {
		t.Fatalf("got %d webhook calls, want 2", len(calls))
	}
	if sources, ok := calls[1]["sources"].([]any); !ok || len(sources) != 0 {
		t.Fatalf("got sources %#v on resolve, want an empty array", calls[1]["sources"])
	}
}

// The exec action gets the whole firing set too — a hook script that only saw
// ALERT_SOURCE would page on one slave's name during a fleet-wide outage.
func TestDispatcherExecPassesFiringSources(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "env")
	script := filepath.Join(dir, "hook.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s|%s\\n' \"$ALERT_SOURCE\" \"$ALERT_SOURCES\" > \"$1\"\n"), 0o700); err != nil {
		t.Fatalf("write script: %v", err)
	}

	cfg := &config.Config{
		Interval: time.Minute,
		Pings:    5,
		Storage:  config.Storage{ClickHouse: config.ClickHouse{Addr: "ch:9000"}},
		Probes:   map[string]config.Probe{"icmp": {Type: "icmp", Timeout: time.Second}},
		Alerts:   map[string]config.Alert{"down": {Condition: "loss_pct > 0", Sustained: 1, Actions: []string{"run"}}},
		Actions:  map[string]config.Action{"run": {Type: "exec", Command: script + " " + out}},
		Targets: []config.Group{{
			Group: "g", Targets: []config.Target{{Name: "a", Host: "1.1.1.1", Probe: "icmp"}},
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	store := config.NewStore("/dev/null", cfg)
	d := NewDispatcher(slog.New(slog.DiscardHandler), store)

	ref := cfg.AllTargets()[0]
	d.Dispatch(context.Background(), Event{
		Time: time.Unix(1_700_000_000, 0), Target: ref, AlertName: "down",
		Alert: cfg.Alerts["down"], Prev: StatePending, Next: StateFiring,
		Cycle:         scheduler.Cycle{Target: ref, Source: "tokyo-1", Sent: 5, LossCount: 5},
		FiringSources: []string{"frankfurt-1", "tokyo-1"},
	})

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("hook did not run: %v", err)
	}
	if want := "tokyo-1|frankfurt-1,tokyo-1\n"; string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
