package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/tumult/gosmokeping/internal/alert"
	"github.com/tumult/gosmokeping/internal/api"
	"github.com/tumult/gosmokeping/internal/cluster/master"
	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/probe"
	"github.com/tumult/gosmokeping/internal/scheduler"
	"github.com/tumult/gosmokeping/internal/slavehealth"
	"github.com/tumult/gosmokeping/internal/storage"
	"github.com/tumult/gosmokeping/internal/ui"
)

// runNode is the default (non-slave) entrypoint: loads a full config, wires
// storage + alerts + UI + optional cluster master endpoints, and blocks
// running the scheduler (via Supervisor, so SIGHUP-triggered target edits are
// applied live) until ctx is cancelled. Returns an error rather than calling
// os.Exit so deferred cleanup (notably backend.close, which flushes the
// batching writer) always runs before the process exits.
func runNode(ctx context.Context, log *slog.Logger, configPath, version string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config %q: %w", configPath, err)
	}
	store := config.NewStore(configPath, cfg)

	store.WatchSIGHUP(ctx, log)
	if err := store.WatchFile(ctx, log); err != nil {
		log.Warn("config file watch disabled", "err", err)
	}

	log.Info("gosmokeping starting",
		"listen", cfg.Listen,
		"interval", cfg.Interval,
		"pings", cfg.Pings,
		"targets", len(cfg.AllTargets()))

	sinks := []scheduler.Sink{&scheduler.LogSink{Log: log}}
	var reader storage.Reader

	backend, err := openStorage(ctx, log, cfg.Storage)
	switch {
	case err == nil:
		defer backend.close()
		sinks = append(sinks, backend.sink)
		// Wrap the reader with two LRU caches so live UI auto-refresh and
		// historical browsing don't re-issue identical ClickHouse queries.
		// Cycles entries are tiny (~hundreds of KB) so 256 is fine; hops
		// timeline entries can be ~100MB at 7d, so we cap hops at 16 to
		// keep worst-case resident memory bounded (~1.5GB upper bound vs.
		// ~25GB if both caps were 256). Inner reader lifetime is still
		// managed by backend.close.
		reader = storage.NewCachingReader(backend.reader, 256, 16)
	case errors.Is(err, storage.ErrDisabled):
		log.Warn("storage backend disabled, running without persistent storage",
			"storage", "clickhouse")
	default:
		return fmt.Errorf("open storage clickhouse: %w", err)
	}

	// Always construct the evaluator, even with zero alerts at startup. It is a
	// cheap no-op sink when there are no conditions, and building it up front
	// lets a later SIGHUP that adds the first alert take effect via Refresh —
	// otherwise the nil→non-nil transition would require a process restart.
	dispatcher := alert.NewDispatcher(log, store)
	evaluator, err := alert.NewEvaluator(log, store, dispatcher)
	if err != nil {
		return fmt.Errorf("init alert evaluator: %w", err)
	}
	sinks = append(sinks, evaluator)

	// Build the fanout once — slave-inbound cycles flow through the exact same
	// sinks as locally-probed ones (Writer, alert evaluator, log sink).
	fanout := scheduler.Fanout(log, sinks...)

	// schedulerSignals is the single wakeup channel the Supervisor subscribes
	// to. Both config reloads and cluster registry changes feed it.
	schedulerSignals := make(chan struct{}, 1)
	healthSet := func() *slavehealth.Set { return nil }

	var clusterHandler http.Handler
	var slaveLister api.SlaveLister
	var healthLister api.HealthLister
	if cfg.Cluster != nil && cfg.Cluster.Token != "" {
		clusterRegistry := master.NewRegistry(log)

		pins, err := cfg.Cluster.ParsedSlaveAddrs()
		if err != nil {
			return fmt.Errorf("cluster.slave_addrs: %w", err)
		}
		clusterRegistry.SetPins(pins)

		// healthSet is read on every scheduler build, on every /config
		// request and on every API target listing, so it is a closure over
		// the live registry and the live config rather than a snapshot
		// captured once — cluster.health_alerts must follow a SIGHUP the
		// same way a target's own alerts do.
		healthSet = func() *slavehealth.Set {
			var alerts []string
			if cl := store.Current().Cluster; cl != nil {
				alerts = cl.HealthAlerts
			}
			return slavehealth.NewSet(clusterRegistry.Peers(), alerts)
		}

		// Registry changes drive a scheduler rebuild through the same channel
		// as a SIGHUP reload, debounced so a fleet restart costs one rebuild.
		rawSignals := make(chan struct{}, 64)
		clusterRegistry.SetOnChange(func() {
			select {
			case rawSignals <- struct{}{}:
			default: // a rebuild is already pending; nothing to add
			}
		})
		go debounce(ctx, rawSignals, schedulerSignals, healthSignalDelay)

		clusterHandler = master.NewServer(log, store, clusterRegistry, fanout, cfg.Cluster.Token, healthSet).Handler()
		slaveLister = clusterRegistry
		// The API is handed the Public() view of the same snapshot the
		// scheduler builds from, never the registry itself — Peers() carries
		// real addresses and must not be reachable from a request handler.
		healthLister = healthListerFunc(func() []config.TargetRef {
			return healthSet().Public()
		})
		log.Info("cluster endpoints enabled", "source", cfg.Cluster.Source)
		// Evict slaves that haven't checked in for 24 hours to prevent unbounded
		// registry growth in environments with ephemeral or renamed slaves.
		go func() {
			t := time.NewTicker(time.Hour)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					clusterRegistry.Sweep(24 * time.Hour)
				}
			}
		}()
	}

	server := api.New(api.Options{
		Log:            log,
		Store:          store,
		Reader:         reader,
		UIFS:           ui.FS(),
		ClusterHandler: clusterHandler,
		Slaves:         slaveLister,
		Health:         healthLister,
		Version:        version,
	})
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- api.Serve(ctx, log, cfg.Listen, server.Router())
	}()

	// The Supervisor owns the scheduler goroutine across config reloads. Build
	// rebuilds the probe registry and reapplies master.LocalTargets on every
	// reload so slave reassignments and probe-timeout edits take effect live.
	// OnReload re-parses alert conditions on the same thread.
	sup := &scheduler.Supervisor{
		Log:     log,
		Store:   store,
		Signals: schedulerSignals,
		Build: func(c *config.Config) (*scheduler.Scheduler, error) {
			local, registry, err := localView(c, healthSet())
			if err != nil {
				return nil, err
			}
			return scheduler.New(log, registry, fanout, local), nil
		},
		ExtraFingerprint: func() string { return healthSet().Fingerprint() },
		OnReload: func(c *config.Config) {
			if err := evaluator.Refresh(); err != nil {
				log.Error("alert refresh failed, keeping previous conditions", "err", err)
			}
			log.Info("config reload applied",
				"targets", len(c.AllTargets()),
				"interval", c.Interval,
				"pings", c.Pings)
		},
	}
	if err := sup.Run(ctx); err != nil {
		return fmt.Errorf("scheduler supervisor: %w", err)
	}

	// Wait for the HTTP server to finish draining in-flight requests before
	// closing the backend — otherwise handlers still running after the scheduler
	// stops could call a closed reader.
	if err := <-serverDone; err != nil {
		log.Error("http server", "err", err)
	}
	log.Info("gosmokeping shutting down")
	return nil
}

// localView returns the master's own probe view and the probe registry built
// from that same view.
//
// The two must be built together. LocalTargets injects the synthetic
// _slave_health probe into a clone of the probe map, and config.Validate
// rejects that name in user config, so a registry built from the stored
// cfg.Probes can never resolve it — every health target would be dropped by
// the scheduler with "probe not found for target" and the mesh would collect
// nothing.
func localView(c *config.Config, health *slavehealth.Set) (*config.Config, *probe.Registry, error) {
	local := master.LocalTargets(c, health)
	registry, err := probe.Build(local.Probes)
	if err != nil {
		return nil, nil, err
	}
	return local, registry, nil
}

// healthListerFunc adapts a closure to api.HealthLister.
type healthListerFunc func() []config.TargetRef

func (f healthListerFunc) PublicTargets() []config.TargetRef { return f() }
