package influxv2

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
	"github.com/tumult/gosmokeping/internal/storage"
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

// sourceFilter builds an optional `|> filter(...)` clause on the `source` tag.
// Returns an empty string when source is "" so the caller still returns every
// row, including pre-cluster data that has no source tag at all.
func sourceFilter(source string) string {
	if source == "" {
		return ""
	}
	return fmt.Sprintf(`
  |> filter(fn: (r) => r.source == "%s")`, fluxEscape(source))
}

// Reader implements storage.Reader against InfluxDB v2. It holds its own
// client so Close can release resources without tearing down Writer.
type Reader struct {
	client influxdb2.Client
	cfg    config.InfluxV2
}

// NewReader constructs a Reader backed by a new v2 client. Caller must
// Close it on shutdown.
//
// HTTP timeout is raised to 90s (default is 20s) because bucketed
// hops/timeline queries at 7d have to scan ~600k raw rows from disk before
// aggregating and can legitimately take 30-60s on a busy InfluxDB; the
// default would deadline-exceed and surface as a generic 502 to the UI.
// CachingReader's TTL means the slow path runs at most once per minute
// per (target, window) pair, so the cost is bounded.
func NewReader(cfg config.InfluxV2) *Reader {
	opts := influxdb2.DefaultOptions().SetHTTPRequestTimeout(90)
	return &Reader{client: influxdb2.NewClientWithOptions(cfg.URL, cfg.Token, opts), cfg: cfg}
}

func (r *Reader) Close() { r.client.Close() }

func (r *Reader) bucketFor(res storage.Resolution) (string, error) {
	switch res {
	case storage.ResolutionRaw:
		return r.cfg.BucketRaw, nil
	case storage.Resolution1h:
		if r.cfg.Bucket1h == "" {
			return r.cfg.BucketRaw, nil
		}
		return r.cfg.Bucket1h, nil
	case storage.Resolution1d:
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
// See QueryFilter for Source semantics.
func (r *Reader) QueryCycles(ctx context.Context, ref config.TargetRef, from, to time.Time, res storage.Resolution, f storage.QueryFilter) ([]storage.CyclePoint, error) {
	for _, try := range fallbackChain(res) {
		bucket, err := r.bucketFor(try)
		if err != nil {
			return nil, err
		}
		points, err := r.queryCyclesFrom(ctx, bucket, ref, from, to, f.Source)
		if err != nil {
			return nil, err
		}
		if len(points) > 0 || try == storage.ResolutionRaw {
			return points, nil
		}
	}
	return nil, nil
}

// fallbackChain lists tiers to try, finest-last, so an empty rollup degrades
// gracefully to raw. Raw is always the final fallback.
func fallbackChain(res storage.Resolution) []storage.Resolution {
	switch res {
	case storage.Resolution1d:
		return []storage.Resolution{storage.Resolution1d, storage.Resolution1h, storage.ResolutionRaw}
	case storage.Resolution1h:
		return []storage.Resolution{storage.Resolution1h, storage.ResolutionRaw}
	default:
		return []storage.Resolution{storage.ResolutionRaw}
	}
}

func (r *Reader) queryCyclesFrom(ctx context.Context, bucket string, ref config.TargetRef, from, to time.Time, source string) ([]storage.CyclePoint, error) {
	flux := fmt.Sprintf(`
from(bucket: "%s")
  |> range(start: %s, stop: %s)
  |> filter(fn: (r) => r._measurement == "%s")
  |> filter(fn: (r) => r.group == "%s" and r.target == "%s")%s
  |> pivot(rowKey: ["_time"], columnKey: ["_field"], valueColumn: "_value")
  |> sort(columns: ["_time"])
`, fluxEscape(bucket), from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano), measurementCycle, fluxEscape(ref.Group), fluxEscape(ref.Target.Name), sourceFilter(source))

	qAPI := r.client.QueryAPI(r.cfg.Org)
	res2, err := qAPI.Query(ctx, flux)
	if err != nil {
		return nil, fmt.Errorf("query cycles: %w", err)
	}
	defer res2.Close()

	var out []storage.CyclePoint
	for res2.Next() {
		rec := res2.Record()
		vals := rec.Values()
		cp := storage.CyclePoint{
			Time:      rec.Time(),
			Source:    stringOf(vals["source"]),
			Min:       floatOf(vals["rtt_min"]),
			Max:       floatOf(vals["rtt_max"]),
			Mean:      floatOf(vals["rtt_mean"]),
			Median:    floatOf(vals["rtt_median"]),
			StdDev:    floatOf(vals["rtt_stddev"]),
			LossPct:   floatOf(vals["loss_pct"]),
			LossCount: intOf(vals["loss_count"]),
			Sent:      intOf(vals["pings_sent"]),
		}
		for _, acc := range storage.CyclePointPercentileAccessors {
			acc.Set(&cp, floatOf(vals["rtt_"+acc.Name]))
		}
		out = append(out, cp)
	}
	if err := res2.Err(); err != nil {
		return nil, fmt.Errorf("read cycles: %w", err)
	}
	return out, nil
}

// QueryRTTs returns individual ping samples. Always reads the raw bucket —
// rollups don't retain per-ping data.
func (r *Reader) QueryRTTs(ctx context.Context, ref config.TargetRef, from, to time.Time, f storage.QueryFilter) ([]storage.RTTPoint, error) {
	source := f.Source
	flux := fmt.Sprintf(`
from(bucket: "%s")
  |> range(start: %s, stop: %s)
  |> filter(fn: (r) => r._measurement == "%s")
  |> filter(fn: (r) => r.group == "%s" and r.target == "%s")%s
  |> pivot(rowKey: ["_time"], columnKey: ["_field"], valueColumn: "_value")
  |> sort(columns: ["_time"])
`, fluxEscape(r.cfg.BucketRaw), from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano), measurementRTT, fluxEscape(ref.Group), fluxEscape(ref.Target.Name), sourceFilter(source))

	qAPI := r.client.QueryAPI(r.cfg.Org)
	res, err := qAPI.Query(ctx, flux)
	if err != nil {
		return nil, fmt.Errorf("query rtts: %w", err)
	}
	defer res.Close()

	var out []storage.RTTPoint
	for res.Next() {
		rec := res.Record()
		vals := rec.Values()
		out = append(out, storage.RTTPoint{
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
func (r *Reader) QueryHTTPSamples(ctx context.Context, ref config.TargetRef, from, to time.Time, f storage.QueryFilter) ([]storage.HTTPPoint, error) {
	source := f.Source
	flux := fmt.Sprintf(`
from(bucket: "%s")
  |> range(start: %s, stop: %s)
  |> filter(fn: (r) => r._measurement == "%s")
  |> filter(fn: (r) => r.group == "%s" and r.target == "%s")%s
  |> pivot(rowKey: ["_time"], columnKey: ["_field"], valueColumn: "_value")
  |> sort(columns: ["_time"])
`, fluxEscape(r.cfg.BucketRaw), from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano), measurementHTTP, fluxEscape(ref.Group), fluxEscape(ref.Target.Name), sourceFilter(source))

	qAPI := r.client.QueryAPI(r.cfg.Org)
	res, err := qAPI.Query(ctx, flux)
	if err != nil {
		return nil, fmt.Errorf("query http: %w", err)
	}
	defer res.Close()

	var out []storage.HTTPPoint
	for res.Next() {
		rec := res.Record()
		vals := rec.Values()
		out = append(out, storage.HTTPPoint{
			Time:   rec.Time(),
			Source: stringOf(vals["source"]),
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
func (r *Reader) QueryHopsAt(ctx context.Context, ref config.TargetRef, at time.Time, window time.Duration, f storage.QueryFilter) ([]storage.HopPoint, error) {
	from := at.Add(-window)
	to := at.Add(window)
	all, err := r.queryHopsRange(ctx, ref, from, to, f.Source)
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
	out := make([]storage.HopPoint, 0, 32)
	for _, h := range all {
		if h.Time.Equal(best) {
			out = append(out, h)
		}
	}
	return out, nil
}

// QueryHopsTimeline returns hops across [from, to] for the heatmap. For
// narrow windows (≤6h) it returns raw per-cycle rows; for wider windows it
// aggregates server-side into time buckets so the response stays small —
// see storage.BucketForHops for the tier table. A 7d view at 20s probe
// interval is ~600k raw rows / ~113MB JSON; bucketed at 15m the same view
// is ~9k rows / ~1.5MB.
func (r *Reader) QueryHopsTimeline(ctx context.Context, ref config.TargetRef, from, to time.Time, f storage.QueryFilter) ([]storage.HopPoint, error) {
	bucket := storage.BucketForHops(to.Sub(from))
	if bucket == 0 {
		return r.queryHopsRange(ctx, ref, from, to, f.Source)
	}
	return r.queryHopsBucketed(ctx, ref, from, to, f.Source, bucket)
}

// queryHopsBucketed aggregates per-cycle hop rows into fixed-width time
// buckets. The heatmap only renders loss%, so we keep it lean: pivot the
// loss_count + pings_sent fields once, then `window + reduce` to roll up
// per-bucket sums in a single read pass. Earlier versions used five parallel
// `aggregateWindow` streams (sum/sum/mean/min/max for loss + the rtt
// distribution); over a 7d window that re-scanned the raw bucket five times
// and timed out the default 20s HTTP deadline. Dropping the rtt fields is
// fine because HopsTable uses /hops?at=… which stays unbucketed and carries
// the full per-cycle distribution. hop_ip is also dropped — it varies
// cycle-to-cycle when the path changes and the heatmap doesn't use it.
//
// LossPct is recomputed from the bucket sums (sum(loss_count) /
// sum(pings_sent) * 100). Bucket timestamp is the window start.
func (r *Reader) queryHopsBucketed(ctx context.Context, ref config.TargetRef, from, to time.Time, source string, bucket time.Duration) ([]storage.HopPoint, error) {
	every := bucket.String()
	flux := fmt.Sprintf(`
from(bucket: %q)
  |> range(start: %s, stop: %s)
  |> filter(fn: (r) => r._measurement == %q)
  |> filter(fn: (r) => r.group == %q and r.target == %q)%s
  |> filter(fn: (r) => r._field == "loss_count" or r._field == "pings_sent")
  |> pivot(rowKey: ["_time", "hop_index"], columnKey: ["_field"], valueColumn: "_value")
  |> window(every: %s)
  |> reduce(
       identity: {loss_count: 0, pings_sent: 0},
       fn: (r, accumulator) => ({
         loss_count: accumulator.loss_count + int(v: r.loss_count),
         pings_sent: accumulator.pings_sent + int(v: r.pings_sent),
       }))
  |> duplicate(column: "_start", as: "_time")
  |> group(columns: ["hop_index"])
`, fluxEscape(r.cfg.BucketRaw),
		from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano),
		measurementHop,
		fluxEscape(ref.Group), fluxEscape(ref.Target.Name),
		sourceFilter(source),
		every,
	)

	qAPI := r.client.QueryAPI(r.cfg.Org)
	res, err := qAPI.Query(ctx, flux)
	if err != nil {
		return nil, fmt.Errorf("query hops bucketed: %w", err)
	}
	defer res.Close()

	var out []storage.HopPoint
	for res.Next() {
		rec := res.Record()
		vals := rec.Values()
		idx, _ := strconv.ParseInt(stringOf(vals["hop_index"]), 10, 64)
		sent := intOf(vals["pings_sent"])
		lost := intOf(vals["loss_count"])
		lossPct := 0.0
		if sent > 0 {
			lossPct = 100 * float64(lost) / float64(sent)
		}
		out = append(out, storage.HopPoint{
			Time:      rec.Time(),
			Source:    stringOf(vals["source"]),
			Index:     idx,
			LossPct:   lossPct,
			LossCount: lost,
			Sent:      sent,
		})
	}
	if err := res.Err(); err != nil {
		return nil, fmt.Errorf("read hops bucketed: %w", err)
	}
	// hop_index is a string tag — re-sort numerically, breaking ties by time.
	slices.SortStableFunc(out, func(a, b storage.HopPoint) int {
		if c := a.Time.Compare(b.Time); c != 0 {
			return c
		}
		return cmp.Compare(a.Index, b.Index)
	})
	return out, nil
}

func (r *Reader) queryHopsRange(ctx context.Context, ref config.TargetRef, from, to time.Time, source string) ([]storage.HopPoint, error) {
	flux := fmt.Sprintf(`
from(bucket: "%s")
  |> range(start: %s, stop: %s)
  |> filter(fn: (r) => r._measurement == "%s")
  |> filter(fn: (r) => r.group == "%s" and r.target == "%s")%s
  |> pivot(rowKey: ["_time", "hop_index"], columnKey: ["_field"], valueColumn: "_value")
  |> sort(columns: ["_time", "hop_index"])
`, fluxEscape(r.cfg.BucketRaw), from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano), measurementHop, fluxEscape(ref.Group), fluxEscape(ref.Target.Name), sourceFilter(source))

	qAPI := r.client.QueryAPI(r.cfg.Org)
	res, err := qAPI.Query(ctx, flux)
	if err != nil {
		return nil, fmt.Errorf("query hops range: %w", err)
	}
	defer res.Close()

	var out []storage.HopPoint
	for res.Next() {
		rec := res.Record()
		vals := rec.Values()
		idx, _ := strconv.ParseInt(stringOf(vals["hop_index"]), 10, 64)
		out = append(out, storage.HopPoint{
			Time:      rec.Time(),
			Source:    stringOf(vals["source"]),
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
	slices.SortStableFunc(out, func(a, b storage.HopPoint) int {
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
func (r *Reader) QueryLatestHops(ctx context.Context, ref config.TargetRef, f storage.QueryFilter) ([]storage.HopPoint, error) {
	all, err := r.queryHopsRange(ctx, ref, time.Now().Add(-24*time.Hour), time.Now(), f.Source)
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
	out := make([]storage.HopPoint, 0, 32)
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
