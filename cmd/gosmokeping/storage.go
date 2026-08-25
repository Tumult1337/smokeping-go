package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/tumult/gosmokeping/internal/api"
	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/scheduler"
	"github.com/tumult/gosmokeping/internal/storage"
	"github.com/tumult/gosmokeping/internal/storage/clickhouse"
)

// storageBackend is the composition-root view of the persistence layer:
// a sink the scheduler fans into, a reader the API queries, and a Close
// to flush on shutdown.
type storageBackend struct {
	sink   scheduler.Sink
	reader storage.Reader
	stats  api.WriterStats
	close  func()
}

// openStorage builds the ClickHouse backend. Returns storage.ErrDisabled
// when no address is configured — the caller logs a warning and runs
// without persistent storage.
func openStorage(ctx context.Context, log *slog.Logger, cfg config.Storage, pings int) (*storageBackend, error) {
	if cfg.ClickHouse.Addr == "" {
		return nil, storage.ErrDisabled
	}
	if err := clickhouse.Bootstrap(ctx, log, cfg.ClickHouse); err != nil {
		return nil, fmt.Errorf("bootstrap clickhouse: %w", err)
	}
	w, err := clickhouse.NewWriter(ctx, log, cfg.ClickHouse, pings)
	if err != nil {
		return nil, fmt.Errorf("clickhouse writer: %w", err)
	}
	r, err := clickhouse.NewReader(ctx, cfg.ClickHouse)
	if err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("clickhouse reader: %w", err)
	}
	return &storageBackend{
		sink:   w,
		reader: r,
		stats:  w,
		close: func() {
			_ = w.Close()
			_ = r.Close()
		},
	}, nil
}
