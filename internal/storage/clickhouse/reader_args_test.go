package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/tumult/gosmokeping/internal/cluster"
	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/storage"
)

// recordConn captures every Query call, not only the last: a reader method
// that issues two — the pinned hop reads pair their rows with a probe_cycle
// counters lookup — would otherwise have its first query overwritten and
// checked by nothing. Every other driver.Conn method panics: a reader path
// reaching one would be doing something this test cannot reason about, and
// silently returning zero values would let it pass.
type recordConn struct {
	driver.Conn
	query   string
	args    []any
	queries []string
	argSets [][]any
}

func (c *recordConn) Query(_ context.Context, query string, args ...any) (driver.Rows, error) {
	c.query, c.args = query, args
	c.queries = append(c.queries, query)
	c.argSets = append(c.argSets, args)
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
// "latest" until the lie expired — and QueryHopsAt's ±window around a pin
// near now reaches the same rows. The ingest bound stops new ones; the reader
// ceiling is what keeps rows already in the table off both pinned reads.
func TestPinnedHopReadsBoundFutureRows(t *testing.T) {
	ref := config.TargetRef{Group: "core", Target: config.Target{Name: "gw"}}
	calls := map[string]func(*Reader) error{
		"QueryLatestHops": func(r *Reader) error {
			_, err := r.QueryLatestHops(context.Background(), ref, storage.QueryFilter{})
			return err
		},
		"QueryHopsAt": func(r *Reader) error {
			_, err := r.QueryHopsAt(context.Background(), ref, time.Now(), 30*time.Minute, storage.QueryFilter{})
			return err
		},
	}
	for name, call := range calls {
		conn := &recordConn{}
		if err := call(&Reader{conn: conn}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(conn.queries[0], "timestamp <= now() + INTERVAL") {
			t.Errorf("%s: no future ceiling in the pinning CTE:\n%s", name, conn.queries[0])
		}
	}
}

// A pinned hop read returns one cycle per source, and cluster ingest refuses a
// cycle carrying more than cluster.MaxHopsPerCycle hop rows, so the ceiling is
// that product across every source name the master's registry admits at once —
// referenced from the source constants, never hand-copied, so mutating either
// reddens this rather than leaving a literal that silently drifts under them.
func TestMaxHopRowsClearsEverySourcesPinnedCycle(t *testing.T) {
	if want := maxHopSources * cluster.MaxHopsPerCycle; maxHopRows != want {
		t.Fatalf("maxHopRows = %d, want the %d rows a full fleet's pinned read holds", maxHopRows, want)
	}
}

// maxHopSources mirrors a constant this package cannot import (master's
// maxRegisteredSlaves is unexported), so the pin is the same source-parsing
// guard internal/config/tracebounds_test.go uses for probe's trace bounds.
func TestMaxHopSourcesMirrorsTheMasterRegistry(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "../../cluster/master/registry.go", nil, 0)
	if err != nil {
		t.Fatalf("parse master/registry.go: %v", err)
	}
	found := -1
	ast.Inspect(file, func(n ast.Node) bool {
		if v, ok := n.(*ast.ValueSpec); ok && len(v.Names) == 1 && v.Names[0].Name == "maxRegisteredSlaves" && len(v.Values) == 1 {
			if lit, ok := v.Values[0].(*ast.BasicLit); ok && lit.Kind == token.INT {
				found, _ = strconv.Atoi(lit.Value)
			}
		}
		return true
	})
	if found < 0 {
		t.Fatal("could not find maxRegisteredSlaves in master/registry.go — the mirror can no longer be checked")
	}
	if found != maxHopSources {
		t.Errorf("master maxRegisteredSlaves = %d, maxHopSources = %d: update the mirror and maxHopRows follows", found, maxHopSources)
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

// A DateTime64(3) bound rendered from a time.Time reaches the server truncated
// to whole seconds, which moves a window edge by up to a second in whichever
// direction admits the wrong rows — and the wrongness is invisible without a
// live server, since the query still parses and still returns rows. Every
// timestamp predicate must therefore go through dtMilli, and every value bound
// into one must already be milliseconds.
//
// Three false negatives an operator-count check alone left open, closed here:
// a predicate rewritten to wrap either side in a second-precision conversion
// (`timestamp >= toDateTime(?)`), an argument carrying unix *seconds* where the
// placeholder wants milliseconds, and a second query issued by the same reader
// method — the counters lookup the pinned hop reads pair their rows with.
func TestReaderBindsEveryTimestampAsMilliseconds(t *testing.T) {
	ref := config.TargetRef{Group: "core", Target: config.Target{Name: "gw"}}
	from := time.Date(2026, 4, 1, 0, 0, 0, 900_000_000, time.UTC)
	to := from.Add(24 * time.Hour)
	f := storage.QueryFilter{Source: "edge-1", Step: 15 * time.Minute, LatestSince: from}

	calls := []struct {
		name string
		call func(*Reader) error
		// hopRow makes the conn answer the first query with one probe_hop row,
		// so the pinned reads go on to issue their counters lookup. A conn
		// that returns nothing stops before it and leaves that query unwritten.
		hopRow bool
	}{
		{name: "QueryCycles raw", call: func(r *Reader) error {
			_, err := r.QueryCycles(context.Background(), ref, from, to, storage.QueryFilter{Source: "edge-1"})
			return err
		}},
		{name: "QueryCycles step", call: func(r *Reader) error {
			_, err := r.QueryCycles(context.Background(), ref, from, to, f)
			return err
		}},
		{name: "QueryRTTs", call: func(r *Reader) error {
			_, err := r.QueryRTTs(context.Background(), ref, from, to, f)
			return err
		}},
		{name: "QueryHTTPSamples", call: func(r *Reader) error {
			_, err := r.QueryHTTPSamples(context.Background(), ref, from, to, f)
			return err
		}},
		{name: "QueryLatestHops", hopRow: true, call: func(r *Reader) error {
			_, err := r.QueryLatestHops(context.Background(), ref, f)
			return err
		}},
		{name: "QueryHopsAt", hopRow: true, call: func(r *Reader) error {
			_, err := r.QueryHopsAt(context.Background(), ref, from, 30*time.Minute, f)
			return err
		}},
		{name: "QueryHopsTimeline", call: func(r *Reader) error {
			_, err := r.QueryHopsTimeline(context.Background(), ref, from, to, f)
			return err
		}},
		{name: "QueryOverview", call: func(r *Reader) error {
			_, err := r.QueryOverview(context.Background(), from, to, []config.TargetRef{ref})
			return err
		}},
		// Also driven directly, for the two-key shape no single-row hop read
		// produces: this is the one reader query whose placeholder set and
		// argument slice both move with their input.
		{name: "queryCycleCounters", call: func(r *Reader) error {
			_, err := r.queryCycleCounters(context.Background(), ref, []storage.HopPoint{
				{Source: "edge-1", Time: from, Index: 1},
				{Source: "edge-2", Time: to, Index: 1},
			})
			return err
		}},
	}

	// The predicate must bind through dtMilli or against the server's own
	// clock; nothing else may sit on the right of a timestamp comparison.
	tsPredicate := regexp.MustCompile(`timestamp\s*(?:>=|<=|>|<|=)\s*`)
	// Every fixture instant carries 900ms and lives inside this window, so a
	// value truncated to a whole second or handed over in seconds is out of
	// band by three orders of magnitude rather than by a rounding.
	lo, hi := from.Add(-time.Hour).UnixMilli(), to.Add(time.Hour).UnixMilli()
	secondsForm := map[int64]bool{}
	for _, at := range []time.Time{from, to, from.Add(-15 * time.Minute), from.Add(15 * time.Minute)} {
		secondsForm[at.Unix()] = true
	}

	for _, tc := range calls {
		name := tc.name
		t.Run(name, func(t *testing.T) {
			conn := &recordConn{}
			var backing driver.Conn = conn
			if tc.hopRow {
				h := &hopRowConn{left: 1, at: from, src: "edge-1"}
				conn, backing = &h.recordConn, h
			}
			if err := tc.call(&Reader{conn: backing}); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if len(conn.queries) == 0 {
				t.Fatalf("%s issued no query", name)
			}
			if tc.hopRow && len(conn.queries) != 2 {
				t.Fatalf("%s issued %d queries, want the hop read and its counters lookup", name, len(conn.queries))
			}
			for qi, q := range conn.queries {
				args := conn.argSets[qi]
				// A conversion around either side of the comparison rounds to
				// whole seconds just as a bound time.Time does, and no reader
				// query has any other use for one.
				if strings.Contains(q, "toDateTime") {
					t.Errorf("%s query %d converts a timestamp at second precision:%s", name, qi, q)
				}
				for _, loc := range tsPredicate.FindAllStringIndex(q, -1) {
					rest := q[loc[1]:]
					if strings.HasPrefix(rest, dtMilli) || strings.HasPrefix(rest, "now()") {
						continue
					}
					t.Errorf("%s query %d binds %q against %.40q, not %s:%s",
						name, qi, q[loc[0]:loc[1]], rest, dtMilli, q)
				}
				for i, a := range args {
					if ts, ok := a.(time.Time); ok {
						t.Errorf("%s query %d: arg %d is a time.Time (%s); the driver renders it at whole-second precision", name, qi, i, ts)
					}
				}
				for i := range msPlaceholders(q) {
					if i >= len(args) {
						t.Fatalf("%s query %d: dtMilli placeholder %d has no argument", name, qi, i)
					}
					v, ok := args[i].(int64)
					if !ok {
						t.Errorf("%s query %d: arg %d fills a dtMilli placeholder with %T, want epoch milliseconds", name, qi, i, args[i])
						continue
					}
					if v < lo || v > hi || v%1000 != 900 {
						t.Errorf("%s query %d: arg %d = %d is not the millisecond form of a fixture instant (want %d..%d ending in 900)",
							name, qi, i, v, lo, hi)
					}
				}
				for i, a := range args {
					v, ok := a.(int64)
					if !ok || !secondsForm[v] {
						continue
					}
					// QueryOverview's bucket origin is the one instant bound in
					// whole seconds: intDiv(toUInt32(timestamp) - ?, ?) counts
					// seconds from `from`. Named rather than swept — it is the
					// known UInt32-epoch limitation, not a truncated predicate.
					if name == "QueryOverview" && v == from.Unix() {
						continue
					}
					t.Errorf("%s query %d: arg %d = %d is a fixture instant in seconds, not milliseconds:%s", name, qi, i, v, q)
				}
			}
		})
	}
}

// msPlaceholders returns the zero-based positions, among all of the query's
// placeholders, of the ones a dtMilli occurrence owns. dtMilli holds exactly
// one `?`, so its position is the count of placeholders before it.
func msPlaceholders(q string) map[int]struct{} {
	out := map[int]struct{}{}
	for pos := 0; ; {
		i := strings.Index(q[pos:], dtMilli)
		if i < 0 {
			return out
		}
		at := pos + i
		out[strings.Count(q[:at], "?")] = struct{}{}
		pos = at + len(dtMilli)
	}
}

// hopRowConn answers the first query with one probe_hop row so a pinned hop
// read goes on to issue its counters lookup, and records both.
type hopRowConn struct {
	recordConn
	left int
	at   time.Time
	src  string
}

func (c *hopRowConn) Query(ctx context.Context, query string, args ...any) (driver.Rows, error) {
	if _, err := c.recordConn.Query(ctx, query, args...); err != nil {
		return nil, err
	}
	if c.left == 0 {
		return emptyRows{}, nil
	}
	c.left--
	return &oneHopRow{at: c.at, src: c.src, left: 1}, nil
}

// oneHopRow fills only the two scan destinations that decide what the counters
// lookup asks for; every other column keeps its zero value.
type oneHopRow struct {
	driver.Rows
	at   time.Time
	src  string
	left int
}

func (r *oneHopRow) Next() bool {
	if r.left == 0 {
		return false
	}
	r.left--
	return true
}

func (r *oneHopRow) Scan(dest ...any) error {
	if len(dest) > 0 {
		if p, ok := dest[0].(*time.Time); ok {
			*p = r.at
		}
	}
	if len(dest) > 1 {
		if p, ok := dest[1].(*string); ok {
			*p = r.src
		}
	}
	return nil
}

func (*oneHopRow) Err() error   { return nil }
func (*oneHopRow) Close() error { return nil }
