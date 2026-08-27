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
