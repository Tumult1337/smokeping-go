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
func NewReader(cfg config.InfluxV2) *Reader {
	return &Reader{client: influxdb2.NewClient(cfg.URL, cfg.Token), cfg: cfg}
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
//
// source is optional: when empty, no source filter is applied — pre-cluster
// data (no `source` tag) still renders. When set, rows are filtered to that
// exact source value.
func (r *Reader) QueryCycles(ctx context.Context, ref config.TargetRef, from, to time.Time, res storage.Resolution, source string) ([]storage.CyclePoint, error) {
	for _, try := range fallbackChain(res) {
		bucket, err := r.bucketFor(try)
		if err != nil {
			return nil, err
		}
		points, err := r.queryCyclesFrom(ctx, bucket, ref, from, to, source)
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
		out = append(out, storage.CyclePoint{
			Time:      rec.Time(),
			Source:    stringOf(vals["source"]),
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
// rollups don't retain per-ping data. See QueryCycles for `source` semantics.
func (r *Reader) QueryRTTs(ctx context.Context, ref config.TargetRef, from, to time.Time, source string) ([]storage.RTTPoint, error) {
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
// usefully anyway. See QueryCycles for `source` semantics.
func (r *Reader) QueryHTTPSamples(ctx context.Context, ref config.TargetRef, from, to time.Time, source string) ([]storage.HTTPPoint, error) {
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
// See QueryCycles for `source` semantics.
func (r *Reader) QueryHopsAt(ctx context.Context, ref config.TargetRef, at time.Time, window time.Duration, source string) ([]storage.HopPoint, error) {
	from := at.Add(-window)
	to := at.Add(window)
	all, err := r.queryHopsRange(ctx, ref, from, to, source)
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

// QueryHopsTimeline returns every hop row across [from, to], sorted by time
// then hop_index. Used by the UI heatmap to render per-hop loss over the
// requested window. See QueryCycles for `source` semantics.
func (r *Reader) QueryHopsTimeline(ctx context.Context, ref config.TargetRef, from, to time.Time, source string) ([]storage.HopPoint, error) {
	return r.queryHopsRange(ctx, ref, from, to, source)
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
func (r *Reader) QueryLatestHops(ctx context.Context, ref config.TargetRef, source string) ([]storage.HopPoint, error) {
	all, err := r.queryHopsRange(ctx, ref, time.Now().Add(-24*time.Hour), time.Now(), source)
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
