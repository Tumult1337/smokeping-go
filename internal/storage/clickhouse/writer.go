package clickhouse

import (
	"context"
	"crypto/tls"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/scheduler"
)

// Table identifiers used for the per-table channel + drop-counter index.
const (
	tableProbeCycle = iota
	tableProbeRTT
	tableProbeHop
	tableProbeHTTP
	numTables
)

// Writer is the storage.Writer / scheduler.Sink implementation backed by
// ClickHouse. One goroutine per table consumes from a buffered channel
// and flushes to a clickhouse.Batch on either max-rows or ticker.
type Writer struct {
	log     *slog.Logger
	conn    driver.Conn
	cfg     config.ClickHouse
	chans   [numTables]chan any
	dropped [numTables]uint64
	wg      sync.WaitGroup
	cancel  context.CancelFunc
}

// NewWriter opens a connection and starts one consumer goroutine per
// table. Returns an error if the initial Ping fails.
func NewWriter(ctx context.Context, log *slog.Logger, cfg config.ClickHouse) (*Writer, error) {
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{cfg.Addr},
		Auth: clickhouse.Auth{Database: cfg.Database, Username: cfg.Username, Password: cfg.Password},
		TLS:  tlsForWriter(cfg.TLS),
		Settings: clickhouse.Settings{
			"async_insert":          0,
			"wait_for_async_insert": 0,
		},
		MaxOpenConns:    runtime.GOMAXPROCS(0),
		BlockBufferSize: 10,
		Compression: &clickhouse.Compression{
			Method: clickhouse.CompressionLZ4,
		},
	})
	if err != nil {
		return nil, err
	}
	if err := conn.Ping(ctx); err != nil {
		conn.Close()
		return nil, err
	}

	loopCtx, cancel := context.WithCancel(context.Background())
	w := &Writer{log: log, conn: conn, cfg: cfg, cancel: cancel}
	for i := range w.chans {
		w.chans[i] = make(chan any, 4096)
	}

	maxInterval, _ := time.ParseDuration(cfg.Batch.MaxInterval) // validated at config-load
	for i := 0; i < numTables; i++ {
		w.wg.Add(1)
		go w.runTable(loopCtx, i, cfg.Batch.MaxRows, maxInterval)
	}
	return w, nil
}

// OnCycle implements scheduler.Sink. The scaffold version routes every
// cycle to the probe_cycle channel only — T9 expands to fan out RTTs,
// Hops, and HTTPSamples to their respective tables.
func (w *Writer) OnCycle(ctx context.Context, c scheduler.Cycle) {
	w.offer(tableProbeCycle, c)
}

// offer is the drop-on-overflow primitive used by OnCycle.
func (w *Writer) offer(table int, row any) {
	select {
	case w.chans[table] <- row:
	default:
		atomic.AddUint64(&w.dropped[table], 1)
	}
}

// runTable consumes rows and flushes on max-rows or ticker. Per-table
// batch population is implemented in T9 (probe_cycle) and T10 (RTT, hop,
// HTTP); for this scaffold, flush is a no-op.
func (w *Writer) runTable(ctx context.Context, table, maxRows int, maxInterval time.Duration) {
	defer w.wg.Done()
	ticker := time.NewTicker(maxInterval)
	defer ticker.Stop()

	pending := make([]any, 0, maxRows)
	flush := func() {
		if len(pending) == 0 {
			return
		}
		// Per-table batch flush filled in by T9/T10.
		pending = pending[:0]
	}

	for {
		select {
		case <-ctx.Done():
			flush()
			return
		case row := <-w.chans[table]:
			pending = append(pending, row)
			if len(pending) >= maxRows {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// Close stops the consumers, drains pending rows, and closes the conn.
func (w *Writer) Close() error {
	if w.cancel != nil {
		w.cancel()
	}
	w.wg.Wait()
	if w.conn != nil {
		return w.conn.Close()
	}
	return nil
}

func tlsForWriter(enabled bool) *tls.Config {
	if !enabled {
		return nil
	}
	return &tls.Config{MinVersion: tls.VersionTLS12}
}
