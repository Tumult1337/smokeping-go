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
	QueryLatestHops(ctx context.Context, ref config.TargetRef, f QueryFilter) ([]HopPoint, error)
	QueryHopsAt(ctx context.Context, ref config.TargetRef, at time.Time, window time.Duration, f QueryFilter) ([]HopPoint, error)
	QueryHopsTimeline(ctx context.Context, ref config.TargetRef, from, to time.Time, f QueryFilter) ([]HopPoint, error)
}

// QueryFilter narrows a query along orthogonal dimensions. Zero value =
// no filtering, raw step (no bucketing). Add new fields here instead of
// lengthening Reader method signatures.
type QueryFilter struct {
	// Source, when non-empty, limits rows to that exact source tag value.
	Source string
	// Step, when > 0, asks the backend to bucket results by this width
	// using toStartOfInterval (or equivalent). Zero = return raw rows.
	// The server picks Step from window width before invoking the reader.
	Step time.Duration
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

// HopPoint is the most recent stats for one hop on an MTR path. Source
// identifies the probe origin (master / slave name), matching CyclePoint;
// empty for pre-cluster rows. The heatmap and HopsTable rely on it to
// disambiguate per-source paths when more than one origin probes the
// target — without it a click on a slave's lossy bucket would silently
// return the master's clean cycle for the same timestamp.
type HopPoint struct {
	Time      time.Time
	Source    string
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

// ErrDisabled is returned by Open when the config selects a backend but
// leaves its credentials empty — the caller treats it as "run without
// persistent storage" rather than a fatal error.
var ErrDisabled = errors.New("storage: backend disabled (no credentials)")
