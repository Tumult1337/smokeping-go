// Package influxv3 is the InfluxDB v3 storage backend. Writes go through the
// official influxdb3-go client (HTTP /api/v3/write_lp under the hood); reads
// go through Apache Arrow Flight via the same client. The line-protocol shape
// (measurements, tags, fields) is identical to the v2 backend so the two can
// be swapped behind the same scheduler.Sink + storage.Reader interfaces with
// no UI or alert-evaluator changes.
//
// Resolution tiering does NOT use pre-rolled buckets here — v3 has no Flux
// task equivalent. A single database holds raw cycles, and the Reader maps a
// requested storage.Resolution into a SQL `date_bin()` width at query time.
// v3's columnar Parquet storage and DataFusion engine make wide aggregations
// cheap in a way v2's TSM was not, which is why this backend is the right
// pick for clusters with many slaves piling write throughput onto one master.
package influxv3

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/InfluxCommunity/influxdb3-go/v2/influxdb3"
	"github.com/InfluxCommunity/influxdb3-go/v2/influxdb3/batching"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/scheduler"
	"github.com/tumult/gosmokeping/internal/stats"
	"github.com/tumult/gosmokeping/internal/storage"
)

// batcher tunables. The batching package fires the emit callback on size
// alone, which is wrong for our shape — a quiet single-target install
// would buffer one cycle indefinitely waiting for 999 more points. We
// add a time-based flusher on top so a stalled probe never holds the last
// cycle in RAM forever.
const (
	batchSize     = 1000
	batchInterval = 1 * time.Second

	// cycleQueueCap matches v2: OnCycle is called synchronously by every
	// per-target scheduler goroutine, and the batcher's Add path emits
	// synchronously under its own lock when the batch fills — so a slow
	// v3 write would otherwise freeze every probing goroutine. With a
	// queue here OnCycle stays non-blocking and the freshest cycle wins
	// when storage is sick.
	cycleQueueCap = 1024

	// droppedReportInterval limits how often we log accumulated drops so
	// a long outage doesn't fill the log.
	droppedReportInterval = time.Minute
)

const (
	measurementCycle = "probe_cycle"
	measurementRTT   = "probe_rtt"
	measurementHop   = "probe_mtr_hop"
	measurementHTTP  = "probe_http"
)

// Writer streams completed cycles into InfluxDB v3. Implements scheduler.Sink.
// One cycle expands to (1 + N + M) points: the cycle aggregate, plus per-ping
// RTT rows OR per-request HTTP rows (mutually exclusive — see comment in
// OnCycle), plus one row per MTR hop. The point shape mirrors the v2 backend
// verbatim so a v2→v3 migration is a config flip, not a schema rewrite.
//
// Writes flow through an internal cycle queue → a single drainLoop goroutine
// → the official batching.Batcher → the v3 client. OnCycle is non-blocking:
// it enqueues a cycle and returns. The drainLoop owns all batcher access
// (Add and Flush both happen on this one goroutine, so the batcher's own
// lock-free Flush never races against Add), and is the only goroutine that
// can be stalled by a slow v3 write — the scheduler stays on cadence.
// When the queue saturates because Influx is sick, OnCycle drops the
// oldest queued cycle so the freshest data wins.
type Writer struct {
	log       *slog.Logger
	client    *influxdb3.Client
	batcher   *batching.Batcher
	hopPolicy *storage.HopPolicy

	queue   chan scheduler.Cycle
	stop    chan struct{}
	done    chan struct{}
	closed  sync.Once
	dropped atomic.Int64
}

// NewWriter constructs a Writer backed by a v3 client. The auth scheme is
// pinned to "Bearer" because every InfluxDB 3 deployment we target (Core OSS,
// Enterprise, Cloud Dedicated) speaks the v3 API with a bearer token; the
// client's empty-default would otherwise emit "Token <secret>" which only
// v2-compat endpoints accept.
func NewWriter(log *slog.Logger, cfg config.InfluxV3, policy *storage.HopPolicy) (*Writer, error) {
	c, err := influxdb3.New(influxdb3.ClientConfig{
		Host:       cfg.URL,
		Token:      cfg.Token,
		Database:   cfg.Database,
		AuthScheme: "Bearer",
	})
	if err != nil {
		return nil, fmt.Errorf("influxv3 client: %w", err)
	}
	w := &Writer{
		log:       log,
		client:    c,
		hopPolicy: policy,
		queue:     make(chan scheduler.Cycle, cycleQueueCap),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	w.batcher = batching.NewBatcher(
		batching.WithSize(batchSize),
		batching.WithEmitCallback(w.flush),
	)
	go w.drainLoop()
	return w, nil
}

// drainLoop is the single owner of batcher access. It pulls cycles off the
// queue (calls Add, which may block when the batch fills and triggers a
// synchronous emit) and periodically Flushes so a quiet install doesn't
// hold the last cycle in RAM forever. Doing both on one goroutine is what
// makes the lock-free batching.Flush safe in our usage.
func (w *Writer) drainLoop() {
	defer close(w.done)
	tick := time.NewTicker(batchInterval)
	defer tick.Stop()
	report := time.NewTicker(droppedReportInterval)
	defer report.Stop()
	var lastReported int64
	for {
		select {
		case c := <-w.queue:
			w.addToBatcher(c)
		case <-tick.C:
			if pts := w.batcher.Flush(); len(pts) > 0 {
				w.flush(pts)
			}
		case <-report.C:
			if cur := w.dropped.Load(); cur > lastReported {
				w.log.Warn("dropped cycles under storage backpressure",
					"since_last_report", cur-lastReported,
					"total", cur)
				lastReported = cur
			}
		case <-w.stop:
			// Best-effort drain: stage everything still queued, then flush.
			for {
				select {
				case c := <-w.queue:
					w.addToBatcher(c)
				default:
					if pts := w.batcher.Flush(); len(pts) > 0 {
						w.flush(pts)
					}
					return
				}
			}
		}
	}
}

// flush is the batcher's emit callback. We intentionally use a fresh,
// untimed background context here: the cycle's caller-context might already
// be cancelled by the time the batch fires (we returned from OnCycle long
// before the size threshold tripped), and the v3 client's WriteTimeout
// already bounds the HTTP call.
func (w *Writer) flush(points []*influxdb3.Point) {
	if len(points) == 0 {
		return
	}
	if err := w.client.WritePoints(context.Background(), points); err != nil {
		w.log.Warn("influxv3 write", "err", err, "points", len(points))
	}
}

// Close stops the flush goroutine, drains any pending points, and releases
// the underlying client. Safe to call multiple times.
func (w *Writer) Close() {
	w.closed.Do(func() {
		close(w.stop)
		<-w.done
		if pts := w.batcher.Flush(); len(pts) > 0 {
			w.flush(pts)
		}
		if w.client != nil {
			_ = w.client.Close()
		}
	})
}

// OnCycle satisfies scheduler.Sink. MUST NOT block — the per-target
// scheduler goroutine calls this synchronously. We enqueue and let the
// drainLoop do the (potentially slow) batcher Add and HTTP write. If the
// queue is full because storage is stuck, drop the oldest cycle so
// monitoring keeps producing fresh data instead of freezing every target.
func (w *Writer) OnCycle(_ context.Context, c scheduler.Cycle) {
	for {
		select {
		case w.queue <- c:
			return
		default:
		}
		select {
		case <-w.queue:
			w.dropped.Add(1)
		default:
			// Drained by a concurrent producer; loop and retry the send.
		}
	}
}

// addToBatcher turns a cycle into points and stages them. Called only from
// drainLoop, so a slow batcher.Add (which may trigger a synchronous emit
// when the batch fills) stalls the drainLoop, never the scheduler.
func (w *Writer) addToBatcher(c scheduler.Cycle) {
	tags := map[string]string{
		"target": c.Target.Target.Name,
		"group":  c.Target.Group,
		"probe":  c.ProbeName,
	}
	// Omit the source tag when empty so pre-cluster data keeps writing to the
	// same series it always did — an explicit "" tag would create a new one.
	if c.Source != "" {
		tags["source"] = c.Source
	}

	lossPct := 0.0
	if c.Sent > 0 {
		lossPct = 100 * float64(c.LossCount) / float64(c.Sent)
	}

	cycleFields := map[string]any{
		"loss_pct":   lossPct,
		"loss_count": c.LossCount,
		"pings_sent": c.Sent,
	}
	if len(c.RTTs) > 0 {
		cycleFields["rtt_min"] = ms(c.Summary.Min)
		cycleFields["rtt_max"] = ms(c.Summary.Max)
		cycleFields["rtt_mean"] = ms(c.Summary.Mean)
		cycleFields["rtt_median"] = ms(c.Summary.Median)
		cycleFields["rtt_stddev"] = ms(c.Summary.StdDev)
		for _, spec := range stats.PercentileSet {
			cycleFields["rtt_"+spec.Name] = ms(spec.Get(c.Summary))
		}
	}

	points := make([]*influxdb3.Point, 0, 1+len(c.RTTs)+len(c.HTTPSamples)+len(c.Hops))
	points = append(points, influxdb3.NewPoint(measurementCycle, tags, cycleFields, c.Time))

	// HTTP cycles get their own per-request measurement with status codes;
	// emitting probe_rtt on top would double-write the same latencies and
	// bloat storage for no UI benefit. Every other probe type uses probe_rtt
	// as the only per-sample record.
	if len(c.HTTPSamples) > 0 {
		for i, s := range c.HTTPSamples {
			ts := s.Time
			if ts.IsZero() {
				ts = c.Time.Add(time.Duration(i) * time.Millisecond)
			}
			fields := map[string]any{
				"rtt_ms":      ms(s.RTT),
				"status_code": s.Status,
				"seq":         i,
			}
			if s.Err != "" {
				fields["error"] = s.Err
			}
			points = append(points, influxdb3.NewPoint(measurementHTTP, tags, fields, ts))
		}
	} else {
		for i, rtt := range c.RTTs {
			// Spread individual samples by 1ms so they don't share a timestamp:
			// InfluxDB would otherwise overwrite points with identical
			// series+time, and v3 enforces this just like v2 did.
			ts := c.Time.Add(time.Duration(i) * time.Millisecond)
			points = append(points, influxdb3.NewPoint(
				measurementRTT,
				tags,
				map[string]any{"rtt_ms": ms(rtt), "seq": i},
				ts,
			))
		}
	}

	if w.hopPolicy.ShouldWrite(c) {
		for _, hop := range c.Hops {
			hopTags := map[string]string{
				"target":    c.Target.Target.Name,
				"group":     c.Target.Group,
				"probe":     c.ProbeName,
				"hop_index": strconv.Itoa(hop.Index),
			}
			if c.Source != "" {
				hopTags["source"] = c.Source
			}
			summary := stats.Compute(hop.RTTs)
			hopLoss := 0.0
			if hop.Sent > 0 {
				hopLoss = 100 * float64(hop.Lost) / float64(hop.Sent)
			}
			points = append(points, influxdb3.NewPoint(measurementHop, hopTags, map[string]any{
				"hop_ip":     hop.IP,
				"rtt_min":    ms(summary.Min),
				"rtt_max":    ms(summary.Max),
				"rtt_mean":   ms(summary.Mean),
				"rtt_median": ms(summary.Median),
				"loss_pct":   hopLoss,
				"loss_count": hop.Lost,
				"pings_sent": hop.Sent,
			}, c.Time))
		}
	}

	w.batcher.Add(points...)
}

func ms(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}
