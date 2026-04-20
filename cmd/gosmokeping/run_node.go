package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"

	"github.com/tumult/gosmokeping/internal/alert"
	"github.com/tumult/gosmokeping/internal/api"
	"github.com/tumult/gosmokeping/internal/cluster/master"
	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/probe"
	"github.com/tumult/gosmokeping/internal/scheduler"
	"github.com/tumult/gosmokeping/internal/storage"
	"github.com/tumult/gosmokeping/internal/ui"
)

// runNode is the default (non-slave) entrypoint: loads a full config, wires
// storage + alerts + UI + optional cluster master endpoints, and blocks
// running the scheduler (via Supervisor, so SIGHUP-triggered target edits are
// applied live) until ctx is cancelled.
func runNode(ctx context.Context, log *slog.Logger, configPath string) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Error("load config", "path", configPath, "err", err)
		os.Exit(1)
	}
	store := config.NewStore(configPath, cfg)

	store.WatchSIGHUP(ctx, log)

	log.Info("gosmokeping starting",
		"listen", cfg.Listen,
		"interval", cfg.Interval,
		"pings", cfg.Pings,
		"targets", len(cfg.AllTargets()))

	sinks := []scheduler.Sink{&scheduler.LogSink{Log: log}}
	var reader api.StorageReader

	backend, err := openStorage(ctx, log, cfg.Storage)
	switch {
	case err == nil:
		defer backend.close()
		sinks = append(sinks, backend.sink)
		reader = backend.reader
	case errors.Is(err, storage.ErrDisabled):
		log.Warn("storage backend disabled, running without persistent storage",
			"backend", cfg.Storage.Backend)
	case errors.Is(err, storage.ErrBackendNotImplemented):
		log.Error("configured storage backend is not implemented",
			"backend", cfg.Storage.Backend)
		os.Exit(1)
	default:
		log.Error("open storage", "backend", cfg.Storage.Backend, "err", err)
		os.Exit(1)
	}

	var evaluator *alert.Evaluator
	if len(cfg.Alerts) > 0 {
		dispatcher := alert.NewDispatcher(log, store)
		evaluator, err = alert.NewEvaluator(log, store, dispatcher)
		if err != nil {
			log.Error("init alert evaluator", "err", err)
			os.Exit(1)
		}
		sinks = append(sinks, evaluator)
	}

	// Build the fanout once — slave-inbound cycles flow through the exact same
	// sinks as locally-probed ones (Writer, alert evaluator, log sink).
	fanout := scheduler.Fanout(sinks...)

	var clusterHandler http.Handler
	var slaveLister api.SlaveLister
	if cfg.Cluster != nil && cfg.Cluster.Token != "" {
		clusterRegistry := master.NewRegistry()
		clusterHandler = master.NewServer(log, store, clusterRegistry, fanout, cfg.Cluster.Token).Handler()
		slaveLister = clusterRegistry
		log.Info("cluster endpoints enabled", "source", cfg.Cluster.Source)
	}

	server := api.New(api.Options{
		Log:            log,
		Store:          store,
		Reader:         reader,
		UIFS:           ui.FS(),
		ClusterHandler: clusterHandler,
		Slaves:         slaveLister,
	})
	go func() {
		if err := api.Serve(ctx, log, cfg.Listen, server.Router()); err != nil {
			log.Error("http server", "err", err)
			cancel()
		}
	}()

	// The Supervisor owns the scheduler goroutine across config reloads. Build
	// rebuilds the probe registry and reapplies master.LocalTargets on every
	// reload so slave reassignments and probe-timeout edits take effect live.
	// OnReload re-parses alert conditions on the same thread.
	sup := &scheduler.Supervisor{
		Log:   log,
		Store: store,
		Build: func(c *config.Config) (*scheduler.Scheduler, error) {
			registry, err := probe.Build(c.Probes)
			if err != nil {
				return nil, err
			}
			return scheduler.New(log, registry, fanout, master.LocalTargets(c)), nil
		},
		OnReload: func(c *config.Config) {
			if evaluator != nil {
				if err := evaluator.Refresh(); err != nil {
					log.Error("alert refresh failed, keeping previous conditions", "err", err)
				}
			}
			log.Info("config reload applied",
				"targets", len(c.AllTargets()),
				"interval", c.Interval,
				"pings", c.Pings)
		},
	}
	if err := sup.Run(ctx); err != nil {
		log.Error("scheduler supervisor", "err", err)
		os.Exit(1)
	}

	log.Info("gosmokeping shutting down")
}
