// Package storage defines the data types and read surface that the API
// and scheduler consume, independent of which concrete backend persists
// results. The active backend lives in the clickhouse subpackage and
// implements storage.Reader + scheduler.Sink; the Backend interface and
// factory live at the composition root so this package stays a pure leaf
// and can be imported by any backend without a cycle.
package storage

import (
	"context"
	"errors"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
)

// Reader is the query surface every backend exposes to the API. Kept
// narrow on purpose: adding a method forces every backend to implement it,
// so new filter knobs belong on QueryFilter rather than as new parameters.
type Reader interface {
	QueryCycles(ctx context.Context, ref config.TargetRef, from, to time.Time, f QueryFilter) ([]CyclePoint, error)
	QueryRTTs(ctx context.Context, ref config.TargetRef, from, to time.Time, f QueryFilter) ([]RTTPoint, error)
	QueryHTTPSamples(ctx context.Context, ref config.TargetRef, from, to time.Time, f QueryFilter) ([]HTTPPoint, error)
	QueryLatestHops(ctx context.Context, ref config.TargetRef, f QueryFilter) (HopsResult, error)
	QueryHopsAt(ctx context.Context, ref config.TargetRef, at time.Time, window time.Duration, f QueryFilter) (HopsResult, error)
	QueryHopsTimeline(ctx context.Context, ref config.TargetRef, from, to time.Time, f QueryFilter) (HopsResult, error)
	// QueryOverview returns one row per (group, name, source) for the
	// configured targets passed in. Used by the fleet overview page; the
	// handler collapses to worst-source per target. Sparkline length is fixed
	// at 24 (one slot per bucket across the window); slots without data are
	// nil. Targets with no rows at all are not returned — the handler
	// synthesizes silent rows by left-joining the input target list against
	// the result.
	QueryOverview(ctx context.Context, from, to time.Time, targets []config.TargetRef) ([]OverviewSourceRow, error)
}

// MinQueryTime and MaxQueryTime are the DateTime64(3) domain every probe
// table's timestamp column carries, and so the widest instant a query can
// name. ClickHouse does not refuse an epoch outside it: fromUnixTimestamp64Milli
// wraps, and Go's UnixMilli wraps first and in the other direction, so an
// unbounded parse hands the server a centre in the opposite epoch direction
// from the one requested. Derived from the column type, not from retention,
// which is per-install and re-appliable at any restart.
var (
	MinQueryTime = time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)
	MaxQueryTime = time.Date(2299, 12, 31, 23, 59, 59, int(999*time.Millisecond), time.UTC)
)

// ValidQueryTime reports whether t names an instant probe storage can address.
// Callers must apply it before any UnixMilli/UnixNano conversion.
func ValidQueryTime(t time.Time) bool {
	return !t.Before(MinQueryTime) && !t.After(MaxQueryTime)
}

// PickCycleStep returns the toStartOfInterval width for cycle queries.
// Tiers: ≤2h raw, ≤24h 2m, ≤7d 15m, ≤30d 1h, ≤180d 6h, >180d 1d. The
// ladder targets ~500–1000 buckets per window so point density stays
// roughly constant as the user zooms out — at a 60s cycle interval no
// transition drops more than ~7× across a boundary, which keeps the
// min/median/max band readable without per-cycle wiggle. Bucketing is
// information-preserving via the weighted-percentile aggregation in the
// CH reader; client-side smoothing would hide outliers in the p95/max
// band, which is the chart's whole point. Lives next to PickHopStep so
// the API layer can pick a step from window width without depending on
// a specific storage backend.
func PickCycleStep(span time.Duration) time.Duration {
	switch {
	case span <= 2*time.Hour:
		return 0
	case span <= 24*time.Hour:
		return 2 * time.Minute
	case span <= 7*24*time.Hour:
		return 15 * time.Minute
	case span <= 30*24*time.Hour:
		return time.Hour
	case span <= 180*24*time.Hour:
		return 6 * time.Hour
	default:
		return 24 * time.Hour
	}
}

// MaxHopTimelineWindow is the widest window /hops/timeline serves, and the
// first term of every row bound derived from its grid.
const MaxHopTimelineWindow = 7 * 24 * time.Hour

// MaxHopGridSlots is the widest grid any tier produces: the whole window at
// the coarsest step the ladder returns, plus one slot for a `from` that is off
// the grid.
const MaxHopGridSlots = int(MaxHopTimelineWindow/(15*time.Minute)) + 1

// finestHopTierSpan is the widest window the hop ladder's finest tier serves.
const finestHopTierSpan = 2 * time.Hour

// MinHopStep is the finest step the ladder returns: the smallest whole second
// (the unit the query renders) whose grid over the finest tier's whole span
// still fits MaxHopGridSlots, so the finest tier never needs more slots than
// the 7d tier the row cap is derived from.
const MinHopStep = time.Duration((int64(finestHopTierSpan/time.Second)+int64(MaxHopGridSlots)-2)/int64(MaxHopGridSlots-1)) * time.Second

// PickHopStep returns the toStartOfInterval width for hop timeline queries.
// Tiers: ≤2h MinHopStep, ≤24h 5m, >24h 15m, never finer than interval.
//
// It never returns 0: a raw grid holds one slot per cycle, and nothing bounds
// a cycle rate — config requires only a positive interval, and a one-hop MTR
// target at 30ms puts 240,000 rows in the 2h window. Bucketing takes cycle
// rate out of the row count, which is what makes the timeline's cap derivable
// instead of a bet on how fast a producer runs.
//
// The step never goes below interval because a grid finer than the cadence
// filling it leaves empty slots, which the heatmap draws as history that was
// never collected.
//
// Lives in the storage package (not the CH reader) so the API layer can
// pick the step from window width without importing a backend implementation.
func PickHopStep(span, interval time.Duration) time.Duration {
	step := 15 * time.Minute
	switch {
	case span <= finestHopTierSpan:
		step = MinHopStep
	case span <= 24*time.Hour:
		step = 5 * time.Minute
	}
	if interval > step {
		// Whole seconds is the unit the query renders, and rounding up rather
		// than down is what keeps the step from landing back under the cadence.
		step = interval.Truncate(time.Second)
		if step < interval {
			step += time.Second
		}
	}
	return step
}

// QueryFilter narrows a query along orthogonal dimensions. Zero value =
// no filtering, raw step (no bucketing). Add new fields here instead of
// lengthening Reader method signatures.
type QueryFilter struct {
	// Source, when non-empty, limits rows to that exact source tag value.
	// QueryHopsTimeline is the exception: it matches Source exactly whatever
	// its value, empty included, because it serves one probe origin per call —
	// the source count is the one factor in its row bound that nothing limits.
	Source string
	// Step, when > 0, asks the backend to bucket results by this width
	// using toStartOfInterval (or equivalent). Zero = return raw rows, which
	// QueryHopsTimeline refuses because a slot per cycle puts the producer's
	// cycle rate back into its row bound.
	// The server picks Step from window width before invoking the reader.
	Step time.Duration
	// LatestSince, when non-zero, bounds the "current path" view
	// (QueryLatestHops): a source whose newest hop row predates this
	// instant is dropped entirely, so a removed or stopped probe origin
	// stops appearing as a live path once it goes silent. Zero = no floor
	// (every source with retained rows is returned). Only QueryLatestHops
	// honours it; windowed queries already bound by from/to.
	LatestSince time.Time
}

// CyclePoint is one row of aggregate per-cycle data. Source identifies the
// probe origin (master / slave name); empty for pre-cluster rows that
// carry no source tag.
type CyclePoint struct {
	Time      time.Time
	Source    string
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

// HTTPPoint is one HTTP request sample. Status == 0 means no response was
// received (DNS, refused, TLS, timeout) and Err explains why. Source
// identifies the probe origin, matching CyclePoint.Source.
type HTTPPoint struct {
	Time   time.Time
	Source string
	RTT    float64
	Status int64
	Seq    int64
	Err    string
}

// ErrHopsTruncated is returned instead of a short result when a hop read
// reaches its row cap: hop reads order oldest-first, so the prefix is missing
// the newest history and reads as a probe that stopped.
var ErrHopsTruncated = errors.New("storage: hop result exceeds the row cap")

// HopsResult is one hop read: the path rows, and the round counters of the
// cycles those rows came from.
type HopsResult struct {
	Hops []HopPoint
	// Cycles carries one entry per (source, cycle) present in Hops, missing
	// for a source whose cycle sent nothing and so wrote no probe_cycle row,
	// and empty for QueryHopsTimeline, which buckets across many cycles.
	Cycles []CycleCounters
}

// CycleCounters is one cycle's own round accounting for one source. Loss at
// the target is a property of the cycle and cannot be recovered from hop
// rows: a per-round walk marks the target at every TTL it ever answered at,
// so summing those rows counts one round once per marked TTL.
type CycleCounters struct {
	Source    string
	Time      time.Time
	Sent      int64
	LossCount int64
	LossPct   float64
}

// HopPoint is the most recent stats for one hop on an MTR path. Source
// identifies the probe origin (master / slave name), matching CyclePoint;
// empty for pre-cluster rows. The heatmap and HopsTable rely on it to
// disambiguate per-source paths when more than one origin probes the
// target — without it a click on a slave's lossy bucket would silently
// return the master's clean cycle for the same timestamp.
type HopPoint struct {
	Time   time.Time
	Source string
	Index  int64
	IP     string
	// TargetReply marks the row(s) whose responder was the target itself.
	// /hops redaction and MTR's RTT selection key on it because a per-round
	// walk does not guarantee the target row is the deepest. Target loss does
	// not: it comes from Cycles, which no hop row can reconstruct.
	TargetReply bool
	// Unreach is the closed-set label of the ICMP unreachable that ended the
	// trace at this hop; empty for ordinary hops and rows predating the
	// column.
	Unreach string
	Min     float64
	Max     float64
	Mean    float64
	Median  float64
	// LossPct is the bucket-average loss. MaxLossPct is the worst single cycle
	// within the bucket, so the two are equal in a slot holding one cycle.
	// The heatmap colors by
	// MaxLossPct so a brief 100% loss event inside a 5-min bucket survives
	// instead of being averaged down to ~3% and visually disappearing.
	LossPct    float64
	MaxLossPct float64
	LossCount  int64
	Sent       int64
	// WorstTime is the exact timestamp of the worst-loss cycle inside the
	// bucket, read off the same row its address came from. Lets a
	// heatmap-cell click jump to the cycle that justifies the cell's colour
	// instead of the bucket's first (often clean) cycle — the bucket is
	// coloured by MaxLossPct, so the first cycle is frequently not the lossy
	// one and the displayed path looked "one cycle to the left".
	WorstTime time.Time
}

// OverviewSourceRow is one row of fleet-overview aggregates for a single
// (group, name, source) tuple over a user-selected window. The API handler
// collapses per-source rows to one row per target using worst-source-per-
// target semantics (worst LossAvg, tiebreak by worst RTTMax) before serving.
// Sparkline is 24 positional buckets across the window; nil entries mean
// "no cycles landed in this bucket".
type OverviewSourceRow struct {
	Group     string
	Name      string
	Source    string
	LossAvg   float64
	LossMax   float64
	RTTMedian float64
	RTTP95    float64
	RTTMax    float64
	LastSeen  time.Time
	Sparkline []*float64
}

// ErrDisabled is returned by Open when the config selects a backend but
// leaves its credentials empty — the caller treats it as "run without
// persistent storage" rather than a fatal error.
var ErrDisabled = errors.New("storage: backend disabled (no credentials)")
