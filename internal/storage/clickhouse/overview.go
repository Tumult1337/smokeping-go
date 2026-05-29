package clickhouse

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/storage"
)

// overviewSparkBuckets is the fixed number of positional slots in every
// returned sparkline. Picked to match the UI's small inline chart (60px wide,
// ~2.5px per slot) while staying coarse enough that a 1h window with 60
// cycles gives multiple cycles per bucket.
const overviewSparkBuckets = 24

// QueryOverview returns one row per (group, name, source) tuple over the
// requested window, scoped to the given targets. Each row carries
// whole-window aggregates plus a positional 24-slot sparkline of the
// per-bucket median. Empty target list → empty result without touching CH.
func (r *Reader) QueryOverview(ctx context.Context, from, to time.Time, targets []config.TargetRef) ([]storage.OverviewSourceRow, error) {
	if len(targets) == 0 {
		return nil, nil
	}

	bucketSec := max(int64(to.Sub(from).Seconds())/int64(overviewSparkBuckets), 1)

	// Tuple-IN with placeholder pairs. target_group/target_id come from
	// config (operator-supplied) and aren't user input — placeholders are
	// still preferable to literal interpolation: keeps escaping in the
	// driver's hands and makes the query plan cacheable.
	pairs := make([]string, len(targets))
	for i := range targets {
		pairs[i] = "(?, ?)"
	}
	inClause := strings.Join(pairs, ", ")

	// groupArrayInsertAt(default, size) takes the default value and array
	// size as parameters of the aggregate combinator, not in the SELECT
	// list. Size is a compile-time constant so it's safe to interpolate.
	q := fmt.Sprintf(`
SELECT
  target_group, target_id, source,
  toFloat64(avg(b_loss_avg))                                 AS loss_avg,
  toFloat64(max(b_loss_max))                                 AS loss_max,
  quantilesExactWeighted(0.5)(b_median, b_sent_total)[1]     AS rtt_median,
  quantilesExactWeighted(0.95)(b_p95,    b_sent_total)[1]    AS rtt_p95,
  max(b_max)                                                 AS rtt_max,
  max(b_last_seen)                                           AS last_seen,
  -- Two parallel arrays so the handler can assemble a fixed-length sparkline
  -- without losing "this slot had no data" — Array(Nullable(Float64)) round-
  -- trips inconsistently through the driver, but two plain arrays work.
  groupArray(toUInt32(bucket_idx))                           AS spark_idx,
  groupArray(b_median)                                       AS spark_val
FROM (
  SELECT
    target_group, target_id, source,
    intDiv(toUInt32(timestamp) - toUInt32(toDateTime(?)), ?)  AS bucket_idx,
    avg(loss_pct)                                             AS b_loss_avg,
    max(loss_pct)                                             AS b_loss_max,
    quantilesExactWeighted(0.5)(rtt_median_ms, sent)[1]       AS b_median,
    quantilesExactWeighted(0.95)(p95_ms, sent)[1]             AS b_p95,
    max(rtt_max_ms)                                           AS b_max,
    sum(sent)                                                 AS b_sent_total,
    max(timestamp)                                            AS b_last_seen
  FROM probe_cycle
  WHERE timestamp >= ? AND timestamp < ?
    AND (target_group, target_id) IN (%s)
  GROUP BY target_group, target_id, source, bucket_idx
)
GROUP BY target_group, target_id, source`, inClause)

	// Arg order matches the placeholders top-to-bottom in the query:
	// inner intDiv(from), inner bucketSec, outer WHERE from, outer WHERE to,
	// then each (group, name) pair.
	args := make([]any, 0, 4+len(targets)*2)
	args = append(args, from, bucketSec, from, to)
	for _, t := range targets {
		args = append(args, t.Group, t.Target.Name)
	}

	rows, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query overview: %w", err)
	}
	defer rows.Close() //nolint:errcheck // rows.Err() returned below

	var out []storage.OverviewSourceRow
	for rows.Next() {
		var (
			row     storage.OverviewSourceRow
			sparkIx []uint32
			sparkVl []float64
		)
		if err := rows.Scan(
			&row.Group, &row.Name, &row.Source,
			&row.LossAvg, &row.LossMax,
			&row.RTTMedian, &row.RTTP95, &row.RTTMax,
			&row.LastSeen,
			&sparkIx, &sparkVl,
		); err != nil {
			return nil, fmt.Errorf("scan overview row: %w", err)
		}
		// Materialize the fixed-length sparkline. Slots without data stay
		// nil so the UI renders gaps instead of zero-drops.
		spark := make([]*float64, overviewSparkBuckets)
		for i, idx := range sparkIx {
			if idx >= overviewSparkBuckets {
				continue
			}
			v := sparkVl[i]
			spark[idx] = &v
		}
		row.Sparkline = spark
		out = append(out, row)
	}
	return out, rows.Err()
}
