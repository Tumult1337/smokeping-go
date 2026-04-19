package storage

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/influxdata/influxdb-client-go/v2/api"
	"github.com/influxdata/influxdb-client-go/v2/api/write"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/scheduler"
	"github.com/tumult/gosmokeping/internal/stats"
)

const (
	MeasurementCycle = "probe_cycle"
	MeasurementRTT   = "probe_rtt"
	// MeasurementHop is one row per hop per MTR cycle. hop_ip is a field (not
	// tag) because routers along a path flap and we don't want a new series
	// every time the path changes.
	MeasurementHop = "probe_mtr_hop"
	// MeasurementHTTP is one row per HTTP request. status_code is a field (not
	// tag) to avoid series cardinality exploding on pages that cycle through
	// error codes.
	MeasurementHTTP = "probe_http"
)

// Writer writes completed cycles to InfluxDB. Implements scheduler.Sink.
// Two points per cycle: a cycle-level aggregate in the raw bucket, plus one
// point per individual RTT (also in the raw bucket) so the UI can render the
// full smoke band at close range. The 1h/1d buckets are populated by tasks.
type Writer struct {
	log    *slog.Logger
	client influxdb2.Client
	write  api.WriteAPI
	cfg    config.InfluxDB
}

func NewWriter(log *slog.Logger, cfg config.InfluxDB) *Writer {
	client := influxdb2.NewClient(cfg.URL, cfg.Token)
	wa := client.WriteAPI(cfg.Org, cfg.BucketRaw)
	// Log async write errors instead of silently dropping them.
	go func() {
		for err := range wa.Errors() {
			log.Warn("influx async write", "err", err)
		}
	}()
	return &Writer{log: log, client: client, write: wa, cfg: cfg}
}

// Close flushes pending writes and releases the client.
func (w *Writer) Close() {
	if w.write != nil {
		w.write.Flush()
	}
	if w.client != nil {
		w.client.Close()
	}
}

func (w *Writer) OnCycle(_ context.Context, c scheduler.Cycle) {
	tags := map[string]string{
		"target": c.Target.Target.Name,
		"group":  c.Target.Group,
		"probe":  c.ProbeName,
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
		"rtt_p5":     ms(c.Summary.P5),
		"rtt_p10":    ms(c.Summary.P10),
		"rtt_p15":    ms(c.Summary.P15),
		"rtt_p20":    ms(c.Summary.P20),
		"rtt_p25":    ms(c.Summary.P25),
		"rtt_p30":    ms(c.Summary.P30),
		"rtt_p35":    ms(c.Summary.P35),
		"rtt_p40":    ms(c.Summary.P40),
		"rtt_p45":    ms(c.Summary.P45),
		"rtt_p55":    ms(c.Summary.P55),
		"rtt_p60":    ms(c.Summary.P60),
		"rtt_p65":    ms(c.Summary.P65),
		"rtt_p70":    ms(c.Summary.P70),
		"rtt_p75":    ms(c.Summary.P75),
		"rtt_p80":    ms(c.Summary.P80),
		"rtt_p85":    ms(c.Summary.P85),
		"rtt_p90":    ms(c.Summary.P90),
		"rtt_p95":    ms(c.Summary.P95),
		"loss_pct":   lossPct,
		"loss_count": c.LossCount,
		"pings_sent": c.Sent,
	}
	w.write.WritePoint(write.NewPoint(MeasurementCycle, tags, cycleFields, c.Time))

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
			w.write.WritePoint(write.NewPoint(MeasurementHTTP, tags, fields, ts))
		}
	} else {
		for i, rtt := range c.RTTs {
			// Spread individual samples by 1ms so they don't share a timestamp
			// (Influx would otherwise overwrite points with identical series+time).
			ts := c.Time.Add(time.Duration(i) * time.Millisecond)
			w.write.WritePoint(write.NewPoint(
				MeasurementRTT,
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
		summary := stats.Compute(hop.RTTs)
		lossPct := 0.0
		if hop.Sent > 0 {
			lossPct = 100 * float64(hop.Lost) / float64(hop.Sent)
		}
		w.write.WritePoint(write.NewPoint(MeasurementHop, hopTags, map[string]any{
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
