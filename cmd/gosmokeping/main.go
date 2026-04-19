package main

import (
	"context"
	"flag"
	"log/slog"
	"os/signal"
	"syscall"

	"github.com/joho/godotenv"
)

func main() {
	// Load .env before flag.Parse so expanded ${NAME} references in config.json
	// resolve against the merged environment. Silent no-op when absent; real
	// shell env always wins over .env, per godotenv default.
	_ = godotenv.Load()

	var (
		configPath = flag.String("config", "config.json", "path to config file")
		logLevel   = flag.String("log-level", "info", "log level: debug|info|warn|error")
		slaveMode  = flag.Bool("slave", false, "run as a cluster slave (register + push to master, no local storage)")
	)
	flag.Parse()

	log := newLogger(*logLevel)
	// Route package-level slog calls (e.g. in internal/probe) through the
	// configured handler so -log-level debug surfaces per-request probe errors.
	slog.SetDefault(log)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if *slaveMode {
		runSlave(ctx, log, *configPath)
		return
	}
	runNode(ctx, log, *configPath)
}
