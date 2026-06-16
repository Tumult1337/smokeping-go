package clickhouse

import (
	"context"
	"crypto/tls"
	"fmt"
	"runtime"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/storage"
)

// Reader implements storage.Reader against ClickHouse.
type Reader struct {
	conn driver.Conn
}

// NewReader opens a connection. Caller must Close.
func NewReader(ctx context.Context, cfg config.ClickHouse) (*Reader, error) {
	// The UI fires concurrent requests (cycles + rtts + hops for the
	// active target plus latest-hops for the sidebar). Size the pool
	// for at least 2x GOMAXPROCS, floored at 8 so 1-2 vCPU containers
	// can still serve a burst.
	maxConns := max(2*runtime.GOMAXPROCS(0), 8)
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr:         []string{cfg.Addr},
		Auth:         clickhouse.Auth{Database: cfg.Database, Username: cfg.Username, Password: cfg.Password},
		TLS:          tlsForReader(cfg.TLS),
		MaxOpenConns: maxConns,
	})
	if err != nil {
		return nil, err
	}
	if err := conn.Ping(ctx); err != nil {
		conn.Close() //nolint:errcheck // best-effort cleanup after ping failure
		return nil, err
	}
	return &Reader{conn: conn}, nil
}

func (r *Reader) Close() error { return r.conn.Close() }

func tlsForReader(enabled bool) *tls.Config {
	if !enabled {
		return nil
	}
	return &tls.Config{MinVersion: tls.VersionTLS12}
}

func (r *Reader) QueryCycles(ctx context.Context, ref config.TargetRef, from, to time.Time, f storage.QueryFilter) ([]storage.CyclePoint, error) {
	step := f.Step
	if step == 0 {
		return r.queryCyclesRaw(ctx, ref, from, to, f.Source)
	}
	return r.queryCyclesBucketed(ctx, ref, from, to, f.Source, step)
}

// sourceFilter builds the optional source-predicate clause and its bound
// argument. Returns ("", nil) when no filter is wanted — the caller's
// WHERE clause then has a static shape so CH can use the ORDER BY's
// secondary sort key (source) for granule pruning instead of scanning
// every granule looking for a runtime-empty branch.
func sourceFilter(source string) (clause string, args []any) {
	if source == "" {
		return "", nil
	}
	return "\n  AND source = ?", []any{source}
}

func (r *Reader) queryCyclesRaw(ctx context.Context, ref config.TargetRef, from, to time.Time, source string) ([]storage.CyclePoint, error) {
	srcClause, srcArgs := sourceFilter(source)
	q := `
SELECT timestamp, source,
       rtt_min_ms, rtt_max_ms, rtt_mean_ms, rtt_median_ms, rtt_stddev_ms,
       p5_ms, p10_ms, p15_ms, p20_ms, p25_ms,
       p30_ms, p35_ms, p40_ms, p45_ms, p55_ms,
       p60_ms, p65_ms, p70_ms, p75_ms, p80_ms,
       p85_ms, p90_ms, p95_ms,
       loss_pct, lost, sent
FROM probe_cycle
WHERE target_id = ?
  AND timestamp >= ? AND timestamp < ?` + srcClause + `
ORDER BY timestamp`
	args := append([]any{ref.Target.Name, from, to}, srcArgs...)
	rows, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query cycles raw: %w", err)
	}
	defer rows.Close() //nolint:errcheck // rows.Err() returned below captures any close-time error

	var out []storage.CyclePoint
	for rows.Next() {
		var p storage.CyclePoint
		var lossPct float32
		var lost, sent uint16
		var min, max, mean, median, stddev float64
		var p5, p10, p15, p20, p25, p30, p35, p40, p45 float64
		var p55, p60, p65, p70, p75, p80, p85, p90, p95 float64
		if err := rows.Scan(
			&p.Time, &p.Source,
			&min, &max, &mean, &median, &stddev,
			&p5, &p10, &p15, &p20, &p25,
			&p30, &p35, &p40, &p45, &p55,
			&p60, &p65, &p70, &p75, &p80,
			&p85, &p90, &p95,
			&lossPct, &lost, &sent,
		); err != nil {
			return nil, err
		}
		p.Min = min
		p.Max = max
		p.Mean = mean
		p.Median = median
		p.StdDev = stddev
		p.P5 = p5
		p.P10 = p10
		p.P15 = p15
		p.P20 = p20
		p.P25 = p25
		p.P30 = p30
		p.P35 = p35
		p.P40 = p40
		p.P45 = p45
		p.P55 = p55
		p.P60 = p60
		p.P65 = p65
		p.P70 = p70
		p.P75 = p75
		p.P80 = p80
		p.P85 = p85
		p.P90 = p90
		p.P95 = p95
		p.LossPct = float64(lossPct)
		p.LossCount = int64(lost)
		p.Sent = int64(sent)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *Reader) queryCyclesBucketed(ctx context.Context, ref config.TargetRef, from, to time.Time, source string, step time.Duration) ([]storage.CyclePoint, error) {
	srcClause, srcArgs := sourceFilter(source)
	// A 100%-loss cycle stores all-zero percentile columns (stats.Compute over
	// an empty RTT slice). Weighting the quantile rollup by `sent` folds those
	// zeros into the distribution, collapsing the bucket's low percentiles to
	// 0 — in log-scale bars that paints a full-height band down to the floor.
	// Weight by received pings (`sent - lost`) instead: a 100%-loss sub-cycle
	// gets weight 0 and drops out, so only cycles that actually measured an RTT
	// shape the percentile band. Loss is reported separately and is unaffected.
	// quantilesExactWeighted needs an unsigned weight (UInt16-UInt16 promotes to
	// Int32), hence toUInt64. min/mean/stddev get the same treatment; a bucket
	// where every sub-cycle was 100% loss has zero total received, so the
	// avgWeighted (mean/stddev) is NaN-guarded — NaN would break JSON encoding,
	// and the value is moot anyway (the UI skips 100%-loss buckets).
	q := fmt.Sprintf(`
SELECT toStartOfInterval(timestamp, INTERVAL %d SECOND)   AS bucket_ts,
       source                                              AS src,
       minIf(rtt_min_ms, sent > lost), max(rtt_max_ms),
       if(sum(sent) = sum(lost), 0, avgWeighted(rtt_mean_ms, toUInt64(sent - lost))),
       quantilesExactWeighted(0.50)(rtt_median_ms, toUInt64(sent - lost))[1] AS rtt_median_ms,
       if(sum(sent) = sum(lost), 0, sqrt(avgWeighted(pow(rtt_stddev_ms, 2), toUInt64(sent - lost)))) AS rtt_stddev_ms,
       quantilesExactWeighted(0.05)(p5_ms, toUInt64(sent - lost))[1],
       quantilesExactWeighted(0.10)(p10_ms, toUInt64(sent - lost))[1],
       quantilesExactWeighted(0.15)(p15_ms, toUInt64(sent - lost))[1],
       quantilesExactWeighted(0.20)(p20_ms, toUInt64(sent - lost))[1],
       quantilesExactWeighted(0.25)(p25_ms, toUInt64(sent - lost))[1],
       quantilesExactWeighted(0.30)(p30_ms, toUInt64(sent - lost))[1],
       quantilesExactWeighted(0.35)(p35_ms, toUInt64(sent - lost))[1],
       quantilesExactWeighted(0.40)(p40_ms, toUInt64(sent - lost))[1],
       quantilesExactWeighted(0.45)(p45_ms, toUInt64(sent - lost))[1],
       quantilesExactWeighted(0.55)(p55_ms, toUInt64(sent - lost))[1],
       quantilesExactWeighted(0.60)(p60_ms, toUInt64(sent - lost))[1],
       quantilesExactWeighted(0.65)(p65_ms, toUInt64(sent - lost))[1],
       quantilesExactWeighted(0.70)(p70_ms, toUInt64(sent - lost))[1],
       quantilesExactWeighted(0.75)(p75_ms, toUInt64(sent - lost))[1],
       quantilesExactWeighted(0.80)(p80_ms, toUInt64(sent - lost))[1],
       quantilesExactWeighted(0.85)(p85_ms, toUInt64(sent - lost))[1],
       quantilesExactWeighted(0.90)(p90_ms, toUInt64(sent - lost))[1],
       quantilesExactWeighted(0.95)(p95_ms, toUInt64(sent - lost))[1],
       if(sum(sent) = 0, 0, 100.0 * sum(lost) / sum(sent)) AS loss_pct,
       sum(lost), sum(sent)
FROM probe_cycle
WHERE target_id = ?
  AND timestamp >= ? AND timestamp < ?%s
GROUP BY bucket_ts, source
ORDER BY bucket_ts, source`, int(step.Seconds()), srcClause)
	args := append([]any{ref.Target.Name, from, to}, srcArgs...)
	rows, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query cycles bucketed: %w", err)
	}
	defer rows.Close() //nolint:errcheck // rows.Err() returned below captures any close-time error

	var out []storage.CyclePoint
	for rows.Next() {
		var p storage.CyclePoint
		var lossPct float64
		var lost, sent uint64
		var min, max, mean, median, stddev float64
		var p5, p10, p15, p20, p25, p30, p35, p40, p45 float64
		var p55, p60, p65, p70, p75, p80, p85, p90, p95 float64
		if err := rows.Scan(
			&p.Time, &p.Source,
			&min, &max, &mean, &median, &stddev,
			&p5, &p10, &p15, &p20, &p25,
			&p30, &p35, &p40, &p45, &p55,
			&p60, &p65, &p70, &p75, &p80,
			&p85, &p90, &p95,
			&lossPct, &lost, &sent,
		); err != nil {
			return nil, err
		}
		p.Min = min
		p.Max = max
		p.Mean = mean
		p.Median = median
		p.StdDev = stddev
		p.P5 = p5
		p.P10 = p10
		p.P15 = p15
		p.P20 = p20
		p.P25 = p25
		p.P30 = p30
		p.P35 = p35
		p.P40 = p40
		p.P45 = p45
		p.P55 = p55
		p.P60 = p60
		p.P65 = p65
		p.P70 = p70
		p.P75 = p75
		p.P80 = p80
		p.P85 = p85
		p.P90 = p90
		p.P95 = p95
		p.LossPct = lossPct
		p.LossCount = int64(lost)
		p.Sent = int64(sent)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *Reader) QueryRTTs(ctx context.Context, ref config.TargetRef, from, to time.Time, f storage.QueryFilter) ([]storage.RTTPoint, error) {
	srcClause, srcArgs := sourceFilter(f.Source)
	q := `
SELECT timestamp, rtt_ms, seq
FROM probe_rtt
WHERE target_id = ?
  AND timestamp >= ? AND timestamp < ?` + srcClause + `
ORDER BY timestamp, seq`
	args := append([]any{ref.Target.Name, from, to}, srcArgs...)
	rows, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query rtts: %w", err)
	}
	defer rows.Close() //nolint:errcheck // rows.Err() returned below captures any close-time error
	var out []storage.RTTPoint
	for rows.Next() {
		var p storage.RTTPoint
		var seq uint16
		if err := rows.Scan(&p.Time, &p.RTT, &seq); err != nil {
			return nil, err
		}
		p.Seq = int64(seq)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *Reader) QueryHTTPSamples(ctx context.Context, ref config.TargetRef, from, to time.Time, f storage.QueryFilter) ([]storage.HTTPPoint, error) {
	srcClause, srcArgs := sourceFilter(f.Source)
	q := `
SELECT timestamp, source, rtt_ms, status, seq, error
FROM probe_http
WHERE target_id = ?
  AND timestamp >= ? AND timestamp < ?` + srcClause + `
ORDER BY timestamp, seq`
	args := append([]any{ref.Target.Name, from, to}, srcArgs...)
	rows, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query http: %w", err)
	}
	defer rows.Close() //nolint:errcheck // rows.Err() returned below captures any close-time error
	var out []storage.HTTPPoint
	for rows.Next() {
		var p storage.HTTPPoint
		var status uint16
		var seq uint16
		if err := rows.Scan(&p.Time, &p.Source, &p.RTT, &status, &seq, &p.Err); err != nil {
			return nil, err
		}
		p.Status = int64(status)
		p.Seq = int64(seq)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *Reader) QueryLatestHops(ctx context.Context, ref config.TargetRef, f storage.QueryFilter) ([]storage.HopPoint, error) {
	srcClause, srcArgs := sourceFilter(f.Source)
	// Optional staleness floor: bounding the CTE's max() to rows at or after
	// the cutoff means a source whose newest row predates it produces no group
	// row and drops out of the response — so a removed/stopped probe origin
	// stops rendering as a live path. The bound also prunes the scan. The
	// outer join needs no bound: it matches the exact (source, ts) pairs the
	// CTE emitted, which are already at or after the cutoff.
	freshClause := ""
	if !f.LatestSince.IsZero() {
		freshClause = " AND timestamp >= ?"
	}
	// Latest cycle PER SOURCE, not a single global max(timestamp). Without
	// GROUP BY source, the CTE returns whichever source happened to flush
	// most recently and every other source's path disappears from the
	// all-view — the UI then renders one randomly-chosen source's latest
	// cycle. With the per-source group the response carries one cycle per
	// origin, matching what QueryHopsAt does for the historical-pin path.
	q := `
WITH latest AS (
  SELECT source, max(timestamp) AS ts
  FROM probe_hop
  WHERE target_id = ?` + srcClause + freshClause + `
  GROUP BY source
)
SELECT timestamp, source, ttl, hop_addr,
       rtt_min_ms, rtt_max_ms, rtt_mean_ms, rtt_median_ms,
       loss_pct, lost, sent
FROM probe_hop
WHERE target_id = ?` + srcClause + `
  AND (source, timestamp) IN (SELECT source, ts FROM latest)
ORDER BY source, ttl`
	// args layout: CTE filter (target + opt source + opt freshness), outer filter (target + opt source).
	args := []any{ref.Target.Name}
	args = append(args, srcArgs...)
	if !f.LatestSince.IsZero() {
		args = append(args, f.LatestSince)
	}
	args = append(args, ref.Target.Name)
	args = append(args, srcArgs...)
	rows, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query latest hops: %w", err)
	}
	defer rows.Close() //nolint:errcheck // scanHopRows returns rows.Err() which covers close errors
	return scanHopRows(rows)
}

func (r *Reader) QueryHopsAt(ctx context.Context, ref config.TargetRef, at time.Time, window time.Duration, f storage.QueryFilter) ([]storage.HopPoint, error) {
	half := window / 2
	srcClause, srcArgs := sourceFilter(f.Source)
	// Pick the single cycle per source nearest to `at` via argMin, then
	// return every row at that exact timestamp. A naive
	// `ORDER BY abs(dt) LIMIT N` is wrong: it spans whichever N
	// cycles happen to be closest, so the UI's "hops at this cycle"
	// table ends up rendering a stack of consecutive cycles instead
	// of one. With argMin we pin per-source and let the IN-list join
	// pull only those rows.
	q := `
WITH nearest AS (
  SELECT source,
         argMin(timestamp, abs(dateDiff('millisecond', timestamp, toDateTime64(?, 3, 'UTC')))) AS ts
  FROM probe_hop
  WHERE target_id = ?` + srcClause + `
    AND timestamp >= ? AND timestamp < ?
  GROUP BY source
)
SELECT timestamp, source, ttl, hop_addr,
       rtt_min_ms, rtt_max_ms, rtt_mean_ms, rtt_median_ms,
       loss_pct, lost, sent
FROM probe_hop
WHERE target_id = ?` + srcClause + `
  AND (source, timestamp) IN (SELECT source, ts FROM nearest)
ORDER BY source, ttl`
	// args layout: CTE — `at` (the centre), target, optional source, from, to;
	//              outer — target, optional source.
	args := []any{at, ref.Target.Name}
	args = append(args, srcArgs...)
	args = append(args, at.Add(-half), at.Add(half))
	args = append(args, ref.Target.Name)
	args = append(args, srcArgs...)
	rows, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query hops at: %w", err)
	}
	defer rows.Close() //nolint:errcheck // scanHopRows returns rows.Err() which covers close errors
	return scanHopRows(rows)
}

// scanHopRows is shared by QueryLatestHops, QueryHopsAt, and the raw
// path of QueryHopsTimeline (added in T15). Returns rows in the order
// they came from the cursor. Raw rows are one cycle each, so MaxLossPct
// is just a mirror of LossPct — set here so consumers can read the field
// uniformly without branching on bucketed vs raw.
func scanHopRows(rows driver.Rows) ([]storage.HopPoint, error) {
	var out []storage.HopPoint
	for rows.Next() {
		var p storage.HopPoint
		var ttl uint8
		var lossPct float32
		var lost, sent uint16
		var min, max, mean, median float64
		if err := rows.Scan(
			&p.Time, &p.Source, &ttl, &p.IP,
			&min, &max, &mean, &median,
			&lossPct, &lost, &sent,
		); err != nil {
			return nil, err
		}
		p.Index = int64(ttl)
		p.Min = min
		p.Max = max
		p.Mean = mean
		p.Median = median
		p.LossPct = float64(lossPct)
		p.MaxLossPct = p.LossPct
		p.WorstTime = p.Time // raw rows are one cycle each
		p.LossCount = int64(lost)
		p.Sent = int64(sent)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *Reader) QueryHopsTimeline(ctx context.Context, ref config.TargetRef, from, to time.Time, f storage.QueryFilter) ([]storage.HopPoint, error) {
	step := f.Step
	if step == 0 {
		return r.queryHopsRaw(ctx, ref, from, to, f.Source)
	}
	return r.queryHopsBucketed(ctx, ref, from, to, f.Source, step)
}

func (r *Reader) queryHopsRaw(ctx context.Context, ref config.TargetRef, from, to time.Time, source string) ([]storage.HopPoint, error) {
	srcClause, srcArgs := sourceFilter(source)
	q := `
SELECT timestamp, source, ttl, hop_addr,
       rtt_min_ms, rtt_max_ms, rtt_mean_ms, rtt_median_ms,
       loss_pct, lost, sent
FROM probe_hop
WHERE target_id = ?
  AND timestamp >= ? AND timestamp < ?` + srcClause + `
ORDER BY timestamp, ttl`
	args := append([]any{ref.Target.Name, from, to}, srcArgs...)
	rows, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query hops raw: %w", err)
	}
	defer rows.Close() //nolint:errcheck // scanHopRows returns rows.Err() which covers close errors
	return scanHopRows(rows)
}

func (r *Reader) queryHopsBucketed(ctx context.Context, ref config.TargetRef, from, to time.Time, source string, step time.Duration) ([]storage.HopPoint, error) {
	srcClause, srcArgs := sourceFilter(source)
	// max(loss_pct) preserves brief 100%-loss spikes that the bucket-average
	// (sum(lost)/sum(sent)) dilutes — at a 5-min bucket with 10 per-cycle
	// rows, a single 100%-loss cycle averages to 10% and disappears against
	// the heatmap's loss palette. The heatmap colors cells by this max so
	// the spike survives bucketing.
	//
	// Aliases use `avg_loss_pct` / `max_loss_pct` (not `loss_pct`) to avoid
	// shadowing the underlying `loss_pct` column: a select-list alias of
	// `loss_pct` would make `max(loss_pct)` aggregate the alias (an aggregate
	// itself) and ClickHouse rejects "aggregate function inside aggregate
	// function" with a 500.
	q := fmt.Sprintf(`
SELECT toStartOfInterval(timestamp, INTERVAL %d SECOND) AS bucket_ts,
       source                                            AS src,
       ttl,
       hop_addr,
       sum(sent)                                         AS total_sent,
       sum(lost)                                         AS total_lost,
       if(sum(sent) = 0, 0, 100.0 * sum(lost) / sum(sent)) AS avg_loss_pct,
       max(loss_pct)                                     AS max_loss_pct,
       argMax(timestamp, loss_pct)                       AS worst_ts
FROM probe_hop
WHERE target_id = ?
  AND timestamp >= ? AND timestamp < ?%s
GROUP BY bucket_ts, source, ttl, hop_addr
ORDER BY bucket_ts, source, ttl`, int(step.Seconds()), srcClause)
	args := append([]any{ref.Target.Name, from, to}, srcArgs...)
	rows, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query hops bucketed: %w", err)
	}
	defer rows.Close() //nolint:errcheck // rows.Err() returned below captures any close-time error
	var out []storage.HopPoint
	for rows.Next() {
		var p storage.HopPoint
		var ttl uint8
		var sent, lost uint64
		var lossPct float64
		var maxLossPct float32
		var worstTs time.Time
		if err := rows.Scan(&p.Time, &p.Source, &ttl, &p.IP, &sent, &lost, &lossPct, &maxLossPct, &worstTs); err != nil {
			return nil, err
		}
		p.Index = int64(ttl)
		p.Sent = int64(sent)
		p.LossCount = int64(lost)
		p.LossPct = lossPct
		p.MaxLossPct = float64(maxLossPct)
		p.WorstTime = worstTs
		out = append(out, p)
	}
	return out, rows.Err()
}
