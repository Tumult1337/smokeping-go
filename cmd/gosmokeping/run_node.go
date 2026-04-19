package main

import (
	"context"
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
// running the scheduler until ctx is cancelled.
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

	registry, err := probe.Build(cfg.Probes)
	if err != nil {
		log.Error("build probes", "err", err)
		os.Exit(1)
	}

	log.Info("gosmokeping starting",
		"listen", cfg.Listen,
		"interval", cfg.Interval,
		"pings", cfg.Pings,
		"targets", len(cfg.AllTargets()))

	sinks := []scheduler.Sink{&scheduler.LogSink{Log: log}}
	var clusterRegistry *master.Registry
	var reader api.StorageReader
	if cfg.InfluxDB.URL != "" && cfg.InfluxDB.Token != "" {
		if err := storage.Bootstrap(ctx, log, cfg.InfluxDB); err != nil {
			log.Error("bootstrap storage", "err", err)
			os.Exit(1)
		}
		writer := storage.NewWriter(log, cfg.InfluxDB)
		defer writer.Close()
		sinks = append(sinks, writer)

		r := storage.NewReader(cfg.InfluxDB)
		defer r.Close()
		reader = r
	} else {
		log.Warn("influxdb not configured, running without persistent storage")
	}

	if len(cfg.Alerts) > 0 {
		dispatcher := alert.NewDispatcher(log, store)
		evaluator, err := alert.NewEvaluator(log, store, dispatcher)
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
		clusterRegistry = master.NewRegistry()
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

	// The master probes only unassigned targets locally; anything with an
	// explicit Slaves list is the assigned slaves' job. The stored cfg is
	// still the UI/ingest source of truth — only the scheduler sees the
	// filtered view.
	sch := scheduler.New(log, registry, fanout, master.LocalTargets(cfg))
	sch.Run(ctx)

	log.Info("gosmokeping shutting down")
}
