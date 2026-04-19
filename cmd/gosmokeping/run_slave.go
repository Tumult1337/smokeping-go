package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/tumult/gosmokeping/internal/cluster/slave"
	"github.com/tumult/gosmokeping/internal/config"
)

// runSlave is the --slave entrypoint: load a minimal config, spin up the
// slave runner, and block until ctx is cancelled. Slaves never touch storage
// or expose the UI — the master does all of that — so we skip the entire
// node-side plumbing.
func runSlave(ctx context.Context, log *slog.Logger, configPath string) {
	cfg, err := config.LoadMinimal(configPath)
	if err != nil {
		log.Error("load slave config", "path", configPath, "err", err)
		os.Exit(1)
	}
	runner := slave.NewRunner(log, cfg, "dev")
	if err := runner.Run(ctx); err != nil {
		log.Error("slave exited with error", "err", err)
		os.Exit(1)
	}
	log.Info("slave shutting down")
}
