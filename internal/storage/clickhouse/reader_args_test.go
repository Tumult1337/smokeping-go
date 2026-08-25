package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"strconv"
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

// orderByLead returns the first column of the query's last ORDER BY clause.
func orderByLead(query string) string {
	i := strings.LastIndex(query, "ORDER BY ")
	if i < 0 {
		return ""
	}
	rest := strings.TrimSpace(query[i+len("ORDER BY "):])
	for _, cut := range []string{",", " ", "\n", "`"} {
		if j := strings.Index(rest, cut); j >= 0 {
			rest = rest[:j]
		}
	}
	return rest
}

// The charts consume row order straight from the server: ui/src/chartUtils.ts
// normalizes each point's timestamp once and the series builders preserve the
// response order rather than re-sorting. A read that lost its ORDER BY, or
// gained a UNION that dissolves it, would draw a scribble instead of a line,
// and no unit test reaches that path without a live server.
func TestReaderQueriesOrderForTheirConsumer(t *testing.T) {
	ref := config.TargetRef{Group: "core", Target: config.Target{Name: "gw"}}
	from := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)

	// Time-series reads must lead on time; the hop snapshot reads pin one
	// instant per source, so their consumer relies on (source, ttl) instead.
	cases := []struct {
		name  string
		call  func(*Reader, storage.QueryFilter) error
		leads []string
	}{
		{"QueryCycles", func(r *Reader, f storage.QueryFilter) error {
			_, err := r.QueryCycles(context.Background(), ref, from, to, f)
			return err
		}, []string{"timestamp", "bucket_ts"}},
		{"QueryRTTs", func(r *Reader, f storage.QueryFilter) error {
			_, err := r.QueryRTTs(context.Background(), ref, from, to, f)
			return err
		}, []string{"timestamp"}},
		{"QueryHTTPSamples", func(r *Reader, f storage.QueryFilter) error {
			_, err := r.QueryHTTPSamples(context.Background(), ref, from, to, f)
			return err
		}, []string{"timestamp"}},
		{"QueryHopsTimeline", func(r *Reader, f storage.QueryFilter) error {
			_, err := r.QueryHopsTimeline(context.Background(), ref, from, to, f)
			return err
		}, []string{"timestamp", "bucket_ts"}},
		{"QueryLatestHops", func(r *Reader, f storage.QueryFilter) error {
			_, err := r.QueryLatestHops(context.Background(), ref, f)
			return err
		}, []string{"source"}},
		{"QueryHopsAt", func(r *Reader, f storage.QueryFilter) error {
			_, err := r.QueryHopsAt(context.Background(), ref, from, 30*time.Minute, f)
			return err
		}, []string{"source"}},
	}

	for _, tc := range cases {
		for fname, f := range map[string]storage.QueryFilter{
			"raw":      {},
			"bucketed": {Step: time.Hour},
		} {
			t.Run(tc.name+"/"+fname, func(t *testing.T) {
				conn := &recordConn{}
				if err := tc.call(&Reader{conn: conn}, f); err != nil {
					t.Fatalf("%s: %v", tc.name, err)
				}
				lead := orderByLead(conn.query)
				if lead == "" {
					t.Fatalf("%s: query has no ORDER BY:%s", tc.name, conn.query)
				}
				for _, want := range tc.leads {
					if lead == want {
						return
					}
				}
				t.Fatalf("%s: ORDER BY leads on %q, want one of %v:%s", tc.name, lead, tc.leads, conn.query)
			})
		}
	}
}

// Every hop read buffers its whole result set into a []storage.HopPoint on an
// unauthenticated endpoint, and hop_addr is a slave-supplied string that
// widens queryHopsBucketed's GROUP BY without bound. Each path carries a
// LIMIT one past the cap, so the row set a single GET can force is finite and
// reaching the cap is still distinguishable from ending on it.
func TestHopQueriesCarryRowLimit(t *testing.T) {
	ref := config.TargetRef{Group: "core", Target: config.Target{Name: "gw"}}
	from := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)

	calls := map[string]func(*Reader) error{
		"QueryLatestHops": func(r *Reader) error {
			_, err := r.QueryLatestHops(context.Background(), ref, storage.QueryFilter{})
			return err
		},
		"QueryHopsAt": func(r *Reader) error {
			_, err := r.QueryHopsAt(context.Background(), ref, from, 30*time.Minute, storage.QueryFilter{})
			return err
		},
		"QueryHopsTimeline/raw": func(r *Reader) error {
			_, err := r.QueryHopsTimeline(context.Background(), ref, from, to, storage.QueryFilter{})
			return err
		},
		"QueryHopsTimeline/bucketed": func(r *Reader) error {
			_, err := r.QueryHopsTimeline(context.Background(), ref, from, to, storage.QueryFilter{Step: 15 * time.Minute})
			return err
		},
	}
	want := "LIMIT " + strconv.Itoa(maxHopRows+1)
	for name, call := range calls {
		conn := &recordConn{}
		if err := call(&Reader{conn: conn}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(conn.query, want) {
			t.Errorf("%s: query has no %q:\n%s", name, want, conn.query)
		}
	}
}

// A future-dated row from a hostile slave pinned itself as that source's
// "latest" until the lie expired. The ingest bound stops new ones; the reader
// ceiling is what keeps rows already in the table off the endpoint.
func TestQueryLatestHopsBoundsFutureRows(t *testing.T) {
	conn := &recordConn{}
	if _, err := (&Reader{conn: conn}).QueryLatestHops(context.Background(),
		config.TargetRef{Group: "core", Target: config.Target{Name: "gw"}}, storage.QueryFilter{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(conn.query, "timestamp <= now() + INTERVAL") {
		t.Errorf("no future ceiling in the latest-hops CTE:\n%s", conn.query)
	}
}

// The cap was first sized against two responder addresses per
// (bucket, source, ttl). A 7d/15m timeline read off the deployed six-source
// fleet holds four, so a 30-TTL path fits in 485k rows and the old 300k sat
// under a shape the probe produces on its own.
func TestMaxHopRowsClearsTheWidestLegitimateTimeline(t *testing.T) {
	const (
		buckets    = 7*24*4 + 2 // 7d, the endpoint's window cap, at the 15m tier plus a partial bucket each end
		ttls       = 30         // the TTL walk's own ceiling
		sources    = 6          // the deployed reference fleet
		responders = 4          // most distinct addresses one (bucket, source, ttl) held over 7d
	)
	if want := buckets * ttls * sources * responders; maxHopRows < want {
		t.Fatalf("maxHopRows = %d, under the %d rows a legitimate 7d timeline holds", maxHopRows, want)
	}
}

// countRows yields n rows of zero values so a hop read can be driven past its
// cap without materialising the real one.
type countRows struct {
	driver.Rows
	left int
}

func (r *countRows) Next() bool {
	if r.left == 0 {
		return false
	}
	r.left--
	return true
}

func (*countRows) Scan(...any) error { return nil }
func (*countRows) Err() error        { return nil }
func (*countRows) Close() error      { return nil }

type countConn struct {
	driver.Conn
	rows  int
	query string
}

func (c *countConn) Query(_ context.Context, query string, _ ...any) (driver.Rows, error) {
	c.query = query
	return &countRows{left: c.rows}, nil
}

// Hop reads order oldest-first, so serving the cap's prefix hands back a path
// history with its newest rows missing — which renders as a probe that
// stopped. Every hop read must refuse instead, and must ask for one row past
// the cap so reaching it is distinguishable from ending on it.
func TestHopReadsRefuseATruncatedResult(t *testing.T) {
	const cap = 4
	orig := hopRowCap
	hopRowCap = cap
	t.Cleanup(func() { hopRowCap = orig })

	ref := config.TargetRef{Group: "core", Target: config.Target{Name: "gw"}}
	from := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)

	calls := map[string]func(*Reader) ([]storage.HopPoint, error){
		"QueryLatestHops": func(r *Reader) ([]storage.HopPoint, error) {
			return r.QueryLatestHops(context.Background(), ref, storage.QueryFilter{})
		},
		"QueryHopsAt": func(r *Reader) ([]storage.HopPoint, error) {
			return r.QueryHopsAt(context.Background(), ref, from, 30*time.Minute, storage.QueryFilter{})
		},
		"QueryHopsTimeline raw": func(r *Reader) ([]storage.HopPoint, error) {
			return r.QueryHopsTimeline(context.Background(), ref, from, to, storage.QueryFilter{})
		},
		"QueryHopsTimeline bucketed": func(r *Reader) ([]storage.HopPoint, error) {
			return r.QueryHopsTimeline(context.Background(), ref, from, to, storage.QueryFilter{Step: 15 * time.Minute})
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			atCap := &countConn{rows: cap}
			got, err := call(&Reader{conn: atCap})
			if err != nil {
				t.Fatalf("a result exactly at the cap was refused: %v", err)
			}
			if len(got) != cap {
				t.Fatalf("got %d rows at the cap, want %d", len(got), cap)
			}
			if want := fmt.Sprintf("LIMIT %d", cap+1); !strings.Contains(atCap.query, want) {
				t.Fatalf("query does not ask for a row past the cap (%s):%s", want, atCap.query)
			}

			over := &countConn{rows: cap + 1}
			got, err = call(&Reader{conn: over})
			if !errors.Is(err, storage.ErrHopsTruncated) {
				t.Fatalf("err = %v, want ErrHopsTruncated", err)
			}
			if got != nil {
				t.Fatalf("a refused read still returned %d rows", len(got))
			}
		})
	}
}
