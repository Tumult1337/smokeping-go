package clickhouse

import (
	"context"
	"crypto/tls"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/storage"
)

// Reader implements storage.Reader against ClickHouse.
type Reader struct {
	conn driver.Conn
}

// NewReader opens a connection. Caller must Close.
func NewReader(ctx context.Context, cfg config.ClickHouse) (*Reader, error) {
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{cfg.Addr},
		Auth: clickhouse.Auth{Database: cfg.Database, Username: cfg.Username, Password: cfg.Password},
		TLS:  tlsForReader(cfg.TLS),
	})
	if err != nil {
		return nil, err
	}
	if err := conn.Ping(ctx); err != nil {
		conn.Close()
		return nil, err
	}
	return &Reader{conn: conn}, nil
}

func (r *Reader) Close() error { return r.conn.Close() }

func tlsForReader(enabled bool) *tls.Config {
	if !enabled {
		return nil
	}
	return &tls.Config{MinVersion: tls.VersionTLS12}
}

// pickCycleStep returns the toStartOfInterval width to use for a cycle
// query covering `span`. Returns 0 to mean "no bucketing, return raw rows".
// Tiers: ≤24h raw, ≤180d 1h, >180d 1d.
func pickCycleStep(span time.Duration) time.Duration {
	switch {
	case span <= 24*time.Hour:
		return 0
	case span <= 180*24*time.Hour:
		return time.Hour
	default:
		return 24 * time.Hour
	}
}

// pickHopStep returns the toStartOfInterval width for hop timeline queries.
// Tiers: ≤24h raw, >24h 15m.
func pickHopStep(span time.Duration) time.Duration {
	if span <= 24*time.Hour {
		return 0
	}
	return 15 * time.Minute
}

// QueryCycles, QueryRTTs, QueryHTTPSamples, QueryLatestHops, QueryHopsAt,
// QueryHopsTimeline are implemented in Tasks 12–15.
func (r *Reader) QueryCycles(ctx context.Context, ref config.TargetRef, from, to time.Time, f storage.QueryFilter) ([]storage.CyclePoint, error) {
	return nil, fmt.Errorf("not implemented")
}
func (r *Reader) QueryRTTs(ctx context.Context, ref config.TargetRef, from, to time.Time, f storage.QueryFilter) ([]storage.RTTPoint, error) {
	return nil, fmt.Errorf("not implemented")
}
func (r *Reader) QueryHTTPSamples(ctx context.Context, ref config.TargetRef, from, to time.Time, f storage.QueryFilter) ([]storage.HTTPPoint, error) {
	return nil, fmt.Errorf("not implemented")
}
func (r *Reader) QueryLatestHops(ctx context.Context, ref config.TargetRef, f storage.QueryFilter) ([]storage.HopPoint, error) {
	return nil, fmt.Errorf("not implemented")
}
func (r *Reader) QueryHopsAt(ctx context.Context, ref config.TargetRef, at time.Time, window time.Duration, f storage.QueryFilter) ([]storage.HopPoint, error) {
	return nil, fmt.Errorf("not implemented")
}
func (r *Reader) QueryHopsTimeline(ctx context.Context, ref config.TargetRef, from, to time.Time, f storage.QueryFilter) ([]storage.HopPoint, error) {
	return nil, fmt.Errorf("not implemented")
}
