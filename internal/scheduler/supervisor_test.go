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

func TestFingerprintIgnoresAlertsAndSlaves(t *testing.T) {
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
		{Name: "a", Host: "1.1.1.1", Probe: "icmp",
			Alerts: []string{"x"}, Slaves: []string{"s1"}},
	}}}
	if Fingerprint(a) != Fingerprint(&b) {
		t.Error("alerts/slaves edits must not change fingerprint (they're not scheduler-visible)")
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
		"probes": map[string]any{
			"fake": map[string]any{"type": "icmp", "timeout": "1s"},
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
