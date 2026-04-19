package storage

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	influxdb2 "github.com/influxdata/influxdb-client-go/v2"

	"github.com/tumult/gosmokeping/internal/config"
)

// fluxEscape escapes a value for safe interpolation inside a Flux double-quoted
// string literal. Without this, a `"` or `\` in any operator-supplied string
// (bucket, org, group, target name) terminates the literal and lets the rest
// of the value execute as Flux. Apply to every string interpolated via %s into
// a `"..."` Flux literal.
func fluxEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// Resolution picks which bucket to query. PickResolution chooses one based on
// the time span so the UI can render cheaply at wide zoom levels.
type Resolution string

const (
	ResolutionRaw Resolution = "raw"
	Resolution1h  Resolution = "1h"
	Resolution1d  Resolution = "1d"
)

// PickResolution selects a bucket tier based on the requested time span.
// The raw bucket keeps 7d, so any span within that is served from raw — this
// avoids empty results when the 1h rollup task hasn't run yet. Wider spans
// fall through to the rollup tiers.
func PickResolution(from, to time.Time) Resolution {
	span := to.Sub(from)
	switch {
	case span <= 7*24*time.Hour:
		return ResolutionRaw
	case span <= 180*24*time.Hour:
		return Resolution1h
	default:
		return Resolution1d
	}
}

// CyclePoint is one row of probe_cycle aggregate data.
type CyclePoint struct {
	Time      time.Time
	Min       float64
	Max       float64
	Mean      float64
	Median    float64
	StdDev    float64
	P5        float64
	P10       float64
	P15       float64
	P20       float64
	P25       float64
	P30       float64
	P35       float64
	P40       float64
	P45       float64
	P55       float64
	P60       float64
	P65       float64
	P70       float64
	P75       float64
	P80       float64
	P85       float64
	P90       float64
	P95       float64
	LossPct   float64
	LossCount int64
	Sent      int64
}

// RTTPoint is one individual ping sample.
type RTTPoint struct {
	Time time.Time
	RTT  float64
	Seq  int64
}

// HTTPPoint is one HTTP request sample: RTT, status code, and an error string
// if the request failed. Status == 0 means no response was received (DNS,
// refused, TLS, timeout) and Err explains why.
type HTTPPoint struct {
	Time   time.Time
	RTT    float64
	Status int64
	Seq    int64
	Err    string
}

// HopPoint is the most recent stats for one hop on an MTR path.
type HopPoint struct {
	Time      time.Time
	Index     int64
	IP        string
	Min       float64
	Max       float64
	Mean      float64
	Median    float64
	LossPct   float64
	LossCount int64
	Sent      int64
}

type Reader struct {
	client influxdb2.Client
	cfg    config.InfluxDB
}

func NewReader(cfg config.InfluxDB) *Reader {
	return &Reader{client: influxdb2.NewClient(cfg.URL, cfg.Token), cfg: cfg}
}

func (r *Reader) Close() { r.client.Close() }

func (r *Reader) bucketFor(res Resolution) (string, error) {
	switch res {
	case ResolutionRaw:
		return r.cfg.BucketRaw, nil
	case Resolution1h:
		if r.cfg.Bucket1h == "" {
			return r.cfg.BucketRaw, nil
		}
		return r.cfg.Bucket1h, nil
	case Resolution1d:
		if r.cfg.Bucket1d == "" {
			if r.cfg.Bucket1h != "" {
				return r.cfg.Bucket1h, nil
			}
			return r.cfg.BucketRaw, nil
		}
		return r.cfg.Bucket1d, nil
	default:
		return "", fmt.Errorf("unknown resolution %q", res)
	}
}

// QueryCycles returns probe_cycle rows for one target across [from, to].
// If the picked rollup tier is empty (e.g. the 1h task hasn't run yet), it
// falls back to successively finer tiers so fresh installs still show data.
func (r *Reader) QueryCycles(ctx context.Context, ref config.TargetRef, from, to time.Time, res Resolution) ([]CyclePoint, error) {
	for _, try := range fallbackChain(res) {
		bucket, err := r.bucketFor(try)
		if err != nil {
			return nil, err
		}
		points, err := r.queryCyclesFrom(ctx, bucket, ref, from, to)
		if err != nil {
			return nil, err
		}
		if len(points) > 0 || try == ResolutionRaw {
			return points, nil
		}
	}
	return nil, nil
}

// fallbackChain lists tiers to try, finest-last, so an empty rollup degrades
// gracefully to raw. Raw is always the final fallback.
func fallbackChain(res Resolution) []Resolution {
	switch res {
	case Resolution1d:
		return []Resolution{Resolution1d, Resolution1h, ResolutionRaw}
	case Resolution1h:
		return []Resolution{Resolution1h, ResolutionRaw}
	default:
		return []Resolution{ResolutionRaw}
	}
}

func (r *Reader) queryCyclesFrom(ctx context.Context, bucket string, ref config.TargetRef, from, to time.Time) ([]CyclePoint, error) {
	flux := fmt.Sprintf(`
from(bucket: "%s")
  |> range(start: %s, stop: %s)
  |> filter(fn: (r) => r._measurement == "%s")
  |> filter(fn: (r) => r.group == "%s" and r.target == "%s")
  |> pivot(rowKey: ["_time"], columnKey: ["_field"], valueColumn: "_value")
  |> sort(columns: ["_time"])
`, fluxEscape(bucket), from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano), MeasurementCycle, fluxEscape(ref.Group), fluxEscape(ref.Target.Name))

	qAPI := r.client.QueryAPI(r.cfg.Org)
	res2, err := qAPI.Query(ctx, flux)
	if err != nil {
		return nil, fmt.Errorf("query cycles: %w", err)
	}
	defer res2.Close()

	var out []CyclePoint
	for res2.Next() {
		rec := res2.Record()
		vals := rec.Values()
		out = append(out, CyclePoint{
			Time:      rec.Time(),
			Min:       floatOf(vals["rtt_min"]),
			Max:       floatOf(vals["rtt_max"]),
			Mean:      floatOf(vals["rtt_mean"]),
			Median:    floatOf(vals["rtt_median"]),
			StdDev:    floatOf(vals["rtt_stddev"]),
			P5:        floatOf(vals["rtt_p5"]),
			P10:       floatOf(vals["rtt_p10"]),
			P15:       floatOf(vals["rtt_p15"]),
			P20:       floatOf(vals["rtt_p20"]),
			P25:       floatOf(vals["rtt_p25"]),
			P30:       floatOf(vals["rtt_p30"]),
			P35:       floatOf(vals["rtt_p35"]),
			P40:       floatOf(vals["rtt_p40"]),
			P45:       floatOf(vals["rtt_p45"]),
			P55:       floatOf(vals["rtt_p55"]),
			P60:       floatOf(vals["rtt_p60"]),
			P65:       floatOf(vals["rtt_p65"]),
			P70:       floatOf(vals["rtt_p70"]),
			P75:       floatOf(vals["rtt_p75"]),
			P80:       floatOf(vals["rtt_p80"]),
			P85:       floatOf(vals["rtt_p85"]),
			P90:       floatOf(vals["rtt_p90"]),
			P95:       floatOf(vals["rtt_p95"]),
			LossPct:   floatOf(vals["loss_pct"]),
			LossCount: intOf(vals["loss_count"]),
			Sent:      intOf(vals["pings_sent"]),
		})
	}
	if err := res2.Err(); err != nil {
		return nil, fmt.Errorf("read cycles: %w", err)
	}
	return out, nil
}

// QueryRTTs returns individual ping samples. Always reads the raw bucket —
// rollups don't retain per-ping data.
func (r *Reader) QueryRTTs(ctx context.Context, ref config.TargetRef, from, to time.Time) ([]RTTPoint, error) {
	flux := fmt.Sprintf(`
from(bucket: "%s")
  |> range(start: %s, stop: %s)
  |> filter(fn: (r) => r._measurement == "%s")
  |> filter(fn: (r) => r.group == "%s" and r.target == "%s")
  |> pivot(rowKey: ["_time"], columnKey: ["_field"], valueColumn: "_value")
  |> sort(columns: ["_time"])
`, fluxEscape(r.cfg.BucketRaw), from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano), MeasurementRTT, fluxEscape(ref.Group), fluxEscape(ref.Target.Name))

	qAPI := r.client.QueryAPI(r.cfg.Org)
	res, err := qAPI.Query(ctx, flux)
	if err != nil {
		return nil, fmt.Errorf("query rtts: %w", err)
	}
	defer res.Close()

	var out []RTTPoint
	for res.Next() {
		rec := res.Record()
		vals := rec.Values()
		out = append(out, RTTPoint{
			Time: rec.Time(),
			RTT:  floatOf(vals["rtt_ms"]),
			Seq:  intOf(vals["seq"]),
		})
	}
	if err := res.Err(); err != nil {
		return nil, fmt.Errorf("read rtts: %w", err)
	}
	return out, nil
}

// QueryHTTPSamples returns per-request HTTP samples for the target across the
// given window. Always reads the raw bucket — HTTP samples aren't rolled up
// because 1-2/cycle is already cheap and the status code wouldn't aggregate
// usefully anyway.
func (r *Reader) QueryHTTPSamples(ctx context.Context, ref config.TargetRef, from, to time.Time) ([]HTTPPoint, error) {
	flux := fmt.Sprintf(`
from(bucket: "%s")
  |> range(start: %s, stop: %s)
  |> filter(fn: (r) => r._measurement == "%s")
  |> filter(fn: (r) => r.group == "%s" and r.target == "%s")
  |> pivot(rowKey: ["_time"], columnKey: ["_field"], valueColumn: "_value")
  |> sort(columns: ["_time"])
`, fluxEscape(r.cfg.BucketRaw), from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano), MeasurementHTTP, fluxEscape(ref.Group), fluxEscape(ref.Target.Name))

	qAPI := r.client.QueryAPI(r.cfg.Org)
	res, err := qAPI.Query(ctx, flux)
	if err != nil {
		return nil, fmt.Errorf("query http: %w", err)
	}
	defer res.Close()

	var out []HTTPPoint
	for res.Next() {
		rec := res.Record()
		vals := rec.Values()
		out = append(out, HTTPPoint{
			Time:   rec.Time(),
			RTT:    floatOf(vals["rtt_ms"]),
			Status: intOf(vals["status_code"]),
			Seq:    intOf(vals["seq"]),
			Err:    stringOf(vals["error"]),
		})
	}
	if err := res.Err(); err != nil {
		return nil, fmt.Errorf("read http: %w", err)
	}
	return out, nil
}

// QueryHopsAt returns hops for the single MTR cycle whose timestamp is
// closest to `at`, within ±window. Picks by minimum absolute time distance
// across all hop rows in the window, then returns every hop sharing that
// cycle's timestamp. Empty result means no cycles hit the window.
func (r *Reader) QueryHopsAt(ctx context.Context, ref config.TargetRef, at time.Time, window time.Duration) ([]HopPoint, error) {
	from := at.Add(-window)
	to := at.Add(window)
	all, err := r.queryHopsRange(ctx, ref, from, to)
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, nil
	}
	// Find the cycle timestamp closest to `at`. Every hop in a single cycle
	// shares the exact same _time (set by the writer), so equality grouping
	// works without bucketing.
	best := all[0].Time
	bestDiff := absDur(at.Sub(best))
	for _, h := range all[1:] {
		if d := absDur(at.Sub(h.Time)); d < bestDiff {
			bestDiff = d
			best = h.Time
		}
	}
	out := make([]HopPoint, 0, 32)
	for _, h := range all {
		if h.Time.Equal(best) {
			out = append(out, h)
		}
	}
	return out, nil
}

// QueryHopsTimeline returns every hop row across [from, to], sorted by time
// then hop_index. Used by the UI heatmap to render per-hop loss over the
// requested window.
func (r *Reader) QueryHopsTimeline(ctx context.Context, ref config.TargetRef, from, to time.Time) ([]HopPoint, error) {
	return r.queryHopsRange(ctx, ref, from, to)
}

func (r *Reader) queryHopsRange(ctx context.Context, ref config.TargetRef, from, to time.Time) ([]HopPoint, error) {
	flux := fmt.Sprintf(`
from(bucket: "%s")
  |> range(start: %s, stop: %s)
  |> filter(fn: (r) => r._measurement == "%s")
  |> filter(fn: (r) => r.group == "%s" and r.target == "%s")
  |> pivot(rowKey: ["_time", "hop_index"], columnKey: ["_field"], valueColumn: "_value")
  |> sort(columns: ["_time", "hop_index"])
`, fluxEscape(r.cfg.BucketRaw), from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano), MeasurementHop, fluxEscape(ref.Group), fluxEscape(ref.Target.Name))

	qAPI := r.client.QueryAPI(r.cfg.Org)
	res, err := qAPI.Query(ctx, flux)
	if err != nil {
		return nil, fmt.Errorf("query hops range: %w", err)
	}
	defer res.Close()

	var out []HopPoint
	for res.Next() {
		rec := res.Record()
		vals := rec.Values()
		idx, _ := strconv.ParseInt(stringOf(vals["hop_index"]), 10, 64)
		out = append(out, HopPoint{
			Time:      rec.Time(),
			Index:     idx,
			IP:        stringOf(vals["hop_ip"]),
			Min:       floatOf(vals["rtt_min"]),
			Max:       floatOf(vals["rtt_max"]),
			Mean:      floatOf(vals["rtt_mean"]),
			Median:    floatOf(vals["rtt_median"]),
			LossPct:   floatOf(vals["loss_pct"]),
			LossCount: intOf(vals["loss_count"]),
			Sent:      intOf(vals["pings_sent"]),
		})
	}
	if err := res.Err(); err != nil {
		return nil, fmt.Errorf("read hops range: %w", err)
	}
	// hop_index is an InfluxDB tag (string), so Flux's sort orders it
	// lexicographically ("1","10","11",...,"2"). Reorder numerically here,
	// breaking ties by time to preserve per-cycle grouping.
	slices.SortStableFunc(out, func(a, b HopPoint) int {
		if c := a.Time.Compare(b.Time); c != 0 {
			return c
		}
		return cmp.Compare(a.Index, b.Index)
	})
	return out, nil
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// QueryLatestHops returns hops from the single most recent MTR cycle for the
// target. All hops in one cycle share an identical _time (set by the writer),
// so we pick the max timestamp across the recent window and return only rows
// matching it. Grouping by hop_index and taking the latest per index — the
// earlier approach — leaves stale rows for higher indexes when the path
// shrinks, rendering phantom hops past the current target.
func (r *Reader) QueryLatestHops(ctx context.Context, ref config.TargetRef) ([]HopPoint, error) {
	all, err := r.queryHopsRange(ctx, ref, time.Now().Add(-24*time.Hour), time.Now())
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, nil
	}
	latest := all[0].Time
	for _, h := range all[1:] {
		if h.Time.After(latest) {
			latest = h.Time
		}
	}
	out := make([]HopPoint, 0, 32)
	for _, h := range all {
		if h.Time.Equal(latest) {
			out = append(out, h)
		}
	}
	return out, nil
}

func stringOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func floatOf(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int64:
		return float64(x)
	case int:
		return float64(x)
	default:
		return 0
	}
}

func intOf(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	default:
		return 0
	}
}
