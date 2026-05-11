package influxv3

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/InfluxCommunity/influxdb3-go/v2/influxdb3"
	"github.com/apache/arrow-go/v18/arrow"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/storage"
)

// Reader implements storage.Reader against InfluxDB v3. It owns its own
// client so Close can release the Flight gRPC stream and HTTP transport
// without tearing down the Writer.
//
// QueryTimeout is bumped well above the client's 10s default because cold
// (uncached) wide-window queries — a 1y view bucketed at 1d, say — have
// to scan a meaningful slice of raw cycles in v3's Parquet store before
// aggregating. v3+DataFusion handle the workload, but on a busy node the
// first response can legitimately take 30-60s; the default would deadline
// out and surface as a 502 to the UI. CachingReader bounds how often the
// slow path runs (~once per minute per (target, window) pair).
type Reader struct {
	client *influxdb3.Client
}

// NewReader constructs a Reader backed by a v3 client. AuthScheme is "Bearer"
// for the same reason it is on the writer (see NewWriter).
func NewReader(cfg config.InfluxV3) (*Reader, error) {
	c, err := influxdb3.New(influxdb3.ClientConfig{
		Host:         cfg.URL,
		Token:        cfg.Token,
		Database:     cfg.Database,
		AuthScheme:   "Bearer",
		QueryTimeout: 90 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("influxv3 client: %w", err)
	}
	return &Reader{client: c}, nil
}

// Close releases the Flight gRPC client and pooled HTTP connections.
func (r *Reader) Close() {
	if r.client != nil {
		_ = r.client.Close()
	}
}

// QueryCycles returns probe_cycle rows for one target across [from, to].
// For raw resolution every cycle is returned as-is. For 1h/1d the query
// aggregates server-side via date_bin so the response stays small without
// needing pre-rolled buckets — see backend.go's PickResolution comment for
// why this trade-off is preferable on v3 storage. f.Source narrows to a
// single source when set; empty means every source plus pre-cluster rows.
func (r *Reader) QueryCycles(ctx context.Context, ref config.TargetRef, from, to time.Time, res storage.Resolution, f storage.QueryFilter) ([]storage.CyclePoint, error) {
	bucket := bucketForResolution(res)
	if bucket == 0 {
		return r.queryCyclesRaw(ctx, ref, from, to, f.Source)
	}
	return r.queryCyclesAggregated(ctx, ref, from, to, f.Source, bucket)
}

func (r *Reader) queryCyclesRaw(ctx context.Context, ref config.TargetRef, from, to time.Time, source string) ([]storage.CyclePoint, error) {
	cols := buildCycleColumns(false, "")
	sql := fmt.Sprintf(
		"SELECT %s FROM %s WHERE %s ORDER BY time",
		cols, quoteIdent(measurementCycle), targetWhereClause(source != ""),
	)
	return r.runCycleQuery(ctx, sql, baseTargetParams(ref, from, to, source))
}

// queryCyclesAggregated rolls raw cycles up into fixed-width buckets at query
// time. Aggregation choices mirror the v2 fluxRollup task to keep the smoke
// band visually identical across backends: min-of-mins, max-of-maxes,
// mean-of-means/medians/stddevs/percentiles, sum-of-loss-counts/sent.
// loss_pct is omitted from the aggregate SELECT — runCycleQuery recomputes
// LossPct from the summed loss_count/pings_sent so partial cycles
// (context-cancelled mid-probe) don't skew the displayed value.
func (r *Reader) queryCyclesAggregated(ctx context.Context, ref config.TargetRef, from, to time.Time, source string, bucket time.Duration) ([]storage.CyclePoint, error) {
	interval := dateBinInterval(bucket)
	cols := buildCycleColumns(true, interval)
	sql := fmt.Sprintf(
		"SELECT %s FROM %s WHERE %s GROUP BY date_bin(INTERVAL '%s', time), %s ORDER BY time",
		cols, quoteIdent(measurementCycle), targetWhereClause(source != ""),
		interval, quoteIdent("source"),
	)
	return r.runCycleQuery(ctx, sql, baseTargetParams(ref, from, to, source))
}

// buildCycleColumns generates the SELECT list for cycle queries. With
// aggregate=true every numeric column is wrapped in MIN/MAX/AVG/SUM matching
// v2's fluxRollup, and the time column is replaced by date_bin().
func buildCycleColumns(aggregate bool, interval string) string {
	var b strings.Builder
	if aggregate {
		fmt.Fprintf(&b, "date_bin(INTERVAL '%s', time) AS time, %s AS source", interval, quoteIdent("source"))
	} else {
		fmt.Fprintf(&b, "time, %s AS source", quoteIdent("source"))
	}
	add := func(field, fn string) {
		if aggregate {
			fmt.Fprintf(&b, ", %s(%s) AS %s", fn, quoteIdent(field), field)
		} else {
			fmt.Fprintf(&b, ", %s", quoteIdent(field))
		}
	}
	add("rtt_min", "MIN")
	add("rtt_max", "MAX")
	add("rtt_mean", "AVG")
	add("rtt_median", "AVG")
	add("rtt_stddev", "AVG")
	for _, acc := range storage.CyclePointPercentileAccessors {
		add("rtt_"+acc.Name, "AVG")
	}
	if !aggregate {
		add("loss_pct", "")
	}
	add("loss_count", "SUM")
	add("pings_sent", "SUM")
	return b.String()
}

func (r *Reader) runCycleQuery(ctx context.Context, sql string, params influxdb3.QueryParameters) ([]storage.CyclePoint, error) {
	iter, err := r.client.QueryWithParameters(ctx, sql, params)
	if err != nil {
		return nil, fmt.Errorf("query cycles: %w", err)
	}
	var out []storage.CyclePoint
	for iter.Next() {
		v := iter.Value()
		sent := intOf(v["pings_sent"])
		lost := intOf(v["loss_count"])
		lossPct := 0.0
		if sent > 0 {
			lossPct = 100 * float64(lost) / float64(sent)
		}
		cp := storage.CyclePoint{
			Time:      timeOf(v["time"]),
			Source:    stringOf(v["source"]),
			Min:       floatOf(v["rtt_min"]),
			Max:       floatOf(v["rtt_max"]),
			Mean:      floatOf(v["rtt_mean"]),
			Median:    floatOf(v["rtt_median"]),
			StdDev:    floatOf(v["rtt_stddev"]),
			LossPct:   lossPct,
			LossCount: lost,
			Sent:      sent,
		}
		for _, acc := range storage.CyclePointPercentileAccessors {
			acc.Set(&cp, floatOf(v["rtt_"+acc.Name]))
		}
		out = append(out, cp)
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("read cycles: %w", err)
	}
	return out, nil
}

// QueryRTTs returns individual ping samples — always raw, no aggregation,
// because per-ping data is what makes the smoke band possible at close zoom.
func (r *Reader) QueryRTTs(ctx context.Context, ref config.TargetRef, from, to time.Time, f storage.QueryFilter) ([]storage.RTTPoint, error) {
	sql := fmt.Sprintf(
		"SELECT time, %s, %s FROM %s WHERE %s ORDER BY time",
		quoteIdent("rtt_ms"), quoteIdent("seq"), quoteIdent(measurementRTT),
		targetWhereClause(f.Source != ""),
	)
	iter, err := r.client.QueryWithParameters(ctx, sql, baseTargetParams(ref, from, to, f.Source))
	if err != nil {
		return nil, fmt.Errorf("query rtts: %w", err)
	}
	var out []storage.RTTPoint
	for iter.Next() {
		v := iter.Value()
		out = append(out, storage.RTTPoint{
			Time: timeOf(v["time"]),
			RTT:  floatOf(v["rtt_ms"]),
			Seq:  intOf(v["seq"]),
		})
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("read rtts: %w", err)
	}
	return out, nil
}

// QueryHTTPSamples returns per-request HTTP samples. No aggregation: the
// status_code field would mean nothing if averaged, and the volume (1-2/cycle)
// is already cheap.
func (r *Reader) QueryHTTPSamples(ctx context.Context, ref config.TargetRef, from, to time.Time, f storage.QueryFilter) ([]storage.HTTPPoint, error) {
	sql := fmt.Sprintf(
		"SELECT time, %s, %s, %s, %s, %s FROM %s WHERE %s ORDER BY time",
		quoteIdent("source"), quoteIdent("rtt_ms"), quoteIdent("status_code"),
		quoteIdent("seq"), quoteIdent("error"), quoteIdent(measurementHTTP),
		targetWhereClause(f.Source != ""),
	)
	iter, err := r.client.QueryWithParameters(ctx, sql, baseTargetParams(ref, from, to, f.Source))
	if err != nil {
		return nil, fmt.Errorf("query http: %w", err)
	}
	var out []storage.HTTPPoint
	for iter.Next() {
		v := iter.Value()
		out = append(out, storage.HTTPPoint{
			Time:   timeOf(v["time"]),
			Source: stringOf(v["source"]),
			RTT:    floatOf(v["rtt_ms"]),
			Status: intOf(v["status_code"]),
			Seq:    intOf(v["seq"]),
			Err:    stringOf(v["error"]),
		})
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("read http: %w", err)
	}
	return out, nil
}

// QueryHopsAt returns hops for the single MTR cycle whose timestamp is
// closest to `at`, within ±window. Same pattern as v2: pull the window,
// pick the timestamp closest to `at`, return every hop sharing that exact
// timestamp (every hop in one cycle gets the same writer-set _time).
func (r *Reader) QueryHopsAt(ctx context.Context, ref config.TargetRef, at time.Time, window time.Duration, f storage.QueryFilter) ([]storage.HopPoint, error) {
	all, err := r.queryHopsRange(ctx, ref, at.Add(-window), at.Add(window), f.Source)
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, nil
	}
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

// QueryHopsTimeline returns hops across [from, to] for the heatmap. Narrow
// windows return raw per-cycle rows; wider windows aggregate server-side
// into time buckets (table in storage.BucketForHops) so the response stays
// small.
func (r *Reader) QueryHopsTimeline(ctx context.Context, ref config.TargetRef, from, to time.Time, f storage.QueryFilter) ([]storage.HopPoint, error) {
	bucket := storage.BucketForHops(to.Sub(from))
	if bucket == 0 {
		return r.queryHopsRange(ctx, ref, from, to, f.Source)
	}
	return r.queryHopsBucketed(ctx, ref, from, to, f.Source, bucket)
}

// QueryLatestHops returns hops from the most recent MTR cycle for the
// target. Picks max(time) across the last 24h and returns every row at that
// exact timestamp; doing it that way (instead of "latest per hop_index")
// avoids stale phantom rows for higher indexes when a path shrinks.
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

func (r *Reader) queryHopsRange(ctx context.Context, ref config.TargetRef, from, to time.Time, source string) ([]storage.HopPoint, error) {
	sql := fmt.Sprintf(
		"SELECT time, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s "+
			"FROM %s WHERE %s ORDER BY time, hop_index",
		quoteIdent("source"), quoteIdent("hop_index"), quoteIdent("hop_ip"),
		quoteIdent("rtt_min"), quoteIdent("rtt_max"), quoteIdent("rtt_mean"), quoteIdent("rtt_median"),
		quoteIdent("loss_pct"), quoteIdent("loss_count"), quoteIdent("pings_sent"),
		quoteIdent(measurementHop),
		targetWhereClause(source != ""),
	)
	iter, err := r.client.QueryWithParameters(ctx, sql, baseTargetParams(ref, from, to, source))
	if err != nil {
		return nil, fmt.Errorf("query hops range: %w", err)
	}
	var out []storage.HopPoint
	for iter.Next() {
		v := iter.Value()
		out = append(out, storage.HopPoint{
			Time:      timeOf(v["time"]),
			Source:    stringOf(v["source"]),
			Index:     intOf(v["hop_index"]),
			IP:        stringOf(v["hop_ip"]),
			Min:       floatOf(v["rtt_min"]),
			Max:       floatOf(v["rtt_max"]),
			Mean:      floatOf(v["rtt_mean"]),
			Median:    floatOf(v["rtt_median"]),
			LossPct:   floatOf(v["loss_pct"]),
			LossCount: intOf(v["loss_count"]),
			Sent:      intOf(v["pings_sent"]),
		})
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("read hops range: %w", err)
	}
	// hop_index is a string tag — re-sort numerically, breaking ties by time
	// so per-cycle grouping (every hop in one cycle shares one timestamp) is
	// preserved.
	slices.SortStableFunc(out, func(a, b storage.HopPoint) int {
		if c := a.Time.Compare(b.Time); c != 0 {
			return c
		}
		return cmp.Compare(a.Index, b.Index)
	})
	return out, nil
}

// queryHopsBucketed aggregates per-cycle hop rows into fixed-width time
// buckets for the heatmap. The heatmap only renders loss%, so we keep the
// query lean: SUM(loss_count) and SUM(pings_sent) per bucket per hop_index,
// recompute LossPct from the sums client-side. RTT distribution and hop_ip
// are dropped — HopsTable hits /hops?at=… (unbucketed) for those.
func (r *Reader) queryHopsBucketed(ctx context.Context, ref config.TargetRef, from, to time.Time, source string, bucket time.Duration) ([]storage.HopPoint, error) {
	interval := dateBinInterval(bucket)
	sql := fmt.Sprintf(
		"SELECT date_bin(INTERVAL '%s', time) AS time, %s, %s, "+
			"SUM(%s) AS loss_count, SUM(%s) AS pings_sent "+
			"FROM %s WHERE %s "+
			"GROUP BY date_bin(INTERVAL '%s', time), %s, %s "+
			"ORDER BY time, hop_index",
		interval,
		quoteIdent("source"), quoteIdent("hop_index"),
		quoteIdent("loss_count"), quoteIdent("pings_sent"),
		quoteIdent(measurementHop),
		targetWhereClause(source != ""),
		interval, quoteIdent("source"), quoteIdent("hop_index"),
	)
	iter, err := r.client.QueryWithParameters(ctx, sql, baseTargetParams(ref, from, to, source))
	if err != nil {
		return nil, fmt.Errorf("query hops bucketed: %w", err)
	}
	var out []storage.HopPoint
	for iter.Next() {
		v := iter.Value()
		sent := intOf(v["pings_sent"])
		lost := intOf(v["loss_count"])
		lossPct := 0.0
		if sent > 0 {
			lossPct = 100 * float64(lost) / float64(sent)
		}
		out = append(out, storage.HopPoint{
			Time:      timeOf(v["time"]),
			Source:    stringOf(v["source"]),
			Index:     intOf(v["hop_index"]),
			LossPct:   lossPct,
			LossCount: lost,
			Sent:      sent,
		})
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("read hops bucketed: %w", err)
	}
	slices.SortStableFunc(out, func(a, b storage.HopPoint) int {
		if c := a.Time.Compare(b.Time); c != 0 {
			return c
		}
		return cmp.Compare(a.Index, b.Index)
	})
	return out, nil
}

// bucketForResolution maps a storage.Resolution to a date_bin width. The
// raw tier returns 0 to signal "no bucketing — return per-cycle rows".
func bucketForResolution(res storage.Resolution) time.Duration {
	switch res {
	case storage.Resolution5m:
		return 5 * time.Minute
	case storage.Resolution1h:
		return time.Hour
	case storage.Resolution1d:
		return 24 * time.Hour
	default:
		return 0
	}
}

// dateBinInterval renders a duration as the SQL INTERVAL literal DataFusion's
// date_bin() accepts. Day-multiples render as "N day(s)", hour-multiples as
// "N hour(s)", minute-multiples as "N minute(s)", otherwise seconds.
// Singular-vs-plural matters: DataFusion's permissive INTERVAL parser accepts
// both forms in current builds but historically rejected `1 hours` on some
// older releases, so we always render the grammatically correct form.
func dateBinInterval(d time.Duration) string {
	pluralise := func(n int, singular string) string {
		if n == 1 {
			return fmt.Sprintf("%d %s", n, singular)
		}
		return fmt.Sprintf("%d %ss", n, singular)
	}
	switch {
	case d >= 24*time.Hour && d%(24*time.Hour) == 0:
		return pluralise(int(d/(24*time.Hour)), "day")
	case d >= time.Hour && d%time.Hour == 0:
		return pluralise(int(d/time.Hour), "hour")
	case d >= time.Minute && d%time.Minute == 0:
		return pluralise(int(d/time.Minute), "minute")
	default:
		return pluralise(int(d/time.Second), "second")
	}
}

// targetWhereClause is the parameterized WHERE body used by every read path.
// `group` is double-quoted because it collides with the SQL keyword.
// Parameters $group/$target/$from/$to are mandatory; $source is appended only
// when the caller has a non-empty filter.
func targetWhereClause(withSource bool) string {
	clause := `"group" = $group AND target = $target AND time >= $from AND time <= $to`
	if withSource {
		clause += " AND source = $source"
	}
	return clause
}

// baseTargetParams returns the parameter map every read path uses. Times go
// in as RFC3339 strings — DataFusion implicit-casts them to TIMESTAMP for
// the `time` comparison.
func baseTargetParams(ref config.TargetRef, from, to time.Time, source string) influxdb3.QueryParameters {
	p := influxdb3.QueryParameters{
		"group":  ref.Group,
		"target": ref.Target.Name,
		"from":   from.UTC().Format(time.RFC3339Nano),
		"to":     to.UTC().Format(time.RFC3339Nano),
	}
	if source != "" {
		p["source"] = source
	}
	return p
}

// quoteIdent double-quotes a SQL identifier and escapes any embedded quote.
// Used for column names that collide with reserved words ("group") and for
// every column reference in generated SELECTs so a future column rename
// can't accidentally shadow a keyword.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// timeOf coerces a query-result value back to time.Time. Metadata-tagged
// timestamp columns (the InfluxDB-injected `time`) come through pre-converted;
// computed columns from date_bin() arrive as arrow.Timestamp and need an
// explicit conversion. Any other shape is treated as zero rather than panic
// — we'd rather render an obviously-wrong row than crash the API.
func timeOf(v any) time.Time {
	switch x := v.(type) {
	case time.Time:
		return x
	case arrow.Timestamp:
		return x.ToTime(arrow.Nanosecond)
	default:
		return time.Time{}
	}
}

func stringOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// floatOf normalises any of the numeric Arrow types the iterator might
// produce into a float64. Aggregations (AVG, SUM) widen integer fields, so
// the union is broader than a single branch would cover.
func floatOf(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int64:
		return float64(x)
	case int32:
		return float64(x)
	case int:
		return float64(x)
	case uint64:
		return float64(x)
	default:
		return 0
	}
}

// intOf normalises numeric and stringy results into int64. hop_index is a
// tag (string), so the string branch parses on demand.
func intOf(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int32:
		return int64(x)
	case int:
		return int64(x)
	case uint64:
		return int64(x)
	case float64:
		return int64(x)
	case string:
		var n int64
		if _, err := fmt.Sscan(x, &n); err == nil {
			return n
		}
		return 0
	default:
		return 0
	}
}
