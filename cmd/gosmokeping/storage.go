package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/scheduler"
	"github.com/tumult/gosmokeping/internal/storage"
	"github.com/tumult/gosmokeping/internal/storage/influxv2"
	"github.com/tumult/gosmokeping/internal/storage/influxv3"
	"github.com/tumult/gosmokeping/internal/storage/prom"
)

// storageBackend is the composition-root view of a persistence implementation:
// a sink the scheduler fans into, a reader the API queries, and a Close to
// flush on shutdown. Kept here (not in the storage package) because the
// factory imports every backend subpackage — putting it in storage itself
// would create a cycle with storage/influxv2 which depends on storage for
// shared data types.
type storageBackend struct {
	sink   scheduler.Sink
	reader storage.Reader
	close  func() error
}

// openStorage builds the backend selected by cfg.Backend. Returns
// storage.ErrDisabled when the selected backend has no credentials — the
// caller logs a warning and runs without persistent storage. Returns
// storage.ErrBackendNotImplemented when the backend is recognised but its
// implementation is still a stub (influxv3, prometheus).
func openStorage(ctx context.Context, log *slog.Logger, cfg config.Storage) (*storageBackend, error) {
	switch cfg.Backend {
	case "":
		return nil, storage.ErrDisabled
	case config.BackendInfluxV2:
		if cfg.InfluxV2.URL == "" || cfg.InfluxV2.Token == "" {
			return nil, storage.ErrDisabled
		}
		if err := influxv2.Bootstrap(ctx, log, cfg.InfluxV2); err != nil {
			return nil, fmt.Errorf("bootstrap influxv2: %w", err)
		}
		w := influxv2.NewWriter(log, cfg.InfluxV2)
		r := influxv2.NewReader(cfg.InfluxV2)
		return &storageBackend{
			sink:   w,
			reader: r,
			close: func() error {
				w.Close()
				r.Close()
				return nil
			},
		}, nil
	case config.BackendInfluxV3:
		if _, err := influxv3.Open(ctx, log, cfg.InfluxV3); err != nil {
			if errors.Is(err, influxv3.ErrBackendNotImplemented) {
				return nil, storage.ErrBackendNotImplemented
			}
			return nil, err
		}
		return nil, storage.ErrBackendNotImplemented
	case config.BackendPrometheus:
		if _, err := prom.Open(ctx, log, cfg.Prometheus); err != nil {
			if errors.Is(err, prom.ErrBackendNotImplemented) {
				return nil, storage.ErrBackendNotImplemented
			}
			return nil, err
		}
		return nil, storage.ErrBackendNotImplemented
	default:
		return nil, fmt.Errorf("unknown storage backend %q", cfg.Backend)
	}
}
