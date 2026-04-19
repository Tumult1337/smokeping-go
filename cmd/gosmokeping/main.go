package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/joho/godotenv"

	"github.com/tumult/gosmokeping/internal/alert"
	"github.com/tumult/gosmokeping/internal/api"
	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/probe"
	"github.com/tumult/gosmokeping/internal/scheduler"
	"github.com/tumult/gosmokeping/internal/storage"
	"github.com/tumult/gosmokeping/internal/ui"
)

func main() {
	// Load .env before flag.Parse so expanded ${NAME} references in config.json
	// resolve against the merged environment. Silent no-op when absent; real
	// shell env always wins over .env, per godotenv default.
	_ = godotenv.Load()

	var (
		configPath = flag.String("config", "config.json", "path to config file")
		logLevel   = flag.String("log-level", "info", "log level: debug|info|warn|error")
	)
	flag.Parse()

	log := newLogger(*logLevel)
	// Route package-level slog calls (e.g. in internal/probe) through the
	// configured handler so -log-level debug surfaces per-request probe errors.
	slog.SetDefault(log)

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Error("load config", "path", *configPath, "err", err)
		os.Exit(1)
	}
	store := config.NewStore(*configPath, cfg)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

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

	server := api.New(api.Options{
		Log:    log,
		Store:  store,
		Reader: reader,
		UIFS:   ui.FS(),
	})
	go func() {
		if err := api.Serve(ctx, log, cfg.Listen, server.Router()); err != nil {
			log.Error("http server", "err", err)
			cancel()
		}
	}()

	sch := scheduler.New(log, registry, scheduler.Fanout(sinks...), cfg)
	sch.Run(ctx)

	log.Info("gosmokeping shutting down")
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
