// Package prom is a placeholder for the Prometheus remote-write storage
// backend. It would push probe results via remote_write and read them
// back through PromQL on the same endpoint (or a federated Thanos /
// Mimir). Not implemented yet — Open returns ErrBackendNotImplemented so
// a misconfigured install fails loud at startup.
package prom

import (
	"context"
	"errors"
	"log/slog"

	"github.com/tumult/gosmokeping/internal/config"
)

// ErrBackendNotImplemented mirrors storage.ErrBackendNotImplemented. Kept
// private-to-package for the same reason as the influxv3 stub.
var ErrBackendNotImplemented = errors.New("storage/prom: backend not yet implemented")

// Open is the factory entry point. Always returns ErrBackendNotImplemented.
func Open(_ context.Context, _ *slog.Logger, _ config.Prometheus) (any, error) {
	return nil, ErrBackendNotImplemented
}
