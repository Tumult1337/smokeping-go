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
	closed  atomic.Bool
}

// dropLogEvery is how often the writer surfaces sustained overflow to the
// log. The first drop after Close (or per restart) always logs; after that
// it's one line per N drops so a saturating channel doesn't flood the log.
const dropLogEvery = 10000

// flushRetainFactor bounds the backlog a table buffer keeps across failed
// flushes. On a flush error the batch is retained for retry rather than
// dropped, but a prolonged ClickHouse outage must not grow `pending` without
// limit — so the retained backlog is capped at maxRows*flushRetainFactor,
// dropping (and counting) the oldest overflow. This mirrors the drop-oldest
// semantics of the slave push ring.
const flushRetainFactor = 4

// NewWriter opens a connection and starts one consumer goroutine per
// table. Returns an error if the initial Ping fails.
func NewWriter(ctx context.Context, log *slog.Logger, cfg config.ClickHouse) (*Writer, error) {
	// Pool must be at least numTables so all four flushers can run
	// concurrently on the ticker without queueing on the connection
	// pool. On larger hosts GOMAXPROCS wins; on 1-2 vCPU containers
	// numTables (4) wins.
	maxConns := max(runtime.GOMAXPROCS(0), numTables)
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{cfg.Addr},
		Auth: clickhouse.Auth{Database: cfg.Database, Username: cfg.Username, Password: cfg.Password},
		TLS:  tlsForWriter(cfg.TLS),
		Settings: clickhouse.Settings{
			"async_insert":          0,
			"wait_for_async_insert": 0,
		},
		MaxOpenConns:    maxConns,
		BlockBufferSize: 10,
		Compression: &clickhouse.Compression{
			Method: clickhouse.CompressionLZ4,
		},
	})
	if err != nil {
		return nil, err
	}
	if err := conn.Ping(ctx); err != nil {
		conn.Close() //nolint:errcheck // best-effort cleanup after ping failure
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
// zero / negative durations as a defensive guard — the current caller
// only iterates scheduler.Cycle.RTTs, which the probes populate with
// successful pings only (lost pings never reach this function). If
// future schema changes start carrying per-ping loss markers through
// this path, NaN preserves unambiguous "no response" semantics against
// legitimate sub-millisecond readings.
func rttMS(d time.Duration) float64 {
	if d <= 0 {
		return math.NaN()
	}
	return float64(d) / float64(time.Millisecond)
}

// offer is the drop-on-overflow primitive used by OnCycle. Rows offered
// after Close are dropped immediately and counted — the channel is still
// open at that point but its consumer goroutines have exited, so a naive
// send would queue forever-unflushed bytes with no observability.
func (w *Writer) offer(table int, row any) {
	if w.closed.Load() {
		w.recordDrop(table)
		return
	}
	select {
	case w.chans[table] <- row:
	default:
		w.recordDrop(table)
	}
}

// recordDrop increments the per-table drop counter and emits a log line
// on the first drop and every dropLogEvery drops after. Log emission is
// guarded by a modulo check on the atomic-incremented counter so it stays
// allocation-free under steady-state.
func (w *Writer) recordDrop(table int) {
	n := atomic.AddUint64(&w.dropped[table], 1)
	if w.log == nil {
		return
	}
	if n == 1 || n%dropLogEvery == 0 {
		w.log.Warn("clickhouse.writer.drop",
			"table", tableName(table),
			"dropped_total", n,
		)
	}
}

// Dropped returns the per-table dropped-row counts. Safe to call
// concurrently with writes; values are eventually consistent.
func (w *Writer) Dropped() map[string]uint64 {
	out := make(map[string]uint64, numTables)
	for i := 0; i < numTables; i++ {
		out[tableName(i)] = atomic.LoadUint64(&w.dropped[i])
	}
	return out
}

func (w *Writer) runTable(ctx context.Context, table, maxRows int, maxInterval time.Duration) {
	defer w.wg.Done()
	ticker := time.NewTicker(maxInterval)
	defer ticker.Stop()

	pending := make([]any, 0, maxRows)
	// flushFailing is set while the last flush errored. It gates the
	// size-triggered flush below so a sustained outage retries only on the
	// ticker tick, not on every incoming row (which would fire a blocking dial
	// per row once pending stays ≥ maxRows — a retry storm).
	flushFailing := false
	flush := func(flushCtx context.Context) {
		if len(pending) == 0 {
			flushFailing = false
			return
		}
		var err error
		switch table {
		case tableProbeCycle:
			err = w.flushCycles(flushCtx, pending)
		case tableProbeRTT:
			err = w.flushRTTs(flushCtx, pending)
		case tableProbeHop:
			err = w.flushHops(flushCtx, pending)
		case tableProbeHTTP:
			err = w.flushHTTP(flushCtx, pending)
		}
		if err != nil {
			flushFailing = true
			w.log.Warn("clickhouse.flush", "table", tableName(table), "err", err, "rows", len(pending))
			// Retain the batch for retry on the next ticker tick instead of
			// dropping it on a transient ClickHouse hiccup. Bound the backlog so
			// a long outage can't grow pending without limit: keep the newest
			// maxRows*flushRetainFactor rows, dropping the oldest overflow.
			if maxRetain := maxRows * flushRetainFactor; len(pending) > maxRetain {
				over := len(pending) - maxRetain
				pending = append(pending[:0], pending[over:]...)
				for range over {
					w.recordDrop(table)
				}
			}
			return
		}
		flushFailing = false
		pending = pending[:0]
	}
	for {
		select {
		case <-ctx.Done():
			// Drain the channel and then flush with a fresh context.
			// The shutdown context is already cancelled so any batch
			// using it would fail immediately; context.Background() with
			// a timeout ensures in-flight rows reach ClickHouse.
			for {
				select {
				case row := <-w.chans[table]:
					pending = append(pending, row)
				default:
					goto drainDone
				}
			}
		drainDone:
			drainCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			flush(drainCtx)
			cancel()
			return
		case row := <-w.chans[table]:
			pending = append(pending, row)
			// While a flush is failing, defer to the ticker for retries so we
			// don't dial ClickHouse on every row during an outage.
			if len(pending) >= maxRows && !flushFailing {
				flush(ctx)
			}
		case <-ticker.C:
			flush(ctx)
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
		// MinMaxMeanMedian sorts r.hop.RTTs in place. Safe here because
		// the writer owns this hopRow exclusively (it was queued by us
		// via offer() and no other consumer holds the slice). Avoids the
		// 17 wasted percentile computations + Clone of stats.Compute.
		hMin, hMax, hMean, hMedian := stats.MinMaxMeanMedian(r.hop.RTTs)
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
			durMS(hMin), durMS(hMax), durMS(hMean), durMS(hMedian),
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
// Idempotent: calling Close twice is safe and the second call is a no-op.
// After Close returns, further OnCycle calls increment the drop counter
// instead of queuing rows into a channel with no reader.
func (w *Writer) Close() error {
	if !w.closed.CompareAndSwap(false, true) {
		return nil
	}
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
