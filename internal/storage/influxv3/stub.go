// Package influxv3 is a placeholder for the InfluxDB v3 storage backend.
// v3 uses databases (not buckets) and speaks Flight SQL / HTTP JSON —
// neither the writer nor the reader is implemented yet. Open always
// returns ErrBackendNotImplemented so a misconfigured install fails loud
// at startup instead of silently dropping data.
package influxv3

import (
	"context"
	"errors"
	"log/slog"

	"github.com/tumult/gosmokeping/internal/config"
)

// ErrBackendNotImplemented mirrors storage.ErrBackendNotImplemented. Kept
// private-to-package to avoid the stub depending on storage (which would
// create an import cycle once storage imports this).
var ErrBackendNotImplemented = errors.New("storage/influxv3: backend not yet implemented")

// Open is the factory entry point. Always returns
// ErrBackendNotImplemented today; when a writer/reader are added this
// will build a real backend from cfg.
func Open(_ context.Context, _ *slog.Logger, _ config.InfluxV3) (any, error) {
	return nil, ErrBackendNotImplemented
}
