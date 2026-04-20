// Package storage defines the data types and read surface that the API
// and scheduler consume, independent of which concrete backend persists
// results. Backends live in subpackages (influxv2, influxv3, prom) and
// implement storage.Reader + scheduler.Sink; the Backend interface and
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
// so prefer new filter parameters on existing methods when possible.
type Reader interface {
	QueryCycles(ctx context.Context, ref config.TargetRef, from, to time.Time, res Resolution, source string) ([]CyclePoint, error)
	QueryRTTs(ctx context.Context, ref config.TargetRef, from, to time.Time, source string) ([]RTTPoint, error)
	QueryHTTPSamples(ctx context.Context, ref config.TargetRef, from, to time.Time, source string) ([]HTTPPoint, error)
	QueryLatestHops(ctx context.Context, ref config.TargetRef, source string) ([]HopPoint, error)
	QueryHopsAt(ctx context.Context, ref config.TargetRef, at time.Time, window time.Duration, source string) ([]HopPoint, error)
	QueryHopsTimeline(ctx context.Context, ref config.TargetRef, from, to time.Time, source string) ([]HopPoint, error)
}

// Resolution picks which retention tier to query. Backends that don't
// rollup can treat every value as "raw".
type Resolution string

const (
	ResolutionRaw Resolution = "raw"
	Resolution1h  Resolution = "1h"
	Resolution1d  Resolution = "1d"
)

// PickResolution chooses a tier by requested span. 7d fits the raw bucket
// retention so any span within that stays at raw — avoids empty results
// from rollup tasks that haven't run yet on a fresh install.
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
// received (DNS, refused, TLS, timeout) and Err explains why.
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

// ErrBackendNotImplemented is returned by Open when the configured backend
// name is recognised but no working implementation is compiled in (stubs
// for influxv3/prometheus return this until they're built out).
var ErrBackendNotImplemented = errors.New("storage: backend not yet implemented")

// ErrDisabled is returned by Open when the config selects a backend but
// leaves its credentials empty — the caller treats it as "run without
// persistent storage" rather than a fatal error.
var ErrDisabled = errors.New("storage: backend disabled (no credentials)")
