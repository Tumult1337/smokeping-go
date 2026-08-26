package alert

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/cluster"
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
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
	clk := pinClock(e, testBase)

	ref := cfg.AllTargets()[0]
	// Each call advances the clock and stamps the cycle with it: one
	// (target, source) stream never emits two cycles at one instant.
	at := func(median time.Duration) scheduler.Cycle {
		clk.advance(time.Minute)
		return scheduler.Cycle{
			Time:      clk.t,
			Target:    ref,
			ProbeName: "icmp",
			Sent:      10,
			Summary:   stats.Summary{Median: median},
		}
	}

	ctx := context.Background()
	e.OnCycle(ctx, at(100*time.Millisecond)) // OK → PENDING
	e.OnCycle(ctx, at(100*time.Millisecond)) // PENDING → FIRING (sustained=2)
	e.OnCycle(ctx, at(100*time.Millisecond)) // FIRING → FIRING (no event)
	e.OnCycle(ctx, at(10*time.Millisecond))  // FIRING → OK

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
	clk := pinClock(e, testBase)

	ref := cfg.AllTargets()[0]
	high := func(src string) scheduler.Cycle {
		clk.advance(time.Minute)
		return scheduler.Cycle{
			Time:      clk.t,
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
				{Index: 3, IP: "1.1.1.1", Sent: 5, Lost: 5, Unreach: "host-unreachable"},
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
	if !strings.Contains(desc, "!host-unreachable") {
		t.Errorf("description missing the hop unreachable annotation:\n%s", desc)
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

func TestDispatcherHTTPFailureLogsSanitizeCredentialURLs(t *testing.T) {
	const secret = "canary-webhook-secret"
	requestURL := "://alerts.example/" + secret
	malformedPortURL := "https://alerts.example:" + secret + "/hook"
	deliveryURL := "https://alerts.example/hooks/" + secret

	failDelivery := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network unreachable")
	})
	non2xx := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	})
	redirectFailure := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": []string{malformedPortURL}},
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	})

	cases := []struct {
		name       string
		actionType string
		url        string
		transport  roundTripFunc
		sanitized  string
		want       string
	}{
		{
			name:       "webhook request",
			actionType: "webhook",
			url:        requestURL,
			sanitized:  "request failed",
			want:       "level=WARN msg=\"webhook request\" err=\"request failed\"\n",
		},
		{
			name:       "webhook malformed port",
			actionType: "webhook",
			url:        malformedPortURL,
			sanitized:  "request failed",
			want:       "level=WARN msg=\"webhook request\" err=\"request failed\"\n",
		},
		{
			name:       "webhook delivery",
			actionType: "webhook",
			url:        deliveryURL,
			transport:  failDelivery,
			sanitized:  "request failed",
			want:       "level=WARN msg=\"webhook deliver\" err=\"request failed\"\n",
		},
		{
			name:       "webhook redirect",
			actionType: "webhook",
			url:        "https://alerts.example/hooks/original",
			transport:  redirectFailure,
			sanitized:  "request failed",
			want:       "level=WARN msg=\"webhook deliver\" err=\"request failed\"\n",
		},
		{
			name:       "webhook non-2xx",
			actionType: "webhook",
			url:        deliveryURL,
			transport:  non2xx,
			sanitized:  "status=500",
			want:       "level=WARN msg=\"webhook non-2xx\" status=500\n",
		},
		{
			name:       "discord request",
			actionType: "discord",
			url:        requestURL,
			sanitized:  "request failed",
			want:       "level=WARN msg=\"discord request\" err=\"request failed\"\n",
		},
		{
			name:       "discord malformed port",
			actionType: "discord",
			url:        malformedPortURL,
			sanitized:  "request failed",
			want:       "level=WARN msg=\"discord request\" err=\"request failed\"\n",
		},
		{
			name:       "discord delivery",
			actionType: "discord",
			url:        deliveryURL,
			transport:  failDelivery,
			sanitized:  "request failed",
			want:       "level=WARN msg=\"discord deliver\" err=\"request failed\"\n",
		},
		{
			name:       "discord redirect",
			actionType: "discord",
			url:        "https://alerts.example/hooks/original",
			transport:  redirectFailure,
			sanitized:  "request failed",
			want:       "level=WARN msg=\"discord deliver\" err=\"request failed\"\n",
		},
		{
			name:       "discord non-2xx",
			actionType: "discord",
			url:        deliveryURL,
			transport:  non2xx,
			sanitized:  "status=500",
			want:       "level=WARN msg=\"discord non-2xx\" status=500\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{
				ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
					if attr.Key == slog.TimeKey {
						return slog.Attr{}
					}
					return attr
				},
			}))
			dispatcher := &ActionDispatcher{
				log:    logger,
				client: &http.Client{Transport: tc.transport},
			}
			action := config.Action{Type: tc.actionType, URL: tc.url}
			event := Event{Time: time.Unix(1_700_000_000, 0)}
			switch tc.actionType {
			case "webhook":
				dispatcher.webhook(context.Background(), action, "test", event)
			case "discord":
				dispatcher.discord(context.Background(), action, "test", event)
			}

			got := logs.String()
			if !strings.Contains(got, tc.sanitized) {
				t.Fatalf("log output = %q, want sanitized value %q", got, tc.sanitized)
			}
			if strings.Contains(got, secret) {
				t.Fatalf("log output contains credential canary %q: %q", secret, got)
			}
			if got != tc.want {
				t.Fatalf("log output = %q, want %q", got, tc.want)
			}
		})
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
	ev, disp, clk := newTestEvaluatorClock(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"},
		Quorum: config.Quorum{Majority: true},
	})

	ctx := context.Background()
	ev.OnCycle(ctx, cycleAt(clk, "master", 100))
	ev.OnCycle(ctx, cycleAt(clk, "tokyo-1", 100))
	disp.reset()

	clk.advance(time.Minute)
	ev.OnCycle(ctx, cycleAt(clk, "master", 0))
	ev.OnCycle(ctx, cycleAt(clk, "tokyo-1", 0))

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
	ev, disp, clk := newTestEvaluatorClock(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"},
		Quorum: config.Quorum{Majority: true},
	})

	ctx := context.Background()

	// Three healthy sources, then two go silent.
	for _, src := range []string{"master", "tokyo-1", "frankfurt-1"} {
		ev.OnCycle(ctx, cycleAt(clk, src, 0))
	}
	disp.reset()

	// Well past the staleness window (3x interval), only master reports.
	clk.advance(time.Hour)
	ev.OnCycle(ctx, cycleAt(clk, "master", 100))

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
	ev, disp, clk := newTestEvaluatorClock(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"},
		Quorum: config.Quorum{Majority: true},
	})

	ctx := context.Background()

	ev.OnCycle(ctx, cycleAt(clk, "master", 100))
	if got := len(disp.events()); got != 0 {
		t.Fatalf("got %d events, want 0 (still inside the warm-up window)", got)
	}

	clk.advance(4 * time.Minute) // > 3x interval(1m) warm-up window
	ev.OnCycle(ctx, cycleAt(clk, "master", 100))

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
	clk := pinClock(ev, testBase)

	ctx := context.Background()

	// Quorum on: both sources go lossy, majority-of-2 fires.
	ev.OnCycle(ctx, cycleAt(clk, "master", 100))
	ev.OnCycle(ctx, cycleAt(clk, "tokyo-1", 100))
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
	clk.advance(time.Minute)
	ev.OnCycle(ctx, cycleAt(clk, "master", 0))
	ev.OnCycle(ctx, cycleAt(clk, "tokyo-1", 0))
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

// fakeClock is the master's receive clock under test. Alert liveness must run
// off it and never off a cycle's own timestamp, which a slave chooses.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// testBase is both the fake clock's start and testCycle's timestamp, so a
// cycle built by the helpers is exactly as fresh as the clock says.
var testBase = time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)

// cycleAt stamps a cycle at the current receive time, which is what an honest
// slave with a synchronised clock produces.
func cycleAt(clk *fakeClock, source string, lossPct float64) scheduler.Cycle {
	c := testCycle(source, lossPct)
	c.Time = clk.t
	return c
}

// pinClock installs a receive clock at a fixed instant. Every test that drives
// OnCycle needs one: cycles carry fixed fixture timestamps, and alerting skips
// a cycle older than alertFreshness relative to the receive clock.
func pinClock(e *Evaluator, at time.Time) *fakeClock {
	clk := &fakeClock{t: at}
	e.nowFn = clk.now
	return clk
}

func newTestEvaluator(t *testing.T, a config.Alert) (*Evaluator, *recordingDispatcher) {
	ev, disp, _ := newTestEvaluatorClock(t, a)
	return ev, disp
}

func newTestEvaluatorClock(t *testing.T, a config.Alert) (*Evaluator, *recordingDispatcher, *fakeClock) {
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
	return ev, disp, pinClock(ev, testBase)
}

// testCycle populates LossCount/Sent, the fields fieldGetter("loss_pct")
// actually reads (see condition.go) — not a "loss" field, which doesn't
// exist as a condition field.
func testCycle(source string, lossPct float64) scheduler.Cycle {
	return scheduler.Cycle{
		Time:   testBase,
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
	pinClock(e, time.Time{})

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
			{Index: 2, IP: slaveAddr, Unreach: "no-route", TargetReply: true},
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
	pinClock(e, time.Time{})

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
	ev, disp, clk := newTestEvaluatorClock(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"},
		Quorum: config.Quorum{Majority: true},
	})

	ctx := context.Background()
	ev.OnCycle(ctx, cycleAt(clk, "master", 100))
	ev.OnCycle(ctx, cycleAt(clk, "tokyo-1", 100))
	disp.reset()
	clk.advance(time.Minute)
	ev.OnCycle(ctx, cycleAt(clk, "master", 0)) // 1 of 2 firing → aggregate resolves
	ev.OnCycle(ctx, cycleAt(clk, "tokyo-1", 0))

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
	ev, disp, clk := newTestEvaluatorClock(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"},
	})

	ctx := context.Background()

	ev.OnCycle(ctx, cycleAt(clk, "tokyo-1", 100))
	if evs := disp.events(); len(evs) != 1 || !slices.Equal(evs[0].FiringSources, []string{"tokyo-1"}) {
		t.Fatalf("setup: got %+v, want tokyo-1 firing", evs)
	}
	disp.reset()

	// Well past the staleness window (3x interval) — tokyo-1 has said nothing
	// since, so it can no longer be reported as currently firing.
	clk.advance(time.Hour)
	ev.OnCycle(ctx, cycleAt(clk, "master", 100))

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
	ev, disp, clk := newTestEvaluatorClock(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"},
	})

	ctx := context.Background()

	ev.OnCycle(ctx, cycleAt(clk, "tokyo-1", 100))
	if evs := disp.events(); len(evs) != 1 || evs[0].Next != StateFiring {
		t.Fatalf("setup: got %+v, want tokyo-1 FIRING", evs)
	}
	disp.reset()

	// A peer dispatches its own transition while tokyo-1 is silent past the
	// staleness window. The firing set is collected on exactly this cycle, so
	// a collector that pruned like tally would evict tokyo-1's state here.
	clk.advance(time.Hour)
	ev.OnCycle(ctx, cycleAt(clk, "master", 100))
	if evs := disp.events(); len(evs) != 1 || evs[0].Cycle.Source != "master" {
		t.Fatalf("setup: got %+v, want master's own FIRING event", evs)
	}
	disp.reset()

	clk.advance(time.Minute)
	ev.OnCycle(ctx, cycleAt(clk, "tokyo-1", 0))

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
	clk.advance(time.Minute)
	ev.OnCycle(ctx, cycleAt(clk, "master", 0))

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

// timeoutError is a net.Error whose Timeout reports true, the shape a stalled
// dial produces.
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return false }

// httpFailureCategory may only ever return one of its own constants: a Discord
// webhook URL is the credential, and url.Error quotes that URL at every nesting
// level, so any error-derived text in the result is a leak.
func TestHTTPFailureCategory(t *testing.T) {
	const canary = "canary-webhook-secret"
	secretURL := "https://alerts.example/hooks/" + canary
	wrap := func(err error) error { return &url.Error{Op: "Post", URL: secretURL, Err: err} }

	allowed := map[string]bool{
		"timeout": true, "canceled": true, "dns": true, "tls": true, safeHTTPError: true,
	}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"deadline exceeded", wrap(context.DeadlineExceeded), "timeout"},
		{"canceled", wrap(context.Canceled), "canceled"},
		{"dns failure names the host", wrap(&net.DNSError{Err: "no such host", Name: canary + ".example"}), "dns"},
		{"tls verification", wrap(&tls.CertificateVerificationError{}), "tls"},
		{"net timeout", wrap(timeoutError{}), "timeout"},
		{"unclassified quotes the url", wrap(errors.New("connection refused to " + secretURL)), safeHTTPError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := httpFailureCategory(tc.err)
			if got != tc.want {
				t.Fatalf("category = %q, want %q", got, tc.want)
			}
			if !allowed[got] {
				t.Fatalf("category %q is not one of the fixed constants", got)
			}
		})
	}
}

// transitions renders a dispatch sequence compactly; the raw Events embed a
// whole Cycle each and drown a failure message.
func transitions(events []Event) string {
	var parts []string
	for _, e := range events {
		parts = append(parts, fmt.Sprintf("%s>%s", e.Prev, e.Next))
	}
	return strings.Join(parts, " ")
}

// noMeasurementCfg is the shared fixture for the Sent == 0 tests: a
// sustained-loss alert, which is exactly what a fabricated 0% loss silences.
func noMeasurementCfg(t *testing.T, sustained int) (*config.Config, *config.Store) {
	t.Helper()
	cfg := &config.Config{
		Interval: time.Minute,
		Pings:    10,
		Storage:  config.Storage{ClickHouse: config.ClickHouse{Addr: "ch:9000"}},
		Probes:   map[string]config.Probe{"mtr": {Type: "mtr", Timeout: time.Second}},
		Alerts: map[string]config.Alert{
			"loss": {Condition: "loss_pct >= 50", Sustained: sustained, Actions: []string{"log"}},
		},
		Actions: map[string]config.Action{"log": {Type: "log"}},
		Targets: []config.Group{{
			Group:   "g",
			Targets: []config.Target{{Name: "a", Host: "1.1.1.1", Probe: "mtr", Alerts: []string{"loss"}}},
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("invalid config: %v", err)
	}
	return cfg, config.NewStore("/dev/null", cfg)
}

// A cycle that sent nothing measured nothing, so it must not clear the
// sustained counter a real loss cycle built up — every condition field reads
// zero on it, which is indistinguishable from a perfect cycle.
func TestNoMeasurementCycleDoesNotResetSustained(t *testing.T) {
	cfg, store := noMeasurementCfg(t, 2)
	disp := &fakeDispatcher{}
	e, err := NewEvaluator(slog.New(slog.NewTextHandler(io.Discard, nil)), store, disp)
	if err != nil {
		t.Fatalf("new evaluator: %v", err)
	}
	ref := cfg.AllTargets()[0]
	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	pinClock(e, base.Add(2*time.Minute))
	lost := func(at time.Time) scheduler.Cycle {
		return scheduler.Cycle{Time: at, Target: ref, ProbeName: "mtr", Sent: 10, LossCount: 10}
	}
	none := func(at time.Time) scheduler.Cycle {
		return scheduler.Cycle{Time: at, Target: ref, ProbeName: "mtr"}
	}

	ctx := context.Background()
	e.OnCycle(ctx, lost(base))                    // OK → PENDING
	e.OnCycle(ctx, none(base.Add(time.Minute)))   // no measurement: ignored
	e.OnCycle(ctx, lost(base.Add(2*time.Minute))) // PENDING → FIRING

	if got := transitions(disp.snapshot()); got != "ok>pending pending>firing" {
		t.Fatalf("transitions = %q, want %q", got, "ok>pending pending>firing")
	}
}

// A gap must not resolve a live alert either: the outage is still there, it
// just wasn't measured this cycle.
func TestNoMeasurementCycleDoesNotResolveFiring(t *testing.T) {
	cfg, store := noMeasurementCfg(t, 1)
	disp := &fakeDispatcher{}
	e, err := NewEvaluator(slog.New(slog.NewTextHandler(io.Discard, nil)), store, disp)
	if err != nil {
		t.Fatalf("new evaluator: %v", err)
	}
	ref := cfg.AllTargets()[0]
	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	pinClock(e, base.Add(time.Minute))
	ctx := context.Background()
	e.OnCycle(ctx, scheduler.Cycle{Time: base, Target: ref, ProbeName: "mtr", Sent: 10, LossCount: 10})
	e.OnCycle(ctx, scheduler.Cycle{Time: base.Add(time.Minute), Target: ref, ProbeName: "mtr"})

	if got := transitions(disp.snapshot()); got != "ok>firing" {
		t.Fatalf("transitions = %q, want %q", got, "ok>firing")
	}
}

// A cycle's timestamp is slave-supplied and ingest accepts one up to
// config.MaxFutureSkew ahead of the master. Reading liveness off it let one
// hostile slave date a cycle forward far enough to age every honest source
// out of tally and become a majority of one — manufacturing a page out of a
// healthy fleet. Liveness runs off the master's receive clock instead.
func TestQuorumLivenessIgnoresSlaveSuppliedTime(t *testing.T) {
	ev, disp, clk := newTestEvaluatorClock(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"},
		Quorum: config.Quorum{Majority: true},
	})
	ctx := context.Background()

	for _, src := range []string{"master", "tokyo-1", "frankfurt-1"} {
		ev.OnCycle(ctx, cycleAt(clk, src, 0))
	}
	disp.reset()

	hostile := lossyCycle("evil-1", 100)
	hostile.Time = clk.t.Add(config.MaxFutureSkew)
	ev.OnCycle(ctx, hostile)

	if evs := disp.events(); len(evs) != 0 {
		t.Fatalf("got %d events, want 0 — one source out of four is not a majority: %+v", len(evs), evs)
	}
}

// The same skew in the other direction is the more damaging one: silencing a
// real outage every honest source is reporting.
func TestQuorumSkewCannotResolveALiveAlert(t *testing.T) {
	ev, disp, clk := newTestEvaluatorClock(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"},
		Quorum: config.Quorum{Majority: true},
	})
	ctx := context.Background()

	for _, src := range []string{"master", "tokyo-1", "frankfurt-1"} {
		ev.OnCycle(ctx, cycleAt(clk, src, 100))
	}
	if evs := disp.events(); len(evs) != 1 || evs[0].Next != StateFiring {
		t.Fatalf("setup: got %+v, want one FIRING", evs)
	}
	disp.reset()

	hostile := healthyCycle("evil-1")
	hostile.Time = clk.t.Add(config.MaxFutureSkew)
	ev.OnCycle(ctx, hostile)

	if evs := disp.events(); len(evs) != 0 {
		t.Fatalf("got %d events, want 0 — a skewed healthy cycle must not resolve a live alert: %+v", len(evs), evs)
	}
}

// Ingest accepts a cycle up to config.MaxCycleAge (7d) old, so a slave can
// replay week-old cycles at will. Alerting is a statement about now: a cycle
// older than the liveness window can neither support nor refute the current
// state, and evaluating it replays historical transitions as if they were
// current. It is skipped whole, exactly like a cycle that sent nothing, so
// the source ages out rather than voting on stale data.
func TestStaleCycleIsNotEvaluated(t *testing.T) {
	ev, disp, clk := newTestEvaluatorClock(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"},
	})
	ctx := context.Background()
	window := alertFreshness(time.Minute)

	replay := lossyCycle("evil-1", 100)
	replay.Time = clk.t.Add(-window - time.Second)
	ev.OnCycle(ctx, replay)
	if evs := disp.events(); len(evs) != 0 {
		t.Fatalf("got %d events from a replayed cycle, want 0: %+v", len(evs), evs)
	}

	// The positive counterpart: one second inside the window still alerts, so
	// the test above is not passing because nothing alerts at all.
	fresh := lossyCycle("tokyo-1", 100)
	fresh.Time = clk.t.Add(-window + time.Second)
	ev.OnCycle(ctx, fresh)
	evs := disp.events()
	if len(evs) != 1 || evs[0].Next != StateFiring {
		t.Fatalf("got %+v, want one FIRING from a cycle inside the window", evs)
	}
}

// The freshness window must never be tighter than the liveness window it
// feeds, or a slow-interval deployment could never keep a source live, and
// never tighter than the skew ingest already tolerates, or an honest slave at
// the accepted limit is silently excluded from alerting.
func TestAlertFreshnessBounds(t *testing.T) {
	for _, interval := range []time.Duration{0, time.Second, 20 * time.Second, time.Minute, 10 * time.Minute} {
		got := alertFreshness(interval)
		if w := stalenessWindow(interval); got < w {
			t.Errorf("interval %s: freshness %s is tighter than the liveness window %s", interval, got, w)
		}
		if got < config.MaxFutureSkew {
			t.Errorf("interval %s: freshness %s is tighter than the accepted clock skew %s", interval, got, config.MaxFutureSkew)
		}
	}
}

// The mirror image of the skew attack, and the one that costs an operator a
// missed page: honest slaves whose clocks lag by the skew ingest tolerates
// must still count as live. Keying lastSeen on the cycle's own timestamp
// prunes a source on the very cycle that set it, so a whole fleet reporting an
// outage goes to zero live sources and nothing fires.
func TestLaggingSlaveClocksStillCountAsLive(t *testing.T) {
	ev, disp, clk := newTestEvaluatorClock(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"},
		Quorum: config.Quorum{Majority: true},
	})
	ctx := context.Background()

	for _, src := range []string{"master", "tokyo-1", "frankfurt-1"} {
		c := lossyCycle(src, 100)
		c.Time = clk.t.Add(-config.MaxFutureSkew) // inside alertFreshness, outside 3×interval
		ev.OnCycle(ctx, c)
	}

	evs := disp.events()
	if len(evs) != 1 || evs[0].Next != StateFiring {
		t.Fatalf("got %+v, want one FIRING — lagging clocks must not prune a live fleet", evs)
	}
	// Two, not three: the aggregate crosses the majority on the second
	// source's cycle and the third changes nothing.
	if evs[0].Live != 2 {
		t.Fatalf("got %d live sources, want 2", evs[0].Live)
	}
}

// Warm-up exists so a master restart cannot page on a majority-of-one. Its
// window must be measured on the receive clock too: keyed on the cycle's own
// timestamp, a single slave back-dating its first cycle by the window makes
// the aggregate "ready" on that very cycle and fires alone.
func TestQuorumWarmupCannotBeSkippedByBackdating(t *testing.T) {
	ev, disp, clk := newTestEvaluatorClock(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"},
		Quorum: config.Quorum{Majority: true},
	})
	ctx := context.Background()

	c := lossyCycle("evil-1", 100)
	c.Time = clk.t.Add(-stalenessWindow(time.Minute) - time.Second)
	ev.OnCycle(ctx, c)

	if evs := disp.events(); len(evs) != 0 {
		t.Fatalf("got %d events, want 0 — warm-up must not be satisfied by a back-dated first cycle: %+v", len(evs), evs)
	}
}

// A slave requeues on any 5xx or network error, so the master ingests the same
// measurement twice whenever an ack is lost. Applying it twice incremented
// consecHits twice and fired a sustained:2 alert off one bad cycle.
func TestDuplicateCycleCannotFireASustainedAlert(t *testing.T) {
	ev, disp, clk := newTestEvaluatorClock(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 2, Actions: []string{"log"},
	})
	ctx := context.Background()

	bad := cycleAt(clk, "tokyo-1", 100)
	ev.OnCycle(ctx, bad)
	ev.OnCycle(ctx, bad)
	evs := disp.events()
	if len(evs) != 1 || evs[0].Next != StatePending {
		t.Fatalf("got %+v, want only PENDING — one cycle delivered twice is one bad cycle", evs)
	}
	disp.reset()

	// The positive counterpart: a genuinely later cycle is the second hit and
	// does fire, so the assertion above is not passing because nothing fires.
	clk.advance(time.Minute)
	ev.OnCycle(ctx, cycleAt(clk, "tokyo-1", 100))
	evs = disp.events()
	if len(evs) != 1 || evs[0].Next != StateFiring {
		t.Fatalf("got %+v, want one FIRING from the second distinct cycle", evs)
	}
}

// The reverse ordering failure: a requeued healthy batch delivered after a
// newer lossy one rolled a live alert back to OK off replayed data.
func TestStaleReplayCannotResolveAFiringAlert(t *testing.T) {
	ev, disp, clk := newTestEvaluatorClock(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"},
	})
	ctx := context.Background()

	older := cycleAt(clk, "tokyo-1", 0)
	clk.advance(time.Minute)
	ev.OnCycle(ctx, cycleAt(clk, "tokyo-1", 100))
	if evs := disp.events(); len(evs) != 1 || evs[0].Next != StateFiring {
		t.Fatalf("got %+v, want one FIRING", evs)
	}
	disp.reset()

	ev.OnCycle(ctx, older)
	if evs := disp.events(); len(evs) != 0 {
		t.Fatalf("got %+v, want no dispatch — an older cycle must not resolve a newer state", evs)
	}

	// The positive counterpart: the next genuinely newer healthy cycle does
	// resolve it.
	clk.advance(time.Minute)
	ev.OnCycle(ctx, cycleAt(clk, "tokyo-1", 0))
	evs := disp.events()
	if len(evs) != 1 || evs[0].Next != StateOK {
		t.Fatalf("got %+v, want one RESOLVED from a newer healthy cycle", evs)
	}
}

// A duplicate must not refresh liveness either: a slave that has stopped
// probing but is still replaying its ring would otherwise sit in the quorum
// denominator forever and hold a real outage below the threshold.
func TestDuplicateCycleDoesNotRefreshLiveness(t *testing.T) {
	ev, disp, clk := newTestEvaluatorClock(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"},
		Quorum: config.Quorum{Majority: true},
	})
	ctx := context.Background()

	ev.OnCycle(ctx, cycleAt(clk, "tokyo-1", 100))
	healthy := cycleAt(clk, "frankfurt-1", 0)
	ev.OnCycle(ctx, healthy)
	if evs := disp.events(); len(evs) != 0 {
		t.Fatalf("got %+v, want no dispatch — one of two sources is not a majority", evs)
	}

	// frankfurt-1 goes silent past the liveness window and replays its last
	// cycle instead of producing a new one.
	clk.advance(stalenessWindow(time.Minute) + time.Second)
	ev.OnCycle(ctx, healthy)

	ev.OnCycle(ctx, cycleAt(clk, "tokyo-1", 100))
	evs := disp.events()
	if len(evs) != 1 || evs[0].Next != StateFiring {
		t.Fatalf("got %+v, want one FIRING — a replaying source must age out of the denominator", evs)
	}
	if evs[0].Live != 1 {
		t.Fatalf("got %d live sources, want 1", evs[0].Live)
	}
}

// logCapture records slog records so a test can assert on an operator-facing
// warning rather than on its absence.
type logCapture struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }
func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler       { return c }
func (c *logCapture) WithGroup(string) slog.Handler            { return c }

func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recs = append(c.recs, r.Clone())
	return nil
}

func (c *logCapture) at(level slog.Level, msg string) []slog.Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []slog.Record
	for _, r := range c.recs {
		if r.Level == level && r.Message == msg {
			out = append(out, r)
		}
	}
	return out
}

func attr(r slog.Record, key string) string {
	var out string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			out = a.Value.String()
			return false
		}
		return true
	})
	return out
}

func newCapturingEvaluator(t *testing.T, a config.Alert) (*Evaluator, *logCapture, *fakeClock) {
	t.Helper()
	cfg := &config.Config{
		Interval: time.Minute,
		Pings:    20,
		Alerts:   map[string]config.Alert{"quorum-test": a},
		Actions:  map[string]config.Action{"log": {Type: "log"}},
	}
	cap := &logCapture{}
	ev, err := NewEvaluator(slog.New(cap), config.NewStore("", cfg), &recordingDispatcher{})
	if err != nil {
		t.Fatalf("NewEvaluator: %v", err)
	}
	return ev, cap, pinClock(ev, testBase)
}

// A slave whose clock lags stably by more than alertFreshness passes the 7-day
// ingest window and has every cycle refused by alerting: non-quorum alerts
// from it vanish and quorum drops it from the denominator. Silent exclusion is
// the failure alerting exists to prevent, so it must not be a Debug line.
func TestExcludedSourceIsWarnedAbout(t *testing.T) {
	ev, cap, clk := newCapturingEvaluator(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"},
	})
	ctx := context.Background()
	window := alertFreshness(time.Minute)

	lagging := lossyCycle("tokyo-1", 100)
	lagging.Time = clk.t.Add(-window - time.Second)
	ev.OnCycle(ctx, lagging)

	recs := cap.at(slog.LevelWarn, "alert.source_excluded")
	if len(recs) != 1 {
		t.Fatalf("got %d warnings, want 1", len(recs))
	}
	if got := attr(recs[0], "source"); got != "tokyo-1" {
		t.Errorf("warning names source %q, want tokyo-1", got)
	}
	if got := attr(recs[0], "reason"); got != reasonClockSkew {
		t.Errorf("warning reason %q, want %q", got, reasonClockSkew)
	}

	// A source that is being evaluated must not be warned about, or the
	// warning means nothing.
	ev.OnCycle(ctx, cycleAt(clk, "frankfurt-1", 100))
	for _, r := range cap.at(slog.LevelWarn, "alert.source_excluded") {
		if attr(r, "source") == "frankfurt-1" {
			t.Fatal("an evaluated source was reported as excluded")
		}
	}
}

// A stably skewed source skips one cycle per target per interval. The warning
// has to survive that without flooding, and must still say how much it hid.
func TestExcludedSourceWarningIsRateLimitedPerWindow(t *testing.T) {
	ev, cap, clk := newCapturingEvaluator(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"},
	})
	ctx := context.Background()
	window := alertFreshness(time.Minute)

	for range 40 {
		lagging := lossyCycle("tokyo-1", 100)
		lagging.Time = clk.t.Add(-window - time.Second)
		ev.OnCycle(ctx, lagging)
		clk.advance(time.Second)
	}
	if recs := cap.at(slog.LevelWarn, "alert.source_excluded"); len(recs) != 1 {
		t.Fatalf("got %d warnings inside one window, want 1", len(recs))
	}

	clk.advance(window)
	lagging := lossyCycle("tokyo-1", 100)
	lagging.Time = clk.t.Add(-window - time.Second)
	ev.OnCycle(ctx, lagging)

	recs := cap.at(slog.LevelWarn, "alert.source_excluded")
	if len(recs) != 2 {
		t.Fatalf("got %d warnings across two windows, want 2", len(recs))
	}
	if got := attr(recs[1], "suppressed"); got != "39" {
		t.Errorf("second warning reports %q suppressed, want 39", got)
	}
}

// The duplicate guard is the other silent exclusion: a producer whose clock
// steps backwards delivers cycles that are never evaluated, and nothing else
// would say so.
func TestDuplicateCycleIsWarnedAbout(t *testing.T) {
	ev, cap, clk := newCapturingEvaluator(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"},
	})
	ctx := context.Background()

	ev.OnCycle(ctx, cycleAt(clk, "tokyo-1", 0))
	clk.advance(time.Second)
	ev.OnCycle(ctx, cycleAt(clk, "tokyo-1", 0))
	if recs := cap.at(slog.LevelWarn, "alert.source_excluded"); len(recs) != 0 {
		t.Fatalf("got %d warnings for an advancing stream, want 0", len(recs))
	}

	stepped := cycleAt(clk, "tokyo-1", 0)
	stepped.Time = stepped.Time.Add(-time.Second)
	ev.OnCycle(ctx, stepped)

	recs := cap.at(slog.LevelWarn, "alert.source_excluded")
	if len(recs) != 1 {
		t.Fatalf("got %d warnings, want 1", len(recs))
	}
	if got := attr(recs[0], "reason"); got != reasonDuplicate {
		t.Errorf("warning reason %q, want %q", got, reasonDuplicate)
	}
}

// The rate-limit map is keyed by source, and slaves churn across a long-lived
// master. The freshness window doubles as the eviction rule, so a source that
// stopped being excluded must not hold a slot forever.
func TestExclusionRecordsAreEvicted(t *testing.T) {
	ev, _, clk := newCapturingEvaluator(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"},
	})
	ctx := context.Background()
	window := alertFreshness(time.Minute)

	for _, src := range []string{"edge-1", "edge-2", "edge-3"} {
		lagging := lossyCycle(src, 100)
		lagging.Time = clk.t.Add(-window - time.Second)
		ev.OnCycle(ctx, lagging)
	}
	if got := len(ev.excluded); got != 3 {
		t.Fatalf("got %d recorded exclusions, want 3", got)
	}

	// Those three are gone from the fleet; a fourth arrives a window later.
	clk.advance(window)
	lagging := lossyCycle("edge-4", 100)
	lagging.Time = clk.t.Add(-window - time.Second)
	ev.OnCycle(ctx, lagging)
	if got := len(ev.excluded); got != 1 {
		t.Fatalf("got %d recorded exclusions after the window, want 1", got)
	}
}

// OnCycle runs once per target goroutine plus the cluster ingest handler, so
// the exclusion bookkeeping is reached concurrently. It takes its own lock,
// never nested inside the state lock.
func TestConcurrentOnCycleIsRaceFree(t *testing.T) {
	ev, cap, clk := newCapturingEvaluator(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"},
	})
	ctx := context.Background()
	window := alertFreshness(time.Minute)

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 25 {
				c := lossyCycle(fmt.Sprintf("edge-%d", i), 100)
				if j%2 == 0 {
					c.Time = clk.t.Add(-window - time.Second)
				} else {
					c.Time = clk.t.Add(-time.Duration(j) * time.Millisecond)
				}
				ev.OnCycle(ctx, c)
			}
		}()
	}
	wg.Wait()
	if len(cap.at(slog.LevelWarn, "alert.source_excluded")) == 0 {
		t.Fatal("no exclusion was reported, so the concurrent path was never reached")
	}
}

// "No cycle accepted yet" must not be spelled as "the last one was at the zero
// time", or a producer that stamps nothing disables the guard for its source
// permanently — the zero value has to deny, not admit.
func TestGuardHoldsForAZeroStampedCycle(t *testing.T) {
	ev, disp, _ := newTestEvaluatorClock(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 2, Actions: []string{"log"},
	})
	pinClock(ev, time.Time{})
	ctx := context.Background()

	bad := lossyCycle("tokyo-1", 100)
	bad.Time = time.Time{}
	ev.OnCycle(ctx, bad)
	ev.OnCycle(ctx, bad)

	evs := disp.events()
	if len(evs) != 1 || evs[0].Next != StatePending {
		t.Fatalf("got %+v, want only PENDING — the second zero-stamped cycle is the same cycle", evs)
	}
}

// The replay floor is a slave-supplied timestamp, and ingest accepts one up to
// config.MaxFutureSkew ahead. A token holder can post under any registered
// peer's name, so a single forward-dated cycle could park that source's floor
// in the future and silence every genuine cycle behind it — the same class the
// lastSeen clock fix closed, reopened through the floor.
func TestFutureDatedCycleCannotMuteASource(t *testing.T) {
	ev, disp, clk := newTestEvaluatorClock(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"},
	})
	ctx := context.Background()

	poison := healthyCycle("tokyo-1")
	poison.Time = clk.t.Add(config.MaxFutureSkew)
	ev.OnCycle(ctx, poison)
	disp.reset()

	clk.advance(time.Second)
	ev.OnCycle(ctx, cycleAt(clk, "tokyo-1", 100))

	evs := disp.events()
	if len(evs) != 1 || evs[0].Next != StateFiring {
		t.Fatalf("got %+v, want one FIRING — a forward-dated cycle must not silence the source", evs)
	}
}

// ...and the mute must not be reachable by degrees either: a quorum's
// denominator is what a mute really attacks, so a poisoned source must still
// be counted live off its own genuine cycles.
func TestFutureDatedCycleCannotEmptyTheQuorumDenominator(t *testing.T) {
	ev, disp, clk := newTestEvaluatorClock(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"},
		Quorum: config.Quorum{Majority: true},
	})
	ctx := context.Background()

	for _, src := range []string{"tokyo-1", "frankfurt-1"} {
		poison := healthyCycle(src)
		poison.Time = clk.t.Add(config.MaxFutureSkew)
		ev.OnCycle(ctx, poison)
	}
	disp.reset()

	clk.advance(time.Second)
	ev.OnCycle(ctx, cycleAt(clk, "tokyo-1", 100))
	ev.OnCycle(ctx, cycleAt(clk, "frankfurt-1", 100))

	evs := disp.events()
	if len(evs) != 1 || evs[0].Next != StateFiring {
		t.Fatalf("got %+v, want one FIRING", evs)
	}
	if evs[0].Live != 2 {
		t.Fatalf("got %d live sources, want 2 — a poisoned floor emptied the denominator", evs[0].Live)
	}
}

// The common honest case: a slave's clock is a little ahead of the master's.
// A redelivery of one of its cycles is still one measurement, so any repair to
// the future-dating hole above must not stop recognising it.
func TestDuplicateIsCaughtWhenTheSlaveClockRunsAhead(t *testing.T) {
	ev, disp, clk := newTestEvaluatorClock(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 2, Actions: []string{"log"},
	})
	ctx := context.Background()

	bad := lossyCycle("tokyo-1", 100)
	bad.Time = clk.t.Add(time.Millisecond) // slave 1ms ahead of the master
	ev.OnCycle(ctx, bad)
	clk.advance(2 * time.Second) // the requeue lands on a later flush
	ev.OnCycle(ctx, bad)

	evs := disp.events()
	if len(evs) != 1 || evs[0].Next != StatePending {
		t.Fatalf("got %+v, want only PENDING — a redelivery from a fast-clocked slave is still one cycle", evs)
	}
}

// The rate-limit key is (source, reason), so one line covers every target that
// source reports on and `suppressed` counts across all of them. Naming a bare
// `target` invites an operator to scope the investigation to that one target
// when the whole fleet is affected.
func TestExclusionWarningDoesNotClaimASingleTarget(t *testing.T) {
	ev, cap, clk := newCapturingEvaluator(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"},
	})
	ctx := context.Background()
	window := alertFreshness(time.Minute)

	lagging := lossyCycle("tokyo-1", 100)
	lagging.Time = clk.t.Add(-window - time.Second)
	ev.OnCycle(ctx, lagging)

	recs := cap.at(slog.LevelWarn, "alert.source_excluded")
	if len(recs) != 1 {
		t.Fatalf("got %d warnings, want 1", len(recs))
	}
	if got := attr(recs[0], "example_target"); got != "core/gw" {
		t.Errorf("example_target = %q, want core/gw", got)
	}
	if got := attr(recs[0], "target"); got != "" {
		t.Errorf("warning still carries a bare target=%q; the record spans every target for this source", got)
	}
}

// The floor is disarmed while it sits ahead of the master's clock, which is
// what keeps a forward-dated cycle from muting a source — but disarmed also
// meant the forward-dated cycle itself was readmitted. Ingest accepts one
// config.MaxFutureSkew ahead and PushSink resends any batch whose ack was
// lost, so one measurement drove two sustained increments.
func TestFutureDatedCycleIsNotAppliedTwiceWhileTheFloorIsAhead(t *testing.T) {
	ev, disp, clk := newTestEvaluatorClock(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 2, Actions: []string{"log"},
	})
	ctx := context.Background()

	bad := lossyCycle("tokyo-1", 100)
	bad.Time = clk.t.Add(config.MaxFutureSkew)
	ev.OnCycle(ctx, bad)
	clk.advance(time.Second) // the requeue lands while the floor is still ahead
	ev.OnCycle(ctx, bad)

	evs := disp.events()
	if len(evs) != 1 || evs[0].Next != StatePending {
		t.Fatalf("got %+v, want only PENDING — one measurement cannot sustain an alert twice", evs)
	}
}

// The same replay, taken through the sequence that empties master.cycleDedup:
// genuine cycles replace the poisoned floor with their own while it is still
// ahead of the clock, more than dedupWindowPerSource identities roll the
// forward-dated one out of the ingest window, and it is redelivered against a
// floor that now sits behind it.
func TestFutureDatedCycleIsNotAppliedTwiceOnceTheFloorSitsBehindIt(t *testing.T) {
	ev, disp, clk := newTestEvaluatorClock(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"},
	})
	ctx := context.Background()

	bad := lossyCycle("tokyo-1", 100)
	bad.Time = clk.t.Add(config.MaxFutureSkew)
	ev.OnCycle(ctx, bad)

	clk.advance(time.Minute)
	ev.OnCycle(ctx, cycleAt(clk, "tokyo-1", 0)) // genuine, and behind the poisoned floor
	clk.advance(5 * time.Minute)                // the master's clock passes the poisoned stamp
	disp.reset()

	ev.OnCycle(ctx, bad)
	if evs := disp.events(); len(evs) != 0 {
		t.Fatalf("replayed cycle produced %+v, want nothing — it was applied when it first arrived", evs)
	}
}

// The guard is exact, not a window over everything ahead of the clock: a slave
// whose clock is genuinely fast produces one distinct measurement per interval,
// and every one of them must still count.
func TestDistinctFutureDatedCyclesEachCount(t *testing.T) {
	ev, disp, clk := newTestEvaluatorClock(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 3, Actions: []string{"log"},
	})
	ctx := context.Background()

	for i := range 3 {
		bad := lossyCycle("tokyo-1", 100)
		bad.Time = clk.t.Add(config.MaxFutureSkew - time.Duration(2-i)*time.Minute)
		ev.OnCycle(ctx, bad)
		clk.advance(time.Second)
	}

	evs := disp.events()
	if len(evs) == 0 || evs[len(evs)-1].Next != StateFiring {
		t.Fatalf("got %+v, want FIRING — three distinct cycles are three measurements", evs)
	}
}

// The exact-match set is per (target, alert, source) state and its entries are
// slave-supplied timestamps, so it is bounded by what an honest producer can
// emit inside the window an entry stays useful for, and evicts oldest-first
// rather than growing with whatever a token holder posts.
func TestFutureCycleTrackingIsBounded(t *testing.T) {
	ev, _, clk := newTestEvaluatorClock(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"},
	})
	ctx := context.Background()

	for i := range 20 * aheadCap(time.Minute) {
		bad := lossyCycle("tokyo-1", 100)
		bad.Time = clk.t.Add(config.MaxFutureSkew + time.Duration(i)*time.Nanosecond)
		ev.OnCycle(ctx, bad)
	}

	ev.mu.Lock()
	defer ev.mu.Unlock()
	for _, bySource := range ev.states {
		for source, st := range bySource {
			got := len(st.ahead)
			if got == 0 {
				t.Errorf("%s tracks no timestamp at all; the bound below is vacuous", source)
			}
			if got > aheadCap(time.Minute) {
				t.Errorf("%s tracks %d timestamps ahead of the clock, cap is %d", source, got, aheadCap(time.Minute))
			}
		}
	}
}

// The high-water mark only rises. Letting a genuine cycle behind a poisoned
// floor lower it back reopens the replay through the ordering arm instead of
// the exact one: the forward-dated stamp is then newer than the mark again.
func TestFutureDatedCycleIsNotAppliedTwiceAfterAGenuineCycleBehindIt(t *testing.T) {
	ev, disp, clk := newTestEvaluatorClock(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 2, Actions: []string{"log"},
	})
	ctx := context.Background()

	bad := lossyCycle("tokyo-1", 100)
	bad.Time = clk.t.Add(config.MaxFutureSkew)
	ev.OnCycle(ctx, bad)

	clk.advance(time.Minute)
	ev.OnCycle(ctx, cycleAt(clk, "tokyo-1", 0))
	disp.reset()

	ev.OnCycle(ctx, bad) // redelivered while the poisoned stamp is still ahead
	if evs := disp.events(); len(evs) != 0 {
		t.Fatalf("replayed cycle produced %+v, want nothing", evs)
	}
}

// A stamp the past-dated mark has risen past is barred by ordering alone, so
// keeping it costs memory and buys nothing.
func TestAcceptedFutureStampsAreDroppedOnceTheMarkPassesThem(t *testing.T) {
	ev, _, clk := newTestEvaluatorClock(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"},
	})
	ctx := context.Background()

	bad := lossyCycle("tokyo-1", 100)
	bad.Time = clk.t.Add(config.MaxFutureSkew)
	ev.OnCycle(ctx, bad)
	if got := len(ev.states[aggKey{target: "core/gw", alert: "quorum-test"}]["tokyo-1"].ahead); got != 1 {
		t.Fatalf("tracking %d stamps ahead of the clock, want the one just accepted", got)
	}

	clk.advance(config.MaxFutureSkew + time.Minute)
	ev.OnCycle(ctx, cycleAt(clk, "tokyo-1", 0))
	if got := len(ev.states[aggKey{target: "core/gw", alert: "quorum-test"}]["tokyo-1"].ahead); got != 0 {
		t.Errorf("still tracking %d stamps the ordering mark has passed", got)
	}
}

// newTestEvaluatorInterval is newTestEvaluatorClock with the probe interval
// under test, which is what both aheadCap and the staleness window derive from.
func newTestEvaluatorInterval(t *testing.T, interval time.Duration, a config.Alert) (*Evaluator, *recordingDispatcher, *fakeClock) {
	t.Helper()
	cfg := &config.Config{
		Interval: interval,
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
	return ev, disp, pinClock(ev, testBase)
}

// The cap derives from the interval and config bounds no interval from below,
// so the derivation alone has no ceiling: a 1ns schedule asks for ~6e11 int64,
// which slices.Contains and slices.DeleteFunc then walk on every cycle. A
// bound with no upper limit is not a resource bound.
func TestAheadCapHasAPracticalCeiling(t *testing.T) {
	for _, interval := range []time.Duration{
		time.Nanosecond, time.Microsecond, time.Millisecond, 100 * time.Millisecond,
		time.Second, 20 * time.Second, time.Minute, config.MaxProbeInterval,
	} {
		got := aheadCap(interval)
		if got < 1 {
			t.Errorf("aheadCap(%s) = %d, which remembers nothing", interval, got)
		}
		if int64(got) > int64(cluster.MaxCyclesPerBatch) {
			t.Errorf("aheadCap(%s) = %d, past the %d identities one redelivery can carry",
				interval, got, cluster.MaxCyclesPerBatch)
		}
	}
}

// The ceiling must not become a bound below the producer: every interval whose
// honest emission fits under it keeps the derived depth.
func TestAheadCapKeepsTheDerivationBelowTheCeiling(t *testing.T) {
	cases := []struct {
		interval time.Duration
		want     int
	}{
		{time.Minute, 11},
		{20 * time.Second, 31},
		{time.Second, 601},
		{config.MaxProbeInterval, 4},
	}
	for _, c := range cases {
		if got := aheadCap(c.interval); got != c.want {
			t.Errorf("aheadCap(%s) = %d, want %d", c.interval, got, c.want)
		}
	}
}

// The ceiling has to reach the state, not just the helper: the evaluator reads
// the limit once per cycle off the live config's interval.
func TestFutureStampsStayBoundedAtASubSecondInterval(t *testing.T) {
	ev, _, clk := newTestEvaluatorInterval(t, time.Millisecond, config.Alert{
		Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"},
	})
	ctx := context.Background()

	for i := range cluster.MaxCyclesPerBatch + 64 {
		bad := lossyCycle("tokyo-1", 100)
		bad.Time = clk.t.Add(time.Minute + time.Duration(i)*time.Nanosecond)
		ev.OnCycle(ctx, bad)
	}

	ev.mu.Lock()
	defer ev.mu.Unlock()
	for _, bySource := range ev.states {
		for source, st := range bySource {
			if len(st.ahead) > cluster.MaxCyclesPerBatch {
				t.Errorf("%s tracks %d stamps ahead of the clock, past the %d ceiling",
					source, len(st.ahead), cluster.MaxCyclesPerBatch)
			}
		}
	}
}

// The ceiling must still cover a redelivery: a source whose clock runs ahead
// requeues a whole batch, and cluster.MaxCyclesPerBatch of them is the largest
// one the master accepts. Below that the ceiling would be a bound under its
// own producer, applying the front of every requeued batch twice.
func TestFullForwardDatedBatchIsRecognisedOnRedelivery(t *testing.T) {
	ev, _, clk := newTestEvaluatorInterval(t, 100*time.Millisecond, config.Alert{
		Condition: "loss_pct > 50", Sustained: 2, Actions: []string{"log"},
	})
	ctx := context.Background()

	batch := make([]scheduler.Cycle, cluster.MaxCyclesPerBatch)
	for i := range batch {
		c := lossyCycle("tokyo-1", 100)
		c.Time = clk.t.Add(time.Minute + time.Duration(i)*time.Millisecond)
		batch[i] = c
	}
	for _, c := range batch {
		ev.OnCycle(ctx, c)
	}

	// Past every stamp in the batch, so the ordering arm no longer answers and
	// the exact set is the only thing left recognising the redelivery.
	clk.advance(2 * time.Minute)

	hits := func() int {
		ev.mu.Lock()
		defer ev.mu.Unlock()
		return ev.states[aggKey{target: "core/gw", alert: "quorum-test"}]["tokyo-1"].consecHits
	}
	before := hits()
	for _, c := range batch {
		ev.OnCycle(ctx, c)
	}
	if after := hits(); after != before {
		t.Errorf("redelivered batch drove consecHits %d -> %d; the ceiling is under one batch", before, after)
	}
}

// A panic inside the state machine is recovered by scheduler.Fanout, so the
// process survives — with the evaluator's mutex held forever unless the
// unlock is deferred, after which every cycle blocks while health reports OK.
func TestPanicInEvaluationDoesNotHoldTheStateLock(t *testing.T) {
	ev, _, clk := newTestEvaluatorClock(t, config.Alert{
		Condition: "loss_pct > 50", Sustained: 1, Actions: []string{"log"},
	})
	// A nil bySource map panics on insert inside the critical section — the
	// survivable shape the fanout's recover() turns into a stuck lock.
	ev.states[aggKey{target: "core/gw", alert: "quorum-test"}] = nil

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected the seeded nil map to panic")
			}
		}()
		ev.OnCycle(context.Background(), cycleAt(clk, "tokyo-1", 100))
	}()

	if !ev.mu.TryLock() {
		t.Fatal("evaluator mutex still held after a recovered panic")
	}
	ev.mu.Unlock()
}
