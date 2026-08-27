package clickhouse

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"math"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/storage"
)

// Reader implements storage.Reader against ClickHouse.
type Reader struct {
	conn driver.Conn
	// logger is nil on the Readers tests construct directly; log() falls back
	// so a diagnostic path can never be the thing that panics a read.
	logger *slog.Logger
}

// WithLogger sets the logger the reader's diagnostics use. Separate from
// NewReader so a caller that has no logger — every test that builds a Reader
// around a fake conn — is not made to invent one.
func (r *Reader) WithLogger(log *slog.Logger) *Reader {
	r.logger = log
	return r
}

func (r *Reader) log() *slog.Logger {
	if r.logger == nil {
		return slog.Default()
	}
	return r.logger
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

// dtMilli renders one DateTime64(3) predicate bound. Every timestamp bound in
// this package goes through it because clickhouse-go renders a bound
// time.Time at whole-second precision, which moves a window edge by up to a
// second in whichever direction admits the wrong rows.
const dtMilli = "fromUnixTimestamp64Milli(?, 'UTC')"

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
       rtt_min_us / 1000.0, rtt_max_us / 1000.0, rtt_mean_us / 1000.0, rtt_median_us / 1000.0, rtt_stddev_us / 1000.0,
       p5_us / 1000.0, p10_us / 1000.0, p15_us / 1000.0, p20_us / 1000.0, p25_us / 1000.0,
       p30_us / 1000.0, p35_us / 1000.0, p40_us / 1000.0, p45_us / 1000.0, p55_us / 1000.0,
       p60_us / 1000.0, p65_us / 1000.0, p70_us / 1000.0, p75_us / 1000.0, p80_us / 1000.0,
       p85_us / 1000.0, p90_us / 1000.0, p95_us / 1000.0,
       loss_pct, lost, sent
FROM probe_cycle
WHERE target_id = ?
  AND target_group = ?
  AND timestamp >= ` + dtMilli + ` AND timestamp < ` + dtMilli + srcClause + `
ORDER BY timestamp`
	args := append([]any{ref.Target.Name, ref.Group, from.UnixMilli(), to.UnixMilli()}, srcArgs...)
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
       minIf(rtt_min_us, sent > lost) / 1000.0, max(rtt_max_us) / 1000.0,
       if(sum(sent) = sum(lost), 0, avgWeighted(rtt_mean_us, toUInt64(sent - lost)) / 1000.0),
       quantilesExactWeighted(0.50)(rtt_median_us, toUInt64(sent - lost))[1] / 1000.0 AS rtt_median_ms,
       if(sum(sent) = sum(lost), 0, sqrt(avgWeighted(pow(rtt_stddev_us, 2), toUInt64(sent - lost))) / 1000.0) AS rtt_stddev_ms,
       quantilesExactWeighted(0.05)(p5_us, toUInt64(sent - lost))[1] / 1000.0,
       quantilesExactWeighted(0.10)(p10_us, toUInt64(sent - lost))[1] / 1000.0,
       quantilesExactWeighted(0.15)(p15_us, toUInt64(sent - lost))[1] / 1000.0,
       quantilesExactWeighted(0.20)(p20_us, toUInt64(sent - lost))[1] / 1000.0,
       quantilesExactWeighted(0.25)(p25_us, toUInt64(sent - lost))[1] / 1000.0,
       quantilesExactWeighted(0.30)(p30_us, toUInt64(sent - lost))[1] / 1000.0,
       quantilesExactWeighted(0.35)(p35_us, toUInt64(sent - lost))[1] / 1000.0,
       quantilesExactWeighted(0.40)(p40_us, toUInt64(sent - lost))[1] / 1000.0,
       quantilesExactWeighted(0.45)(p45_us, toUInt64(sent - lost))[1] / 1000.0,
       quantilesExactWeighted(0.55)(p55_us, toUInt64(sent - lost))[1] / 1000.0,
       quantilesExactWeighted(0.60)(p60_us, toUInt64(sent - lost))[1] / 1000.0,
       quantilesExactWeighted(0.65)(p65_us, toUInt64(sent - lost))[1] / 1000.0,
       quantilesExactWeighted(0.70)(p70_us, toUInt64(sent - lost))[1] / 1000.0,
       quantilesExactWeighted(0.75)(p75_us, toUInt64(sent - lost))[1] / 1000.0,
       quantilesExactWeighted(0.80)(p80_us, toUInt64(sent - lost))[1] / 1000.0,
       quantilesExactWeighted(0.85)(p85_us, toUInt64(sent - lost))[1] / 1000.0,
       quantilesExactWeighted(0.90)(p90_us, toUInt64(sent - lost))[1] / 1000.0,
       quantilesExactWeighted(0.95)(p95_us, toUInt64(sent - lost))[1] / 1000.0,
       if(sum(sent) = 0, 0, 100.0 * sum(lost) / sum(sent)) AS loss_pct,
       sum(lost), sum(sent)
FROM probe_cycle
WHERE target_id = ?
  AND target_group = ?
  AND timestamp >= `+dtMilli+` AND timestamp < `+dtMilli+`%s
GROUP BY bucket_ts, source
ORDER BY bucket_ts, source`, int(step.Seconds()), srcClause)
	args := append([]any{ref.Target.Name, ref.Group, from.UnixMilli(), to.UnixMilli()}, srcArgs...)
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
  AND target_group = ?
  AND timestamp >= ` + dtMilli + ` AND timestamp < ` + dtMilli + srcClause + `
ORDER BY timestamp, seq`
	args := append([]any{ref.Target.Name, ref.Group, from.UnixMilli(), to.UnixMilli()}, srcArgs...)
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
		// Rows this binary wrote as NaN for a 0µs RTT before rttMS clamped
		// them; a non-finite value breaks the JSON encoder mid-response.
		if math.IsNaN(p.RTT) || math.IsInf(p.RTT, 0) {
			continue
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
  AND target_group = ?
  AND timestamp >= ` + dtMilli + ` AND timestamp < ` + dtMilli + srcClause + `
ORDER BY timestamp, seq`
	args := append([]any{ref.Target.Name, ref.Group, from.UnixMilli(), to.UnixMilli()}, srcArgs...)
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

// maxHopSources mirrors master's unexported maxRegisteredSlaves and is the
// live fleet a pinned read must always serve, not a bound on what one can
// return: QueryLatestHops groups by source over probe_hop itself, so its
// group count is every label the table still holds within that TTL, which
// operator churn raises without limit.
const maxHopSources = 512

// maxHopRows is a memory ceiling rather than a product of producer limits,
// because the source count above them is unbounded: at MaxHopAddrLen per
// hop_addr it holds one unauthenticated pinned read to ~36.9 MB, and it stays
// clear of the rows a full live fleet needs.
const maxHopRows = 485_280

// maxHopTTLs is the ttl column's whole domain: probe_hop stores it as UInt8
// and cluster ingest refuses an index outside [0, 255], so nothing a source
// can push puts more distinct TTLs on one grid slot.
const maxHopTTLs = 256

// maxHopTimelineBuckets is the widest grid /hops/timeline can ask for, and
// storage owns it because the step ladder there is what has to fit inside it.
const maxHopTimelineBuckets = storage.MaxHopGridSlots

// maxHopTimelineRows is that grid's whole product, and the timeline's ceiling:
// one probe origin per request, one row per (slot, ttl). The widest window
// reaches it exactly — 673 off-grid slots at the 15m tier, every one of the
// ttl column's 256 values — and is served, because the refusal is past
// equality; the product is the ceiling of a schema-legal result rather than
// an estimate of a typical one.
const maxHopTimelineRows = maxHopTimelineBuckets * maxHopTTLs

// hopRowCap is maxHopRows and hopTimelineRowCap is maxHopTimelineRows, held
// as vars so a test can lower one and drive a refusal without materialising
// the real row count.
var (
	hopRowCap         = maxHopRows
	hopTimelineRowCap = maxHopTimelineRows
)

// hopRowLimit is appended to every hop query rather than declared per query,
// so a new hop read cannot inherit the unbounded shape by omission. It asks
// for one row past the cap so reaching it is distinguishable from ending on it.
func hopRowLimit(cap int) string {
	return fmt.Sprintf("\nLIMIT %d", cap+1)
}

// hopRowsWithinCap refuses a result that reached the cap instead of returning
// its prefix. Hop reads order oldest-first, so a truncated path history is
// missing its newest rows and reads as a probe that stopped — an
// incident-shaped lie on the endpoint an operator opens during an incident.
func hopRowsWithinCap(cap int, out []storage.HopPoint, err error) ([]storage.HopPoint, error) {
	if err != nil {
		return nil, err
	}
	if len(out) > cap {
		return nil, storage.ErrHopsTruncated
	}
	return out, nil
}

// maxFutureSkewSeconds mirrors the ingest ceiling on the read side. Rows a
// slave wrote ahead of the master's clock before that bound existed are still
// in the table, and each one still pins itself as its source's newest until
// the lie expires; the CTE stops considering them.
var maxFutureSkewSeconds = strconv.Itoa(int(config.MaxFutureSkew / time.Second))

func (r *Reader) QueryLatestHops(ctx context.Context, ref config.TargetRef, f storage.QueryFilter) (storage.HopsResult, error) {
	srcClause, srcArgs := sourceFilter(f.Source)
	// Optional staleness floor: bounding the CTE's max() to rows at or after
	// the cutoff means a source whose newest row predates it produces no group
	// row and drops out of the response — so a removed/stopped probe origin
	// stops rendering as a live path. The bound also prunes the scan. The
	// outer join needs no bound: it matches the exact (source, ts) pairs the
	// CTE emitted, which are already at or after the cutoff.
	freshClause := ""
	if !f.LatestSince.IsZero() {
		freshClause = " AND timestamp >= " + dtMilli
	}
	// Latest cycle PER SOURCE, not a single global max(timestamp). Without
	// GROUP BY source, the CTE returns whichever source happened to flush
	// most recently and every other source's path disappears from the
	// all-view — the UI then renders one randomly-chosen source's latest
	// cycle. With the per-source group the response carries one cycle per
	// origin, matching what QueryHopsAt does for the historical-pin path.
	cte := `
WITH pinned AS (
  SELECT source, max(timestamp) AS ts
  FROM probe_hop
  WHERE target_id = ?
    AND target_group = ?` + srcClause + freshClause + `
    AND timestamp <= now() + INTERVAL ` + maxFutureSkewSeconds + ` SECOND
  GROUP BY source
)`
	// args layout: CTE filter (target id+group, opt source, opt freshness), outer filter (target id+group, opt source).
	args := []any{ref.Target.Name, ref.Group}
	args = append(args, srcArgs...)
	if !f.LatestSince.IsZero() {
		args = append(args, f.LatestSince.UnixMilli())
	}
	args = append(args, ref.Target.Name, ref.Group)
	args = append(args, srcArgs...)
	return r.pinnedHopRows(ctx, ref, "query latest hops", cte, srcClause, args)
}

// pinnedHopRows is the tail both pinned reads share: the hop rows at exactly
// the (source, ts) pairs the caller's `pinned` CTE selected, paired with those
// cycles' own round counters.
func (r *Reader) pinnedHopRows(ctx context.Context, ref config.TargetRef, what, cte, srcClause string, args []any) (storage.HopsResult, error) {
	q := cte + `
SELECT timestamp, source, ttl, hop_addr, unreach, target_reply,
       rtt_min_us / 1000.0, rtt_max_us / 1000.0, rtt_mean_us / 1000.0, rtt_median_us / 1000.0,
       loss_pct, lost, sent
FROM probe_hop
WHERE target_id = ?
  AND target_group = ?` + srcClause + `
  AND (source, timestamp) IN (SELECT source, ts FROM pinned)
ORDER BY source, ttl` + hopRowLimit(hopRowCap)
	rows, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return storage.HopsResult{}, fmt.Errorf("%s: %w", what, err)
	}
	defer rows.Close() //nolint:errcheck // scanHopRows returns rows.Err() which covers close errors
	hops, err := scanHopRows(rows)
	return r.withCycleCounters(ctx, ref, hops, err)
}

func (r *Reader) QueryHopsAt(ctx context.Context, ref config.TargetRef, at time.Time, window time.Duration, f storage.QueryFilter) (storage.HopsResult, error) {
	half := window / 2
	srcClause, srcArgs := sourceFilter(f.Source)
	// Pick the single cycle per source nearest to `at` via argMin, then
	// return every row at that exact timestamp. A naive
	// `ORDER BY abs(dt) LIMIT N` is wrong: it spans whichever N
	// cycles happen to be closest, so the UI's "hops at this cycle"
	// table ends up rendering a stack of consecutive cycles instead
	// of one. With argMin we pin per-source and let the IN-list join
	// pull only those rows.
	//
	// Centre and both window edges are dtMilli bounds: a centre rounded off
	// the cycle it names leaves a neighbouring cycle nearer than that cycle,
	// and edges rounded the other way make that neighbour the only candidate.
	// The now() ceiling mirrors QueryLatestHops': ingest stops new
	// future-dated rows, this keeps ones already in the table off the pin.
	cte := `
WITH pinned AS (
  SELECT source,
         argMin(timestamp, abs(dateDiff('millisecond', timestamp, ` + dtMilli + `))) AS ts
  FROM probe_hop
  WHERE target_id = ?
    AND target_group = ?` + srcClause + `
    AND timestamp >= ` + dtMilli + ` AND timestamp < ` + dtMilli + `
    AND timestamp <= now() + INTERVAL ` + maxFutureSkewSeconds + ` SECOND
  GROUP BY source
)`
	// args layout: CTE — `at` (the centre), target id+group, optional source, from, to;
	//              outer — target id+group, optional source.
	args := []any{at.UnixMilli(), ref.Target.Name, ref.Group}
	args = append(args, srcArgs...)
	args = append(args, at.Add(-half).UnixMilli(), at.Add(half).UnixMilli())
	args = append(args, ref.Target.Name, ref.Group)
	args = append(args, srcArgs...)
	return r.pinnedHopRows(ctx, ref, "query hops at", cte, srcClause, args)
}

// maxCycleCounterKeys bounds the (source, cycle) pairs one hop read asks
// probe_cycle about, each costing two bound arguments in the IN set. It is
// twice the live names the registry admits — a bound the pinned reads
// themselves do not have, which is why maxHopRows stopped being derived that
// way — and it is sound here only because it **trims rather than refuses**: it
// governs this query alone, a missing counter already renders as unknown loss
// by contract, and letting it fail a path view whose hop rows are present and
// correct is the worse answer. Extreme source churn therefore costs target
// loss on the oldest cycles, never the read.
const maxCycleCounterKeys = 2 * maxHopSources

// withCycleCounters pairs a pinned hop read with the round counters of the
// cycles it selected. Target loss is per cycle and cannot be recovered from
// hop rows, so the two travel together rather than leaving a caller to derive
// one from the other.
func (r *Reader) withCycleCounters(ctx context.Context, ref config.TargetRef, hops []storage.HopPoint, err error) (storage.HopsResult, error) {
	if err != nil {
		return storage.HopsResult{}, err
	}
	cycles, err := r.queryCycleCounters(ctx, ref, hops)
	if err != nil {
		return storage.HopsResult{}, err
	}
	return storage.HopsResult{Hops: hops, Cycles: cycles}, nil
}

// queryCycleCounters reads probe_cycle at exactly the (source, timestamp)
// pairs the hop rows carry. A cycle that sent nothing wrote no row there, so a
// source can legitimately come back without counters.
func (r *Reader) queryCycleCounters(ctx context.Context, ref config.TargetRef, hops []storage.HopPoint) ([]storage.CycleCounters, error) {
	keys := cycleKeys(hops)
	if len(keys) == 0 {
		return nil, nil
	}
	// Past the cap the counters are trimmed, not the read refused. A hop
	// prefix reads as a probe that stopped, which is why hop rows refuse
	// instead — but a missing counter already renders as unknown loss by
	// contract, so degrading the answer beats failing a path view whose rows
	// are all present and correct.
	if len(keys) > maxCycleCounterKeys {
		// Newest first before trimming: cycleKeys preserves the hop query's
		// ORDER BY source, so taking a prefix kept whichever sources sort
		// first alphabetically rather than whichever cycles an operator is
		// looking at.
		slices.SortStableFunc(keys, func(a, b storage.CycleCounters) int {
			return b.Time.Compare(a.Time)
		})
		r.log().Warn("hop read exceeded its cycle-counter budget, target loss omitted for the excess",
			"target", ref.Target.Name, "group", ref.Group,
			"sources", len(keys), "budget", maxCycleCounterKeys)
		keys = keys[:maxCycleCounterKeys]
	}
	// The range bound is redundant with the IN set and exists so the primary
	// key still prunes.
	pairs := make([]string, len(keys))
	first, last := keys[0].Time.UnixMilli(), keys[0].Time.UnixMilli()
	tuples := make([]any, 0, 2*len(keys))
	for i, k := range keys {
		ms := k.Time.UnixMilli()
		pairs[i] = "(?, " + dtMilli + ")"
		tuples = append(tuples, k.Source, ms)
		first = min(first, ms)
		last = max(last, ms)
	}
	args := append([]any{ref.Target.Name, ref.Group, first, last}, tuples...)
	// GROUP BY, not raw rows: ingestion is at-least-once and probe_cycle is an
	// ordinary MergeTree, so a requeued push leaves the same cycle twice and a
	// LIMIT sized by the key count would spend it on one source's duplicates.
	q := `
SELECT source, timestamp, any(sent), any(lost), any(loss_pct)
FROM probe_cycle
WHERE target_id = ?
  AND target_group = ?
  AND timestamp >= ` + dtMilli + `
  AND timestamp <= ` + dtMilli + `
  AND (source, timestamp) IN (` + strings.Join(pairs, ", ") + `)
GROUP BY source, timestamp
LIMIT ` + strconv.Itoa(len(keys))
	rows, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query cycle counters: %w", err)
	}
	defer rows.Close() //nolint:errcheck // rows.Err() returned below captures any close-time error
	var out []storage.CycleCounters
	for rows.Next() {
		var c storage.CycleCounters
		var sent, lost uint16
		var lossPct float32
		if err := rows.Scan(&c.Source, &c.Time, &sent, &lost, &lossPct); err != nil {
			return nil, err
		}
		c.Sent = int64(sent)
		c.LossCount = int64(lost)
		c.LossPct = float64(lossPct)
		out = append(out, c)
	}
	return out, rows.Err()
}

// cycleKeys reduces hop rows to the distinct cycles they came from, in the
// order first seen.
func cycleKeys(hops []storage.HopPoint) []storage.CycleCounters {
	type key struct {
		source string
		ts     int64
	}
	seen := make(map[key]struct{}, 8)
	var out []storage.CycleCounters
	for _, h := range hops {
		k := key{h.Source, h.Time.UnixMilli()}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, storage.CycleCounters{Source: h.Source, Time: h.Time})
	}
	return out
}

// scanHopRows is shared by the pinned reads, QueryLatestHops and
// QueryHopsAt; the hop timeline buckets at every tier and scans its own rows.
// Returns them in cursor order, mirroring LossPct into MaxLossPct so a
// consumer reads one field for a pinned cycle and a bucket alike.
func scanHopRows(rows driver.Rows) ([]storage.HopPoint, error) {
	var out []storage.HopPoint
	for rows.Next() {
		var p storage.HopPoint
		var ttl uint8
		var targetReply uint8
		var lossPct float32
		var lost, sent uint16
		var min, max, mean, median float64
		if err := rows.Scan(
			&p.Time, &p.Source, &ttl, &p.IP, &p.Unreach, &targetReply,
			&min, &max, &mean, &median,
			&lossPct, &lost, &sent,
		); err != nil {
			return nil, err
		}
		p.TargetReply = targetReply != 0
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
	return hopRowsWithinCap(hopRowCap, out, rows.Err())
}

func (r *Reader) QueryHopsTimeline(ctx context.Context, ref config.TargetRef, from, to time.Time, f storage.QueryFilter) (storage.HopsResult, error) {
	hops, err := r.queryHopsGrid(ctx, ref, from, to, f.Source, f.Step)
	if err != nil {
		return storage.HopsResult{}, err
	}
	// No Cycles: a grid slot spans whatever cycles fell in it, so there is no
	// single cycle whose counters it could carry.
	return storage.HopsResult{Hops: hops}, nil
}

// queryHopsGrid reads the heatmap's grid — one row per (bucket, ttl) for one
// probe origin, each slot's responders folded into its worst-loss cycle's row
// with address, annotation and timestamp read from one argMax tuple so they
// describe the same row (see CLAUDE.md's storage bullet for the folding and
// max_loss_pct rationale). No alias may shadow a column it aggregates:
// `loss_pct AS loss_pct` makes max(loss_pct) an aggregate of an aggregate,
// and aliasing worst_addr/worst_unreach to their own column names makes
// `worst`, which reads those columns, cyclic.
func (r *Reader) queryHopsGrid(ctx context.Context, ref config.TargetRef, from, to time.Time, source string, step time.Duration) ([]storage.HopPoint, error) {
	// Refused rather than served raw: a slot per cycle puts the producer's
	// cycle rate back in the row count, which nothing bounds.
	if step <= 0 {
		return nil, fmt.Errorf("query hops grid: step %s is not a grid", step)
	}
	slot := fmt.Sprintf("toStartOfInterval(timestamp, INTERVAL %d SECOND)", int(step.Seconds()))
	q := `
SELECT ` + slot + ` AS bucket_ts,
       source                                              AS src,
       ttl,
       (argMax(tuple(hop_addr, unreach, timestamp),
               loss_pct) AS worst).1                       AS worst_addr,
       worst.2                                             AS worst_unreach,
       sum(sent)                                           AS total_sent,
       sum(lost)                                           AS total_lost,
       if(sum(sent) = 0, 0, 100.0 * sum(lost) / sum(sent)) AS avg_loss_pct,
       max(loss_pct)                                       AS max_loss_pct,
       worst.3                                             AS worst_ts
FROM probe_hop
WHERE target_id = ?
  AND target_group = ?
  AND source = ?
  AND timestamp >= ` + dtMilli + ` AND timestamp < ` + dtMilli + `
GROUP BY bucket_ts, source, ttl
ORDER BY bucket_ts, ttl` + hopRowLimit(hopTimelineRowCap)
	rows, err := r.conn.Query(ctx, q, ref.Target.Name, ref.Group, source, from.UnixMilli(), to.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("query hops grid: %w", err)
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
		if err := rows.Scan(&p.Time, &p.Source, &ttl, &p.IP, &p.Unreach, &sent, &lost, &lossPct, &maxLossPct, &worstTs); err != nil {
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
	return hopRowsWithinCap(hopTimelineRowCap, out, rows.Err())
}
