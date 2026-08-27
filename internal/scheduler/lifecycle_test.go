package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/probe"
)

// Anything destructive keyed on "this target is gone" must wait for a Build
// that succeeded. RunLifecycle keeps the OLD scheduler when Build fails, so a
// sweep fired from OnReload deletes state for targets that are still being
// probed — and config.Validate does not check Probe.Type while probe.Build
// rejects an unknown one, so a typo in the same edit that removes a target
// reaches exactly this path.
func TestOnRebuiltDoesNotFireWhenBuildFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeLifecycleConfig(t, path, "gw")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	store := config.NewStore(path, cfg)

	reloads := make(chan struct{}, 1)
	var reloaded, rebuilt, builds atomic.Int64
	buildErr := errors.New("unknown type \"typo\"")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunLifecycle(ctx, LifecycleOptions{
			Log:     slog.New(slog.DiscardHandler),
			Initial: store.Current(),
			Current: store.Current,
			// The initial build succeeds, the reload's fails — deterministic
			// rather than racing a flag against the lifecycle's first call.
			Build: func(c *config.Config) (*Scheduler, error) {
				if builds.Add(1) > 1 {
					return nil, buildErr
				}
				return New(slog.New(slog.DiscardHandler), probe.NewRegistry(), &recordingSink{}, c), nil
			},
			Reloads:   reloads,
			OnReload:  func(*config.Config) { reloaded.Add(1) },
			OnRebuilt: func(*config.Config) { rebuilt.Add(1) },
		})
	}()

	writeLifecycleConfig(t, path, "gw2")
	if err := store.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	reloads <- struct{}{}

	deadline := time.Now().Add(3 * time.Second)
	for reloaded.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done

	if reloaded.Load() == 0 {
		t.Fatal("OnReload never fired")
	}
	if got := rebuilt.Load(); got != 0 {
		t.Fatalf("OnRebuilt fired %d times against a failing Build — the old scheduler is still probing targets a sweep just declared departed", got)
	}
}

func writeLifecycleConfig(t *testing.T, path, target string) {
	t.Helper()
	raw := `{
		"listen": ":8080",
		"interval": "1m",
		"pings": 5,
		"storage": {"clickhouse": {"addr": "ch:9000"}},
		"probes": {"icmp": {"type": "icmp", "timeout": "2s"}},
		"targets": [{"group":"core","targets":[{"name":"` + target + `","host":"1.1.1.1","probe":"icmp"}]}]
	}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// The positive half: OnRebuilt must actually fire after a successful rebuild,
// and the test must assert its own precondition that a rebuild was attempted.
// Asserting only that it does NOT fire on failure is satisfied by a mechanism
// that never fires at all — which is how all three links of the production
// path to Evaluator.PruneDeparted were individually severable with the suite
// green.
func TestOnRebuiltFiresAfterASuccessfulRebuild(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeLifecycleConfig(t, path, "gw")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	store := config.NewStore(path, cfg)

	reloads := make(chan struct{}, 1)
	var builds, rebuilt atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunLifecycle(ctx, LifecycleOptions{
			Log:     slog.New(slog.DiscardHandler),
			Initial: store.Current(),
			Current: store.Current,
			Build: func(c *config.Config) (*Scheduler, error) {
				builds.Add(1)
				return New(slog.New(slog.DiscardHandler), probe.NewRegistry(), &recordingSink{}, c), nil
			},
			Reloads:   reloads,
			OnRebuilt: func(*config.Config) { rebuilt.Add(1) },
		})
	}()

	// The initial Build is what proves RunLifecycle captured its Initial
	// config. A reload landing before that leaves the fingerprint already
	// current, no rebuild is due, and the assertion below fails as a flake.
	started := time.Now().Add(5 * time.Second)
	for builds.Load() == 0 && time.Now().Before(started) {
		time.Sleep(time.Millisecond)
	}
	if builds.Load() == 0 {
		t.Fatal("lifecycle never performed its initial build")
	}

	writeLifecycleConfig(t, path, "gw2")
	if err := store.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	reloads <- struct{}{}

	deadline := time.Now().Add(5 * time.Second)
	for rebuilt.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done

	if got := builds.Load(); got < 2 {
		t.Fatalf("Build ran %d times, want the initial plus the reload — the fixture never reached the rebuild path", got)
	}
	if got := rebuilt.Load(); got != 1 {
		t.Fatalf("OnRebuilt fired %d times after a successful rebuild, want 1 — the destructive sweep it carries is unreachable in the shipped binary", got)
	}
}

// Supervisor must forward OnRebuilt; without it the hook exists and nothing
// production ever calls it.
func TestSupervisorForwardsOnRebuilt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeLifecycleConfig(t, path, "gw")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	store := config.NewStore(path, cfg)

	signals := make(chan struct{}, 1)
	var rebuilt, builds atomic.Int64
	sup := &Supervisor{
		Log:     slog.New(slog.DiscardHandler),
		Store:   store,
		Signals: signals,
		Build: func(c *config.Config) (*Scheduler, error) {
			builds.Add(1)
			return New(slog.New(slog.DiscardHandler), probe.NewRegistry(), &recordingSink{}, c), nil
		},
		OnRebuilt: func(*config.Config) { rebuilt.Add(1) },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	// The initial Build is what proves Run captured its Initial config; a
	// reload landing before that leaves the fingerprint already current and no
	// rebuild is due.
	started := time.Now().Add(5 * time.Second)
	for builds.Load() == 0 && time.Now().Before(started) {
		time.Sleep(time.Millisecond)
	}
	if builds.Load() == 0 {
		t.Fatal("supervisor never performed its initial build")
	}

	writeLifecycleConfig(t, path, "gw2")
	if err := store.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	signals <- struct{}{}

	deadline := time.Now().Add(5 * time.Second)
	for rebuilt.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done

	if rebuilt.Load() == 0 {
		t.Fatal("Supervisor did not forward OnRebuilt; the alert sweep is unreachable through the only path production uses")
	}
}
