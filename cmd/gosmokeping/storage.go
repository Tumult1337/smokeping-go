package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/scheduler"
	"github.com/tumult/gosmokeping/internal/storage"
	"github.com/tumult/gosmokeping/internal/storage/influxv2"
	"github.com/tumult/gosmokeping/internal/storage/influxv3"
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
// caller logs a warning and runs without persistent storage.
func openStorage(ctx context.Context, log *slog.Logger, cfg config.Storage) (*storageBackend, error) {
	hopPolicy, err := buildHopPolicy(log, cfg.HopPolicy)
	if err != nil {
		return nil, err
	}

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
		w := influxv2.NewWriter(log, cfg.InfluxV2, hopPolicy)
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
		if cfg.InfluxV3.URL == "" || cfg.InfluxV3.Token == "" {
			return nil, storage.ErrDisabled
		}
		if err := influxv3.Bootstrap(ctx, log, cfg.InfluxV3); err != nil {
			return nil, fmt.Errorf("bootstrap influxv3: %w", err)
		}
		w, err := influxv3.NewWriter(log, cfg.InfluxV3, hopPolicy)
		if err != nil {
			return nil, fmt.Errorf("influxv3 writer: %w", err)
		}
		r, err := influxv3.NewReader(cfg.InfluxV3)
		if err != nil {
			w.Close()
			return nil, fmt.Errorf("influxv3 reader: %w", err)
		}
		return &storageBackend{
			sink:   w,
			reader: r,
			close: func() error {
				w.Close()
				r.Close()
				return nil
			},
		}, nil
	default:
		return nil, fmt.Errorf("unknown storage backend %q", cfg.Backend)
	}
}

// buildHopPolicy turns the config sub-block into a runtime HopPolicy. Returns
// (nil, nil) when the mode is empty so callers get a nil receiver that
// behaves as "always" — i.e. legacy behaviour unchanged unless the operator
// explicitly opts in.
func buildHopPolicy(log *slog.Logger, cfg config.HopPolicy) (*storage.HopPolicy, error) {
	if cfg.Mode == "" {
		return nil, nil
	}
	var sampleEvery time.Duration
	if cfg.Mode == "sampled" {
		d, err := cfg.ParsedSampleEvery()
		if err != nil {
			return nil, err
		}
		sampleEvery = d
	}
	p, err := storage.NewHopPolicy(cfg.Mode, sampleEvery)
	if err != nil {
		return nil, fmt.Errorf("storage.hop_policy: %w", err)
	}
	log.Info("storage.hop_policy", "mode", cfg.Mode, "sample_every", sampleEvery)
	return p, nil
}
