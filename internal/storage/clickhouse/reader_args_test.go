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
			_, err := r.QueryHopsTimeline(context.Background(), ref, from, to, withGrid(f))
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
			_, err := r.QueryHopsTimeline(context.Background(), ref, from, to, withGrid(f))
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
// unauthenticated endpoint. Each path carries a LIMIT one past its own cap —
// the pinned reads' and the timeline's are derived from different bounds — so
// the row set a single GET can force is finite and reaching the cap is still
// distinguishable from ending on it.
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
		"QueryHopsTimeline/finest": func(r *Reader) error {
			_, err := r.QueryHopsTimeline(context.Background(), ref, from, to, storage.QueryFilter{Step: storage.MinHopStep})
			return err
		},
		"QueryHopsTimeline/bucketed": func(r *Reader) error {
			_, err := r.QueryHopsTimeline(context.Background(), ref, from, to, storage.QueryFilter{Step: 15 * time.Minute})
			return err
		},
	}
	caps := map[string]int{
		"QueryLatestHops":            maxHopRows,
		"QueryHopsAt":                maxHopRows,
		"QueryHopsTimeline/finest":   maxHopTimelineRows,
		"QueryHopsTimeline/bucketed": maxHopTimelineRows,
	}
	for name, call := range calls {
		conn := &recordConn{}
		if err := call(&Reader{conn: conn}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := "LIMIT " + strconv.Itoa(caps[name]+1)
		if !strings.Contains(conn.query, want) {
			t.Errorf("%s: query has no %q:\n%s", name, want, conn.query)
		}
	}
}

// withGrid gives a filter the finest step the ladder returns, so a sweep that
// varies every other field still reaches the timeline: it has no raw tier.
func withGrid(f storage.QueryFilter) storage.QueryFilter {
	if f.Step == 0 {
		f.Step = storage.MinHopStep
	}
	return f
}

// The timeline's ceiling is a product of bounds, not a number picked against a
// typical read: one origin per request, one row per (grid slot, ttl), the grid
// no wider than the window cap divided by the step ladder's floor, and ttl a
// UInt8 column ingest refuses to exceed. Every schedule config.Validate
// accepts has to sit under it by construction — a tier exempted from that,
// as the raw one was, is a cap sitting below what a legitimate producer emits.
func TestHopTimelineGridFitsItsRowCap(t *testing.T) {
	if maxHopTimelineRows != maxHopTimelineBuckets*maxHopTTLs {
		t.Fatalf("maxHopTimelineRows = %d, want the grid product %d",
			maxHopTimelineRows, maxHopTimelineBuckets*maxHopTTLs)
	}
	intervals := []time.Duration{time.Nanosecond, 30 * time.Millisecond, time.Second, 20 * time.Second, 5 * time.Minute, config.MaxProbeInterval}
	for span := time.Minute; span <= storage.MaxHopTimelineWindow; span += 11 * time.Minute {
		for _, interval := range intervals {
			step := storage.PickHopStep(span, interval)
			if step <= 0 {
				t.Fatalf("span %s at interval %s picked a raw grid, whose slots are the producer's cycle count", span, interval)
			}
			if slots := int(span/step) + 1; slots > maxHopTimelineBuckets {
				t.Fatalf("span %s at interval %s (step %s) needs %d slots, over the %d the cap is derived from",
					span, interval, step, slots, maxHopTimelineBuckets)
			}
		}
	}
}

// The reader is the last place that can tell a grid from a raw scan, so it
// refuses rather than falling back to the shape whose row count is unbounded.
func TestQueryHopsTimelineRefusesARawGrid(t *testing.T) {
	conn := &recordConn{}
	_, err := (&Reader{conn: conn}).QueryHopsTimeline(context.Background(),
		config.TargetRef{Group: "core", Target: config.Target{Name: "gw"}},
		time.Now().Add(-time.Hour), time.Now(), storage.QueryFilter{Source: "master"})
	if err == nil {
		t.Fatal("a zero step was served as a raw scan")
	}
	if conn.query != "" {
		t.Fatalf("a raw grid reached ClickHouse:\n%s", conn.query)
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

// A pinned hop read returns one cycle per source, and cluster ingest refuses a
// cycle carrying more than 600 hop rows, so the ceiling has to clear that
// product across every source name the master's registry admits at once.
func TestMaxHopRowsClearsEverySourcesPinnedCycle(t *testing.T) {
	const (
		rowsPerCycle      = 600 // cluster.MaxHopsPerCycle, twice the producer's own 300
		registeredSources = 512 // master's maxRegisteredSlaves
	)
	if want := rowsPerCycle * registeredSources; maxHopRows < want {
		t.Fatalf("maxHopRows = %d, under the %d rows a full fleet's pinned read holds", maxHopRows, want)
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
	rows    int
	queries []string
}

func (c *countConn) Query(_ context.Context, query string, _ ...any) (driver.Rows, error) {
	c.queries = append(c.queries, query)
	return &countRows{left: c.rows}, nil
}

// Hop reads order oldest-first, so serving the cap's prefix hands back a path
// history with its newest rows missing — which renders as a probe that
// stopped. Every hop read must refuse instead, and must ask for one row past
// the cap so reaching it is distinguishable from ending on it.
func TestHopReadsRefuseATruncatedResult(t *testing.T) {
	const cap = 4
	origPinned, origTimeline := hopRowCap, hopTimelineRowCap
	hopRowCap, hopTimelineRowCap = cap, cap
	t.Cleanup(func() { hopRowCap, hopTimelineRowCap = origPinned, origTimeline })

	ref := config.TargetRef{Group: "core", Target: config.Target{Name: "gw"}}
	from := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)

	calls := map[string]func(*Reader) (storage.HopsResult, error){
		"QueryLatestHops": func(r *Reader) (storage.HopsResult, error) {
			return r.QueryLatestHops(context.Background(), ref, storage.QueryFilter{})
		},
		"QueryHopsAt": func(r *Reader) (storage.HopsResult, error) {
			return r.QueryHopsAt(context.Background(), ref, from, 30*time.Minute, storage.QueryFilter{})
		},
		"QueryHopsTimeline finest": func(r *Reader) (storage.HopsResult, error) {
			return r.QueryHopsTimeline(context.Background(), ref, from, to, storage.QueryFilter{Step: storage.MinHopStep})
		},
		"QueryHopsTimeline bucketed": func(r *Reader) (storage.HopsResult, error) {
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
			if len(got.Hops) != cap {
				t.Fatalf("got %d rows at the cap, want %d", len(got.Hops), cap)
			}
			if want := fmt.Sprintf("LIMIT %d", cap+1); !strings.Contains(atCap.queries[0], want) {
				t.Fatalf("query does not ask for a row past the cap (%s):%s", want, atCap.queries[0])
			}

			over := &countConn{rows: cap + 1}
			got, err = call(&Reader{conn: over})
			if !errors.Is(err, storage.ErrHopsTruncated) {
				t.Fatalf("err = %v, want ErrHopsTruncated", err)
			}
			if got.Hops != nil {
				t.Fatalf("a refused read still returned %d rows", len(got.Hops))
			}
		})
	}
}

// The counters query is built from a variable number of (source, timestamp)
// pairs, so its placeholder count moves with the input the other reads' fixed
// text never does.
func TestCycleCounterQueryPlaceholdersMatchArgs(t *testing.T) {
	ref := config.TargetRef{Group: "core", Target: config.Target{Name: "gw"}}
	ts := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	for _, n := range []int{1, 2, 7} {
		hops := make([]storage.HopPoint, 0, n)
		for i := range n {
			hops = append(hops, storage.HopPoint{Source: fmt.Sprintf("s%d", i), Time: ts.Add(time.Duration(i) * time.Second)})
		}
		conn := &recordConn{}
		if _, err := (&Reader{conn: conn}).queryCycleCounters(context.Background(), ref, hops); err != nil {
			t.Fatalf("%d keys: %v", n, err)
		}
		if got, want := len(conn.args), strings.Count(conn.query, "?"); got != want {
			t.Fatalf("%d keys: %d placeholders, %d args\nquery:%s\nargs:%v", n, want, got, conn.query, conn.args)
		}
		if !strings.Contains(conn.query, "FROM probe_cycle") {
			t.Fatalf("%d keys: counters read the wrong table:%s", n, conn.query)
		}
	}
}

// One hop read pins one cycle per source, so many hop rows collapse to few
// keys; asking probe_cycle once per row would turn a 30-hop path into 30
// point lookups.
func TestCycleKeysDedupePerCycle(t *testing.T) {
	ts := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	hops := []storage.HopPoint{
		{Source: "a", Time: ts, Index: 1},
		{Source: "a", Time: ts, Index: 2},
		{Source: "b", Time: ts.Add(time.Second), Index: 1},
		{Source: "b", Time: ts.Add(time.Second), Index: 2},
	}
	got := cycleKeys(hops)
	if len(got) != 2 {
		t.Fatalf("cycleKeys = %+v, want one key per source", got)
	}
	if got[0].Source != "a" || !got[0].Time.Equal(ts) || got[1].Source != "b" {
		t.Fatalf("cycleKeys lost identity or order: %+v", got)
	}
}

// A hop read with no rows names no cycle, so it must not issue the counters
// query at all — an empty IN list is not a valid one.
func TestEmptyHopReadIssuesNoCounterQuery(t *testing.T) {
	conn := &recordConn{}
	if _, err := (&Reader{conn: conn}).QueryLatestHops(context.Background(),
		config.TargetRef{Group: "core", Target: config.Target{Name: "gw"}}, storage.QueryFilter{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(conn.query, "FROM probe_cycle") {
		t.Fatalf("an empty hop read still queried probe_cycle:%s", conn.query)
	}
}

// The counters query interpolates one tuple per key, so the key count is what
// bounds its text and its argument slice. Ingest holds live source names to
// 512, and a hop read pins one cycle per source, so a read naming more cycles
// than the cap is refused rather than turned into an unbounded query.
func TestCycleCountersRefuseTooManyKeys(t *testing.T) {
	ref := config.TargetRef{Group: "core", Target: config.Target{Name: "gw"}}
	ts := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	hops := make([]storage.HopPoint, 0, maxCycleCounterKeys+1)
	for i := range maxCycleCounterKeys + 1 {
		hops = append(hops, storage.HopPoint{Source: strconv.Itoa(i), Time: ts})
	}
	conn := &recordConn{}
	if _, err := (&Reader{conn: conn}).queryCycleCounters(context.Background(), ref, hops); !errors.Is(err, storage.ErrHopsTruncated) {
		t.Fatalf("err = %v, want ErrHopsTruncated", err)
	}
	if conn.query != "" {
		t.Fatalf("the refused read still issued a query:%s", conn.query)
	}
	if _, err := (&Reader{conn: conn}).queryCycleCounters(context.Background(), ref, hops[:maxCycleCounterKeys]); err != nil {
		t.Fatalf("a read exactly at the key cap was refused: %v", err)
	}
}
