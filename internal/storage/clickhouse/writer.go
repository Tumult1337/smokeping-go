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

// The flushes name their columns explicitly so a table that gained a column
// still accepts a batch built before it — a listless INSERT is positional and
// breaks the moment the two disagree.
const (
	insertProbeCycle = `INSERT INTO probe_cycle (timestamp, target_id, target_group, source, probe_type,
  sent, lost, loss_pct,
  rtt_min_us, rtt_max_us, rtt_mean_us, rtt_median_us, rtt_stddev_us,
  p5_us, p10_us, p15_us, p20_us, p25_us, p30_us, p35_us, p40_us, p45_us, p55_us,
  p60_us, p65_us, p70_us, p75_us, p80_us, p85_us, p90_us, p95_us)`
	insertProbeRTT = `INSERT INTO probe_rtt (timestamp, target_id, target_group, source, seq, rtt_ms)`
	insertProbeHop = `INSERT INTO probe_hop (timestamp, target_id, target_group, source, ttl, hop_addr, unreach, target_reply,
  sent, lost, loss_pct, rtt_min_us, rtt_max_us, rtt_mean_us, rtt_median_us)`
	insertProbeHTTP = `INSERT INTO probe_http (timestamp, target_id, target_group, source, seq, rtt_ms, status, error)`
)

// flushRetainFactor bounds the backlog a table buffer keeps across failed
// flushes. On a flush error the batch is retained for retry rather than
// dropped, but a prolonged ClickHouse outage must not grow `pending` without
// limit — so the retained backlog is capped at maxRows*flushRetainFactor,
// dropping (and counting) the oldest overflow. This mirrors the drop-oldest
// semantics of the slave push ring.
const flushRetainFactor = 4

// Channel sizing, in slots per table, bound once at startup. A flat 4096
// everywhere equalized nothing: a cycle produces ~1 cycle row, `pings` rtt
// rows and ~20 hop rows, so at the deployed 122-target/20s install a
// ClickHouse stall drank 11 minutes of cycle buffer against 96 seconds of hop
// buffer and hops died first. Scaling by rows-per-cycle equalizes
// time-to-overflow instead. hopRowFactor is clamp-limited on purpose: 4096×32
// is maxChanCap exactly, so any larger factor is inert, and the worst-case
// hop cycles (90 rows for icmp's 3 rounds × 30 TTLs, 300 for MTR's 10 × 30)
// still overflow first — drop-oldest and the drop counters are the bound
// there, not the buffer.
const (
	baseChanCap   = 4096
	maxChanCap    = 131072
	hopRowFactor  = 32
	httpRowFactor = 2
)

func writerChanCap(table, pings int) int {
	factor := 1
	switch table {
	case tableProbeRTT:
		factor = pings
	case tableProbeHop:
		factor = hopRowFactor
	case tableProbeHTTP:
		factor = httpRowFactor
	}
	return min(max(baseChanCap*factor, baseChanCap), maxChanCap)
}

// newWriterChans is the single construction point for the per-table buffers;
// NewWriter must build w.chans through it, never inline make().
func newWriterChans(pings int) [numTables]chan any {
	var chans [numTables]chan any
	for i := range chans {
		chans[i] = make(chan any, writerChanCap(i, pings))
	}
	return chans
}

// NewWriter opens a connection and starts one consumer goroutine per
// table, sizing each table's buffer from pings. Returns an error if the
// initial Ping fails.
func NewWriter(ctx context.Context, log *slog.Logger, cfg config.ClickHouse, pings int) (*Writer, error) {
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
	w := &Writer{log: log, conn: conn, cfg: cfg, cancel: cancel, chans: newWriterChans(pings)}

	maxInterval, _ := time.ParseDuration(cfg.Batch.MaxInterval) // validated at config-load
	for i := 0; i < numTables; i++ {
		w.wg.Add(1)
		go w.runTable(loopCtx, i, cfg.Batch.MaxRows, maxInterval)
	}
	return w, nil
}

// OnCycle decomposes a Cycle into rows for each relevant table. A cycle that
// sent nothing is left out of probe_cycle entirely: loss_pct has no defined
// value there, and storing the 0 it computes renders a healthy point over a
// gap the probe never filled. Hop rows still go — the TTL walk's own counters
// are real measurements.
func (w *Writer) OnCycle(ctx context.Context, c scheduler.Cycle) {
	if c.Sent > 0 {
		w.offer(tableProbeCycle, c)
	} else {
		w.logNoMeasurement(c)
	}
	for i, rtt := range c.RTTs {
		w.offer(tableProbeRTT, rttRow{
			ts: c.Time, target: c.Target.Target.Name, group: c.Target.Group, source: c.Source,
			seq: uint16(i), rttMS: rttMS(rtt),
		})
	}
	for _, hop := range c.Hops {
		w.offer(tableProbeHop, hopRow{
			ts: c.Time, target: c.Target.Target.Name, group: c.Target.Group, source: c.Source,
			hop: hop,
		})
	}
	for i, s := range c.HTTPSamples {
		w.offer(tableProbeHTTP, httpRow{
			ts: s.Time, target: c.Target.Target.Name, group: c.Target.Group, source: c.Source,
			seq: uint16(i), rttMS: float64(s.RTT) / float64(time.Millisecond),
			status: uint16(s.Status), err: s.Err,
		})
	}
}

// logNoMeasurement surfaces the skipped cycle, so the resulting chart gap and
// silent alert have a reason in the log rather than looking like a lost write.
func (w *Writer) logNoMeasurement(c scheduler.Cycle) {
	if w.log == nil {
		return
	}
	w.log.Warn("clickhouse.writer.no_measurement",
		"target", c.Target.ID(), "probe", c.ProbeName, "source", c.Source)
}

type rttRow struct {
	ts                    time.Time
	target, group, source string
	seq                   uint16
	rttMS                 float64
}

type hopRow struct {
	ts                    time.Time
	target, group, source string
	hop                   probe.Hop
}

type httpRow struct {
	ts                    time.Time
	target, group, source string
	seq                   uint16
	rttMS                 float64
	status                uint16
	err                   string
}

// rttMS converts a time.Duration to milliseconds for the Float64 rtt_ms
// column, clamping negatives to 0 like durUS does; cluster ingest accepts an
// RTT of exactly 0, and the NaN this returned for it broke /rtts' JSON
// encoding after the 200 header was already written.
func rttMS(d time.Duration) float64 {
	if d < 0 {
		return 0
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
	batch, err := w.conn.PrepareBatch(ctx, insertProbeCycle)
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
			durUS(s.Min), durUS(s.Max), durUS(s.Mean), durUS(s.Median), durUS(s.StdDev),
			durUS(s.P5), durUS(s.P10), durUS(s.P15), durUS(s.P20), durUS(s.P25),
			durUS(s.P30), durUS(s.P35), durUS(s.P40), durUS(s.P45), durUS(s.P55),
			durUS(s.P60), durUS(s.P65), durUS(s.P70), durUS(s.P75), durUS(s.P80),
			durUS(s.P85), durUS(s.P90), durUS(s.P95),
		)
		if err != nil {
			return err
		}
	}
	return batch.Send()
}

func boolToUint8(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

// durUS converts a duration to microseconds for the UInt32 latency columns
// in probe_cycle / probe_hop. Zero/negative maps to 0 (matching the all-zero
// Summary a 100%-loss cycle produces) and the value is clamped to the UInt32
// range so a pathologically large reading can't wrap around.
func durUS(d time.Duration) uint32 {
	if d <= 0 {
		return 0
	}
	us := math.Round(float64(d) / float64(time.Microsecond))
	if us >= math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(us)
}

func (w *Writer) flushRTTs(ctx context.Context, rows []any) error {
	batch, err := w.conn.PrepareBatch(ctx, insertProbeRTT)
	if err != nil {
		return err
	}
	for _, raw := range rows {
		r := raw.(rttRow)
		if err := batch.Append(r.ts, r.target, r.group, r.source, r.seq, r.rttMS); err != nil {
			return err
		}
	}
	return batch.Send()
}

func (w *Writer) flushHops(ctx context.Context, rows []any) error {
	batch, err := w.conn.PrepareBatch(ctx, insertProbeHop)
	if err != nil {
		return err
	}
	// MinMaxMeanMedian sorts in place and OnCycle queues a shallow probe.Hop,
	// so r.hop.RTTs still aliases the Cycle every other sink holds: sort a
	// copy in a buffer reused across the batch, which keeps the hot probe path
	// allocation-free rather than cloning per hop at queue time.
	var sorted []time.Duration
	for _, raw := range rows {
		r := raw.(hopRow)
		sorted = append(sorted[:0], r.hop.RTTs...)
		hMin, hMax, hMean, hMedian := stats.MinMaxMeanMedian(sorted)
		lossPct := float32(0)
		if r.hop.Sent > 0 {
			lossPct = float32(100 * float64(r.hop.Lost) / float64(r.hop.Sent))
		}
		err := batch.Append(
			r.ts,
			r.target,
			r.group,
			r.source,
			uint8(r.hop.Index),
			r.hop.IP,
			r.hop.Unreach,
			boolToUint8(r.hop.TargetReply),
			uint16(r.hop.Sent),
			uint16(r.hop.Lost),
			lossPct,
			durUS(hMin), durUS(hMax), durUS(hMean), durUS(hMedian),
		)
		if err != nil {
			return err
		}
	}
	return batch.Send()
}

func (w *Writer) flushHTTP(ctx context.Context, rows []any) error {
	batch, err := w.conn.PrepareBatch(ctx, insertProbeHTTP)
	if err != nil {
		return err
	}
	for _, raw := range rows {
		r := raw.(httpRow)
		if err := batch.Append(r.ts, r.target, r.group, r.source, r.seq, r.rttMS, r.status, r.err); err != nil {
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
