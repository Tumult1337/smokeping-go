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
	"time"

	"github.com/InfluxCommunity/influxdb3-go/v2/influxdb3"
	"github.com/InfluxCommunity/influxdb3-go/v2/influxdb3/batching"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/scheduler"
	"github.com/tumult/gosmokeping/internal/stats"
)

// batcher tunables. The batching package fires the emit callback on size
// alone, which is wrong for our shape — a quiet single-target install
// would buffer one cycle indefinitely waiting for 999 more points. We
// add a time-based flusher on top so a stalled probe never holds the last
// cycle in RAM forever.
const (
	batchSize     = 1000
	batchInterval = 1 * time.Second
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
// Writes flow through the official batching.Batcher: OnCycle stages points
// in the batcher and a background goroutine flushes either when a batch
// fills (default 1000 points) or once per batchInterval. This is the
// recommended pattern for InfluxDB v3 high-write workloads — sync writes
// would force one HTTP roundtrip per cycle, which is the bottleneck a
// many-slave master is trying to avoid.
type Writer struct {
	log     *slog.Logger
	client  *influxdb3.Client
	batcher *batching.Batcher

	stop   chan struct{}
	done   chan struct{}
	closed sync.Once
}

// NewWriter constructs a Writer backed by a v3 client. The auth scheme is
// pinned to "Bearer" because every InfluxDB 3 deployment we target (Core OSS,
// Enterprise, Cloud Dedicated) speaks the v3 API with a bearer token; the
// client's empty-default would otherwise emit "Token <secret>" which only
// v2-compat endpoints accept.
func NewWriter(log *slog.Logger, cfg config.InfluxV3) (*Writer, error) {
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
		log:    log,
		client: c,
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	w.batcher = batching.NewBatcher(
		batching.WithSize(batchSize),
		batching.WithEmitCallback(w.flush),
	)
	go w.tick()
	return w, nil
}

// tick periodically drains anything sitting in the batcher so a quiet
// install isn't holding the most recent cycle indefinitely waiting for
// the next size-trigger. A separate goroutine is the simplest way to add
// time-based flushing on top of the size-only batching package.
func (w *Writer) tick() {
	defer close(w.done)
	t := time.NewTicker(batchInterval)
	defer t.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-t.C:
			if pts := w.batcher.Flush(); len(pts) > 0 {
				w.flush(pts)
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

// OnCycle satisfies scheduler.Sink. Stages points in the batcher and returns
// quickly; the actual HTTP write happens on the next batch fill or tick.
// ctx is unused — the batcher fires its emit callback async (see flush) and
// callers shouldn't block the scheduler waiting for a network roundtrip.
func (w *Writer) OnCycle(_ context.Context, c scheduler.Cycle) {
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

	w.batcher.Add(points...)
}

func ms(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}
