// Package influxv2 is the InfluxDB v2 storage backend. It writes cycles via
// the v2 write API and queries them back via Flux; rollup buckets (1h, 1d)
// are populated by tasks this package installs in Bootstrap.
package influxv2

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/influxdata/influxdb-client-go/v2/api"
	"github.com/influxdata/influxdb-client-go/v2/api/write"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/scheduler"
	"github.com/tumult/gosmokeping/internal/stats"
)

const (
	measurementCycle = "probe_cycle"
	measurementRTT   = "probe_rtt"
	// measurementHop is one row per hop per MTR cycle. hop_ip is a field (not
	// tag) because routers along a path flap and we don't want a new series
	// every time the path changes.
	measurementHop = "probe_mtr_hop"
	// measurementHTTP is one row per HTTP request. status_code is a field (not
	// tag) to avoid series cardinality exploding on pages that cycle through
	// error codes.
	measurementHTTP = "probe_http"

	// cycleQueueCap bounds memory if Influx becomes unhealthy. OnCycle is
	// called synchronously by every per-target scheduler goroutine, and the
	// underlying WriteAPI.WritePoint blocks on its own bounded channel when
	// the influx-client-go background flusher can't keep up — which is
	// exactly what froze the whole scheduler when the disk filled. With a
	// queue here, OnCycle stays non-blocking and the freshest cycle wins
	// over the oldest when storage is sick.
	cycleQueueCap = 1024

	// droppedReportInterval limits how often we log accumulated drops so a
	// long outage doesn't fill the log; one summary per minute is enough to
	// see the problem in the logs without being noisy.
	droppedReportInterval = time.Minute
)

// Writer writes completed cycles to InfluxDB. Implements scheduler.Sink.
// Two points per cycle: a cycle-level aggregate in the raw bucket, plus one
// point per individual RTT (also in the raw bucket) so the UI can render
// the full smoke band at close range. The 1h/1d buckets are populated by
// rollup tasks installed in Bootstrap.
//
// OnCycle is non-blocking: cycles land on an internal bounded queue and a
// single goroutine drains it into the v2 write API. When Influx is sick the
// write side stalls and our queue fills; we then drop the oldest cycle to
// make room for the new one, so monitoring keeps producing fresh data
// instead of freezing every probing goroutine in lockstep.
type Writer struct {
	log     *slog.Logger
	client  influxdb2.Client
	write   api.WriteAPI
	cfg     config.InfluxV2
	queue   chan scheduler.Cycle
	stop    chan struct{}
	wg      sync.WaitGroup
	dropped atomic.Int64
}

// NewWriter constructs a Writer backed by a new v2 client. The caller must
// Close the returned Writer on shutdown to flush buffered writes.
func NewWriter(log *slog.Logger, cfg config.InfluxV2) *Writer {
	client := influxdb2.NewClient(cfg.URL, cfg.Token)
	wa := client.WriteAPI(cfg.Org, cfg.BucketRaw)
	w := &Writer{
		log:    log,
		client: client,
		write:  wa,
		cfg:    cfg,
		queue:  make(chan scheduler.Cycle, cycleQueueCap),
		stop:   make(chan struct{}),
	}
	// Log async write errors instead of silently dropping them.
	go func() {
		for err := range wa.Errors() {
			log.Warn("influx async write", "err", err)
		}
	}()
	w.wg.Add(1)
	go w.flushLoop()
	return w
}

// Close flushes pending writes and releases the client. Best-effort: the
// flush goroutine drains whatever's still in the queue before we tell the
// underlying client to flush its own buffers and shut down.
func (w *Writer) Close() {
	close(w.stop)
	w.wg.Wait()
	if w.write != nil {
		w.write.Flush()
	}
	if w.client != nil {
		w.client.Close()
	}
}

// OnCycle satisfies scheduler.Sink. MUST NOT block — the per-target
// goroutines in scheduler.loopTarget call this synchronously and the next
// tick on that target can't fire until we return. We enqueue and let the
// flushLoop do the (potentially slow) WritePoint calls; if the queue is
// full because storage is stuck, drop the oldest cycle to make room.
func (w *Writer) OnCycle(_ context.Context, c scheduler.Cycle) {
	for {
		select {
		case w.queue <- c:
			return
		default:
		}
		// Queue full. Drain one oldest cycle to make room and retry.
		// Concurrent enqueuers may race; the loop converges because each
		// iteration either lands the cycle or frees a slot for someone.
		select {
		case <-w.queue:
			w.dropped.Add(1)
		default:
			// Drained by a concurrent producer; loop and try the send again.
		}
	}
}

// flushLoop is the single drainer of w.queue. It runs in a dedicated
// goroutine so that a slow or stalled v2 write pipeline can't propagate
// back-pressure to the probing goroutines via OnCycle.
func (w *Writer) flushLoop() {
	defer w.wg.Done()
	report := time.NewTicker(droppedReportInterval)
	defer report.Stop()
	var lastReported int64
	for {
		select {
		case c := <-w.queue:
			w.writeCycle(c)
		case <-report.C:
			if cur := w.dropped.Load(); cur > lastReported {
				w.log.Warn("dropped cycles under storage backpressure",
					"since_last_report", cur-lastReported,
					"total", cur)
				lastReported = cur
			}
		case <-w.stop:
			// Best-effort drain: write what's already queued, then exit.
			for {
				select {
				case c := <-w.queue:
					w.writeCycle(c)
				default:
					return
				}
			}
		}
	}
}

// writeCycle is the body that used to be OnCycle: the actual WritePoint
// calls into the v2 write API. Split out so OnCycle is the cheap enqueue
// and the slow path runs only from the flush goroutine.
func (w *Writer) writeCycle(c scheduler.Cycle) {
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
		"rtt_min":    ms(c.Summary.Min),
		"rtt_max":    ms(c.Summary.Max),
		"rtt_mean":   ms(c.Summary.Mean),
		"rtt_median": ms(c.Summary.Median),
		"rtt_stddev": ms(c.Summary.StdDev),
		"loss_pct":   lossPct,
		"loss_count": c.LossCount,
		"pings_sent": c.Sent,
	}
	for _, spec := range stats.PercentileSet {
		cycleFields["rtt_"+spec.Name] = ms(spec.Get(c.Summary))
	}
	w.write.WritePoint(write.NewPoint(measurementCycle, tags, cycleFields, c.Time))

	// HTTP cycles get their own per-request measurement with status codes;
	// emitting probe_rtt on top would double-write the same latencies and bloat
	// the raw bucket for no UI benefit. For every other probe type, probe_rtt
	// is the only per-sample record.
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
			w.write.WritePoint(write.NewPoint(measurementHTTP, tags, fields, ts))
		}
	} else {
		for i, rtt := range c.RTTs {
			// Spread individual samples by 1ms so they don't share a timestamp
			// (Influx would otherwise overwrite points with identical series+time).
			ts := c.Time.Add(time.Duration(i) * time.Millisecond)
			w.write.WritePoint(write.NewPoint(
				measurementRTT,
				tags,
				map[string]any{"rtt_ms": ms(rtt), "seq": i},
				ts,
			))
		}
	}

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
		lossPct := 0.0
		if hop.Sent > 0 {
			lossPct = 100 * float64(hop.Lost) / float64(hop.Sent)
		}
		w.write.WritePoint(write.NewPoint(measurementHop, hopTags, map[string]any{
			"hop_ip":     hop.IP,
			"rtt_min":    ms(summary.Min),
			"rtt_max":    ms(summary.Max),
			"rtt_mean":   ms(summary.Mean),
			"rtt_median": ms(summary.Median),
			"loss_pct":   lossPct,
			"loss_count": hop.Lost,
			"pings_sent": hop.Sent,
		}, c.Time))
	}
}

func ms(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

// Ping checks the server is reachable and authenticated.
func (w *Writer) Ping(ctx context.Context) error {
	ok, err := w.client.Ping(ctx)
	if err != nil {
		return fmt.Errorf("influx ping: %w", err)
	}
	if !ok {
		return fmt.Errorf("influx ping: server not ready")
	}
	return nil
}
