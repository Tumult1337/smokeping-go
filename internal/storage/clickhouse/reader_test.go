package clickhouse

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/storage"
)

// rttValueRows yields one probe_rtt row per value, filling only the rtt_ms
// destination.
type rttValueRows struct {
	driver.Rows
	values []float64
}

func (r *rttValueRows) Next() bool { return len(r.values) > 0 }
func (r *rttValueRows) Scan(dest ...any) error {
	if p, ok := dest[1].(*float64); ok {
		*p = r.values[0]
	}
	r.values = r.values[1:]
	return nil
}
func (*rttValueRows) Err() error   { return nil }
func (*rttValueRows) Close() error { return nil }

type rttValueConn struct {
	driver.Conn
	values []float64
}

func (c *rttValueConn) Query(context.Context, string, ...any) (driver.Rows, error) {
	return &rttValueRows{values: c.values}, nil
}

// Rows written as NaN (a 0µs RTT through the pre-clamp rttMS) survive until
// probe_rtt's TTL, and one of them made writeJSON fail after the 200 header —
// every /rtts overlapping it returned an empty body. The reader must not hand
// a non-finite value to the encoder.
func TestQueryRTTsDropsNonFiniteRows(t *testing.T) {
	conn := &rttValueConn{values: []float64{math.NaN(), 1.5, math.Inf(1), 0}}
	got, err := (&Reader{conn: conn}).QueryRTTs(context.Background(),
		config.TargetRef{Group: "core", Target: config.Target{Name: "gw"}},
		time.Now().Add(-time.Hour), time.Now(), storage.QueryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].RTT != 1.5 || got[1].RTT != 0 {
		t.Fatalf("QueryRTTs = %+v, want the finite rows 1.5 and 0 only", got)
	}
}

type hopGridRows struct {
	driver.Rows
	left int
}

func (r *hopGridRows) Next() bool {
	if r.left == 0 {
		return false
	}
	r.left--
	return true
}

func (r *hopGridRows) Scan(dest ...any) error {
	if _, ok := dest[7].(*int64); !ok {
		return fmt.Errorf("total_replies destination is %T, want *int64", dest[7])
	}
	return nil
}

func (*hopGridRows) Err() error   { return nil }
func (*hopGridRows) Close() error { return nil }

type hopGridConn struct{ driver.Conn }

func (*hopGridConn) Query(context.Context, string, ...any) (driver.Rows, error) {
	return &hopGridRows{left: 1}, nil
}

func TestQueryHopsGridScansSignedReplyCount(t *testing.T) {
	ref := config.TargetRef{Group: "core", Target: config.Target{Name: "gw"}}
	from := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	_, err := (&Reader{conn: &hopGridConn{}}).queryHopsGrid(
		context.Background(), ref, from, from.Add(time.Hour), "master", time.Hour)
	if err != nil {
		t.Fatalf("queryHopsGrid: %v", err)
	}
}
