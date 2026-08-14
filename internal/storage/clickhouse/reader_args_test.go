package clickhouse

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/storage"
)

// recordConn captures the query and args of the last Query call. Every other
// driver.Conn method panics: a reader path reaching one would be doing
// something this test cannot reason about, and silently returning zero values
// would let it pass.
type recordConn struct {
	driver.Conn
	query string
	args  []any
}

func (c *recordConn) Query(_ context.Context, query string, args ...any) (driver.Rows, error) {
	c.query, c.args = query, args
	return emptyRows{}, nil
}

type emptyRows struct{ driver.Rows }

func (emptyRows) Next() bool   { return false }
func (emptyRows) Err() error   { return nil }
func (emptyRows) Close() error { return nil }

// Query text and its argument slice are built in separate places in every
// reader method, so nothing but a count catches a placeholder added without
// its argument — and the failure is a runtime driver error on a path no unit
// test reaches without a live server. This walks every read with the filter
// permutations that add and remove clauses.
func TestReaderQueryPlaceholdersMatchArgs(t *testing.T) {
	ref := config.TargetRef{Group: "core", Target: config.Target{Name: "gw"}}
	from := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)

	filters := map[string]storage.QueryFilter{
		"no filters":        {},
		"source":            {Source: "edge-1"},
		"step":              {Step: time.Hour},
		"source+step":       {Source: "edge-1", Step: time.Hour},
		"source+fresh":      {Source: "edge-1", LatestSince: from},
		"fresh only":        {LatestSince: from},
		"source+step+fresh": {Source: "edge-1", Step: 15 * time.Minute, LatestSince: from},
	}

	calls := map[string]func(*Reader, storage.QueryFilter) error{
		"QueryCycles": func(r *Reader, f storage.QueryFilter) error {
			_, err := r.QueryCycles(context.Background(), ref, from, to, f)
			return err
		},
		"QueryRTTs": func(r *Reader, f storage.QueryFilter) error {
			_, err := r.QueryRTTs(context.Background(), ref, from, to, f)
			return err
		},
		"QueryHTTPSamples": func(r *Reader, f storage.QueryFilter) error {
			_, err := r.QueryHTTPSamples(context.Background(), ref, from, to, f)
			return err
		},
		"QueryLatestHops": func(r *Reader, f storage.QueryFilter) error {
			_, err := r.QueryLatestHops(context.Background(), ref, f)
			return err
		},
		"QueryHopsAt": func(r *Reader, f storage.QueryFilter) error {
			_, err := r.QueryHopsAt(context.Background(), ref, from, 30*time.Minute, f)
			return err
		},
		"QueryHopsTimeline": func(r *Reader, f storage.QueryFilter) error {
			_, err := r.QueryHopsTimeline(context.Background(), ref, from, to, f)
			return err
		},
	}

	for name, call := range calls {
		for fname, f := range filters {
			t.Run(name+"/"+fname, func(t *testing.T) {
				conn := &recordConn{}
				if err := call(&Reader{conn: conn}, f); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				want := strings.Count(conn.query, "?")
				if got := len(conn.args); got != want {
					t.Fatalf("%s: %d placeholders, %d args\nquery:%s\nargs:%v", name, want, got, conn.query, conn.args)
				}
				// The group predicate is what separates two targets sharing a
				// name; a query that lost it would still have matching counts.
				if !strings.Contains(conn.query, "target_group = ?") {
					t.Fatalf("%s: query does not scope by target_group:%s", name, conn.query)
				}
			})
		}
	}
}
