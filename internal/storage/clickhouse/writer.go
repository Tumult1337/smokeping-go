package clickhouse

import (
	"context"
	"crypto/tls"
	"log/slog"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/probe"
	"github.com/tumult/gosmokeping/internal/scheduler"
	"github.com/tumult/gosmokeping/internal/stats"
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

// OnCycle decomposes a Cycle into rows for each relevant table.
func (w *Writer) OnCycle(ctx context.Context, c scheduler.Cycle) {
	w.offer(tableProbeCycle, c)
	for i, rtt := range c.RTTs {
		w.offer(tableProbeRTT, rttRow{
			ts: c.Time, target: c.Target.Target.Name, source: c.Source,
			seq: uint16(i), rttMS: rttMS(rtt),
		})
	}
	for _, hop := range c.Hops {
		w.offer(tableProbeHop, hopRow{cycle: c, hop: hop})
	}
	for i, s := range c.HTTPSamples {
		w.offer(tableProbeHTTP, httpRow{
			ts: s.Time, target: c.Target.Target.Name, source: c.Source,
			seq: uint16(i), rttMS: float64(s.RTT) / float64(time.Millisecond),
			status: uint16(s.Status), err: s.Err,
		})
	}
}

type rttRow struct {
	ts             time.Time
	target, source string
	seq            uint16
	rttMS          float64
}

type hopRow struct {
	cycle scheduler.Cycle
	hop   probe.Hop
}

type httpRow struct {
	ts             time.Time
	target, source string
	seq            uint16
	rttMS          float64
	status         uint16
	err            string
}

// rttMS converts a time.Duration to milliseconds. Returns NaN for
// zero / negative durations so unanswered pings (Duration == 0) don't
// pollute aggregations as legitimate zero-ms responses.
func rttMS(d time.Duration) float64 {
	if d <= 0 {
		return math.NaN()
	}
	return float64(d) / float64(time.Millisecond)
}

// offer is the drop-on-overflow primitive used by OnCycle.
func (w *Writer) offer(table int, row any) {
	select {
	case w.chans[table] <- row:
	default:
		atomic.AddUint64(&w.dropped[table], 1)
	}
}

func (w *Writer) runTable(ctx context.Context, table, maxRows int, maxInterval time.Duration) {
	defer w.wg.Done()
	ticker := time.NewTicker(maxInterval)
	defer ticker.Stop()

	pending := make([]any, 0, maxRows)
	flush := func() {
		if len(pending) == 0 {
			return
		}
		var err error
		switch table {
		case tableProbeCycle:
			err = w.flushCycles(ctx, pending)
		case tableProbeRTT:
			err = w.flushRTTs(ctx, pending)
		case tableProbeHop:
			err = w.flushHops(ctx, pending)
		case tableProbeHTTP:
			err = w.flushHTTP(ctx, pending)
		}
		if err != nil {
			w.log.Warn("clickhouse.flush", "table", tableName(table), "err", err, "rows", len(pending))
		}
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

func tableName(t int) string {
	return [...]string{"probe_cycle", "probe_rtt", "probe_hop", "probe_http"}[t]
}

func (w *Writer) flushCycles(ctx context.Context, rows []any) error {
	batch, err := w.conn.PrepareBatch(ctx, "INSERT INTO probe_cycle")
	if err != nil {
		return err
	}
	for _, raw := range rows {
		c := raw.(scheduler.Cycle)
		s := c.Summary
		sentF := float64(c.Sent)
		lossPct := float32(0)
		if c.Sent > 0 {
			lossPct = float32(100 * float64(c.LossCount) / sentF)
		}
		err := batch.Append(
			c.Time,
			c.Target.Target.Name,
			c.Target.Group,
			c.Source,
			c.ProbeName,
			uint16(c.Sent),
			uint16(c.LossCount),
			lossPct,
			durMS(s.Min), durMS(s.Max), durMS(s.Mean), durMS(s.Median), durMS(s.StdDev),
			durMS(s.P5), durMS(s.P10), durMS(s.P15), durMS(s.P20), durMS(s.P25),
			durMS(s.P30), durMS(s.P35), durMS(s.P40), durMS(s.P45), durMS(s.P55),
			durMS(s.P60), durMS(s.P65), durMS(s.P70), durMS(s.P75), durMS(s.P80),
			durMS(s.P85), durMS(s.P90), durMS(s.P95),
		)
		if err != nil {
			return err
		}
	}
	return batch.Send()
}

func durMS(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

func (w *Writer) flushRTTs(ctx context.Context, rows []any) error {
	batch, err := w.conn.PrepareBatch(ctx, "INSERT INTO probe_rtt")
	if err != nil {
		return err
	}
	for _, raw := range rows {
		r := raw.(rttRow)
		if err := batch.Append(r.ts, r.target, r.source, r.seq, r.rttMS); err != nil {
			return err
		}
	}
	return batch.Send()
}

func (w *Writer) flushHops(ctx context.Context, rows []any) error {
	batch, err := w.conn.PrepareBatch(ctx, "INSERT INTO probe_hop")
	if err != nil {
		return err
	}
	for _, raw := range rows {
		r := raw.(hopRow)
		summary := stats.Compute(r.hop.RTTs)
		lossPct := float32(0)
		if r.hop.Sent > 0 {
			lossPct = float32(100 * float64(r.hop.Lost) / float64(r.hop.Sent))
		}
		err := batch.Append(
			r.cycle.Time,
			r.cycle.Target.Target.Name,
			r.cycle.Source,
			uint8(r.hop.Index),
			r.hop.IP,
			uint16(r.hop.Sent),
			uint16(r.hop.Lost),
			lossPct,
			durMS(summary.Min), durMS(summary.Max), durMS(summary.Mean), durMS(summary.Median),
		)
		if err != nil {
			return err
		}
	}
	return batch.Send()
}

func (w *Writer) flushHTTP(ctx context.Context, rows []any) error {
	batch, err := w.conn.PrepareBatch(ctx, "INSERT INTO probe_http")
	if err != nil {
		return err
	}
	for _, raw := range rows {
		r := raw.(httpRow)
		if err := batch.Append(r.ts, r.target, r.source, r.seq, r.rttMS, r.status, r.err); err != nil {
			return err
		}
	}
	return batch.Send()
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
