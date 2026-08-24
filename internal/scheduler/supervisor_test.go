package scheduler

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/probe"
)

func TestFingerprintStableWithinEqualConfig(t *testing.T) {
	a := &config.Config{
		Interval: 10 * time.Second,
		Pings:    5,
		Probes: map[string]config.Probe{
			"icmp": {Type: "icmp", Timeout: time.Second},
			"tcp":  {Type: "tcp", Timeout: 2 * time.Second},
		},
		Targets: []config.Group{{
			Group: "g", Targets: []config.Target{
				{Name: "a", Host: "1.1.1.1", Probe: "icmp"},
			},
		}},
	}
	b := &config.Config{
		Interval: 10 * time.Second,
		Pings:    5,
		Probes: map[string]config.Probe{
			"tcp":  {Type: "tcp", Timeout: 2 * time.Second},
			"icmp": {Type: "icmp", Timeout: time.Second},
		},
		Targets: []config.Group{{
			Group: "g", Targets: []config.Target{
				{Name: "a", Host: "1.1.1.1", Probe: "icmp"},
			},
		}},
	}
	if Fingerprint(a) != Fingerprint(b) {
		t.Error("fingerprint must be insensitive to probe-map iteration order")
	}
}

func TestFingerprintChangesOnTargetEdits(t *testing.T) {
	base := &config.Config{
		Interval: 10 * time.Second,
		Pings:    5,
		Probes:   map[string]config.Probe{"icmp": {Type: "icmp", Timeout: time.Second}},
		Targets: []config.Group{{
			Group: "g", Targets: []config.Target{
				{Name: "a", Host: "1.1.1.1", Probe: "icmp"},
			},
		}},
	}
	before := Fingerprint(base)

	added := *base
	added.Targets = []config.Group{{Group: "g", Targets: []config.Target{
		{Name: "a", Host: "1.1.1.1", Probe: "icmp"},
		{Name: "b", Host: "2.2.2.2", Probe: "icmp"},
	}}}
	if Fingerprint(&added) == before {
		t.Error("adding a target should change fingerprint")
	}

	pings := *base
	pings.Pings = 10
	if Fingerprint(&pings) == before {
		t.Error("changing pings should change fingerprint")
	}
}

// A target's alert *attachment* list (config.Target.Alerts) is baked into
// the scheduler's config at Build time — same as probe/host/url — so editing
// it must change the fingerprint, or attaching an alert and sending SIGHUP
// would leave the scheduler running the pre-attachment shape while
// /api/v1/targets already reports it attached.
func TestFingerprintChangesOnAlertAttachment(t *testing.T) {
	a := &config.Config{
		Interval: time.Second,
		Pings:    3,
		Probes:   map[string]config.Probe{"icmp": {Type: "icmp", Timeout: time.Second}},
		Targets: []config.Group{{
			Group: "g", Targets: []config.Target{
				{Name: "a", Host: "1.1.1.1", Probe: "icmp"},
			},
		}},
	}
	b := *a
	b.Targets = []config.Group{{Group: "g", Targets: []config.Target{
		{Name: "a", Host: "1.1.1.1", Probe: "icmp", Alerts: []string{"x"}},
	}}}
	if Fingerprint(a) == Fingerprint(&b) {
		t.Error("attaching an alert to a target must change fingerprint (baked into config.Target at Build time)")
	}
}

// Alert *definitions* (cfg.Alerts — condition, sustained, actions, quorum)
// are re-read from the live config store per cycle by the evaluator, not
// baked into anything the scheduler builds, so editing one must NOT force a
// scheduler rebuild.
func TestFingerprintIgnoresAlertDefinitions(t *testing.T) {
	a := &config.Config{
		Interval: time.Second,
		Pings:    3,
		Probes:   map[string]config.Probe{"icmp": {Type: "icmp", Timeout: time.Second}},
		Targets: []config.Group{{
			Group: "g", Targets: []config.Target{
				{Name: "a", Host: "1.1.1.1", Probe: "icmp", Alerts: []string{"x"}},
			},
		}},
		Alerts: map[string]config.Alert{
			"x": {Condition: "loss > 0", Sustained: 1, Actions: []string{"log"}},
		},
	}
	b := *a
	b.Alerts = map[string]config.Alert{
		"x": {Condition: "loss > 50", Sustained: 5, Actions: []string{"webhook"}},
	}
	if Fingerprint(a) != Fingerprint(&b) {
		t.Error("alert definition edits must not change fingerprint (re-read per cycle by the evaluator)")
	}
}

func TestFingerprintSlavesChange(t *testing.T) {
	a := &config.Config{
		Interval: time.Second,
		Pings:    3,
		Probes:   map[string]config.Probe{"icmp": {Type: "icmp", Timeout: time.Second}},
		Targets: []config.Group{{
			Group: "g", Targets: []config.Target{
				{Name: "a", Host: "1.1.1.1", Probe: "icmp"},
			},
		}},
	}
	b := *a
	b.Targets = []config.Group{{Group: "g", Targets: []config.Target{
		{Name: "a", Host: "1.1.1.1", Probe: "icmp", Slaves: []string{"s1"}},
	}}}
	if Fingerprint(a) == Fingerprint(&b) {
		t.Error("slave assignment changes must change fingerprint (affects master.LocalTargets filtering)")
	}
}

// writeConfig writes a valid minimal Config to disk at path as JSON so the
// Store.Reload() path exercises the full load-and-validate cycle.
func writeConfig(t *testing.T, path string, targets []config.Target) {
	t.Helper()
	cfg := map[string]any{
		"listen":   "127.0.0.1:0",
		"interval": "30ms",
		"pings":    1,
		"storage":  map[string]any{"clickhouse": map[string]any{"addr": "ch:9000"}},
		"probes": map[string]any{
			// Not icmp: a 30ms interval cannot schedule an icmp ping batch, and
			// the registry below serves this name with a fake probe anyway.
			"fake": map[string]any{"type": "tcp", "timeout": "1s"},
		},
		"targets": []any{map[string]any{
			"group":   "g",
			"targets": targets,
		}},
	}
	buf, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestSupervisorRebuildsOnTargetChange(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	reg := probe.NewRegistry()
	reg.Register(&fakeProbe{name: "fake", rtts: []time.Duration{10 * time.Millisecond}})

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeConfig(t, path, []config.Target{
		{Name: "a", Host: "1.1.1.1", Probe: "fake"},
	})

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	store := config.NewStore(path, cfg)
	sink := &recordingSink{}

	var builds atomic.Int32
	var reloads atomic.Int32

	sup := &Supervisor{
		Log:   log,
		Store: store,
		Build: func(c *config.Config) (*Scheduler, error) {
			builds.Add(1)
			return New(log, reg, sink, c), nil
		},
		OnReload: func(_ *config.Config) { reloads.Add(1) },
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	time.Sleep(80 * time.Millisecond)
	if b := builds.Load(); b != 1 {
		t.Errorf("initial builds = %d, want 1", b)
	}

	// Reload with identical contents — no rebuild, but OnReload still fires.
	if err := store.Reload(); err != nil {
		t.Fatalf("reload unchanged: %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	if b := builds.Load(); b != 1 {
		t.Errorf("identical reload rebuilt scheduler: builds = %d", b)
	}
	if r := reloads.Load(); r != 1 {
		t.Errorf("identical reload fired OnReload %d times, want 1", r)
	}

	// Reload after adding a new target — fingerprint must change, scheduler
	// must be rebuilt, and cycles from target "b" must show up in the sink.
	writeConfig(t, path, []config.Target{
		{Name: "a", Host: "1.1.1.1", Probe: "fake"},
		{Name: "b", Host: "2.2.2.2", Probe: "fake"},
	})
	if err := store.Reload(); err != nil {
		t.Fatalf("reload changed: %v", err)
	}
	// Give the supervisor time to tear down the old scheduler and let the new
	// one fire at least one cycle per target (interval is 30ms).
	time.Sleep(200 * time.Millisecond)

	if b := builds.Load(); b != 2 {
		t.Errorf("changed reload rebuilds = %d, want 2", b)
	}
	if r := reloads.Load(); r != 2 {
		t.Errorf("OnReload calls = %d, want 2", r)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("supervisor returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("supervisor did not exit after cancel")
	}

	seen := map[string]bool{}
	for _, c := range sink.snapshot() {
		seen[c.Target.ID()] = true
	}
	if !seen["g/a"] {
		t.Error("target a never fired")
	}
	if !seen["g/b"] {
		t.Error("target b never fired after reload — scheduler didn't pick up the new target")
	}
}

// testConfig returns a minimal valid config for lifecycle tests that exercise
// the rebuild decision rather than real probing. Deliberately zero targets:
// these tests commonly build a Scheduler with a nil registry/sink (only the
// build-count and fingerprint behavior matters), and Scheduler.Run would nil
// dereference on a real target with no registry behind it.
func testConfig() *config.Config {
	return &config.Config{
		Interval: 20 * time.Millisecond,
		Pings:    1,
		Probes:   map[string]config.Probe{"fake": {Type: "icmp", Timeout: time.Second}},
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within 2s")
}

// Health targets are injected inside Build and are absent from the stored
// config, so the config fingerprint alone cannot see a mesh change. Without
// ExtraFingerprint a registry change signals a reload that then no-ops.
func TestLifecycleRebuildsOnExtraFingerprintChange(t *testing.T) {
	cfg := testConfig()
	var builds atomic.Int64
	var extra atomic.Value
	extra.Store("mesh-a")

	reloads := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- RunLifecycle(ctx, LifecycleOptions{
			Log:     slog.New(slog.DiscardHandler),
			Initial: cfg,
			Current: func() *config.Config { return cfg },
			Build: func(*config.Config) (*Scheduler, error) {
				builds.Add(1)
				return New(slog.New(slog.DiscardHandler), nil, nil, cfg), nil
			},
			Reloads:          reloads,
			ExtraFingerprint: func() string { return extra.Load().(string) },
		})
	}()

	waitFor(t, func() bool { return builds.Load() == 1 })

	// Same config, same extra — no rebuild.
	reloads <- struct{}{}
	time.Sleep(50 * time.Millisecond)
	if got := builds.Load(); got != 1 {
		t.Fatalf("got %d builds, want 1 (unchanged fingerprint must not rebuild)", got)
	}

	// Same config, changed extra — rebuild.
	extra.Store("mesh-b")
	reloads <- struct{}{}
	waitFor(t, func() bool { return builds.Load() == 2 })

	cancel()
	<-done
}
