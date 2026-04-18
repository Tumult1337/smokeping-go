package alert

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/probe"
	"github.com/tumult/gosmokeping/internal/scheduler"
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
		"loss_pct > 5":       {"loss_pct", OpGT, 5},
		"rtt_median > 50ms":  {"rtt_median", OpGT, 50},
		"rtt_p95 >= 100":     {"rtt_p95", OpGE, 100},
		"loss_pct != 0":      {"loss_pct", OpNE, 0},
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
		InfluxDB: config.InfluxDB{URL: "http://x", BucketRaw: "raw"},
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
		InfluxDB: config.InfluxDB{URL: "http://x", BucketRaw: "raw"},
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
