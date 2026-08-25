package clickhouse

import (
	"context"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/probe"
	"github.com/tumult/gosmokeping/internal/scheduler"
)

// TestOfferDropsWhenChannelFull asserts the offer primitive drops rather
// than blocks when the per-table channel has no space. This is the in-
// memory channel-saturation case; the post-Close case is covered by
// TestOfferDropsAfterClose below.
func TestOfferDropsWhenChannelFull(t *testing.T) {
	w := newTestWriter(t, 1) // tiny channel buffer, no consumer goroutines
	defer w.Close() //nolint:errcheck // test cleanup

	// First send fills the channel; the rest must be dropped immediately.
	for i := 0; i < 10; i++ {
		w.OnCycle(context.Background(), testCycle(time.Now()))
	}
	if got := atomic.LoadUint64(&w.dropped[tableProbeCycle]); got != 9 {
		t.Fatalf("expected exactly 9 drops (buffer=1, sends=10), got %d", got)
	}
}

// TestOfferDropsAfterClose proves the writer's Close contract: once Close
// has returned, further OnCycle calls increment the drop counter rather
// than queueing into a channel with no reader. Regression guard for a
// silent-data-loss bug — the original implementation only checked channel
// fullness, which made post-Close sends succeed (into the buffer) and
// vanish on GC with no observability.
func TestOfferDropsAfterClose(t *testing.T) {
	w := newTestWriter(t, 16) // generous buffer; we're testing the closed flag, not fullness
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	before := atomic.LoadUint64(&w.dropped[tableProbeCycle])
	for i := 0; i < 5; i++ {
		w.OnCycle(context.Background(), testCycle(time.Now()))
	}
	after := atomic.LoadUint64(&w.dropped[tableProbeCycle])
	if after-before != 5 {
		t.Fatalf("expected 5 post-Close drops, got %d", after-before)
	}
}

// TestCloseIsIdempotent guards against double-close panics. The cmd-side
// composition root may call Close from multiple defers under unusual
// shutdown paths.
func TestCloseIsIdempotent(t *testing.T) {
	w := newTestWriter(t, 1)
	if err := w.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

// TestDroppedReports proves the Dropped() accessor returns a per-table
// snapshot suitable for surfacing via /api/v1/health or a metric.
func TestDroppedReports(t *testing.T) {
	w := newTestWriter(t, 0) // zero-buffer => every send drops
	defer w.Close() //nolint:errcheck // test cleanup

	w.OnCycle(context.Background(), testCycle(time.Now()))
	w.OnCycle(context.Background(), testCycle(time.Now()))
	got := w.Dropped()
	if got["probe_cycle"] != 2 {
		t.Fatalf("probe_cycle dropped = %d, want 2 (got %+v)", got["probe_cycle"], got)
	}
	if got["probe_rtt"] != 0 {
		t.Errorf("probe_rtt dropped = %d, want 0", got["probe_rtt"])
	}
}

// Test helpers used only by writer_test.go; kept here so they don't ship
// in the binary.

func newTestWriter(_ *testing.T, bufSize int) *Writer {
	w := &Writer{}
	for i := range w.chans {
		w.chans[i] = make(chan any, bufSize)
	}
	return w
}

func testCycle(at time.Time) scheduler.Cycle {
	return scheduler.Cycle{
		Time:   at,
		Target: config.TargetRef{Group: "core", Target: config.Target{Name: "gw"}},
		Source: "master",
	}
}

// recordBatch captures Append calls; unimplemented driver.Batch methods panic
// via the embedded nil interface, which is the point — a flush path reaching
// one is doing something this test cannot reason about.
type recordBatch struct {
	driver.Batch
	appended [][]any
	sent     bool
}

func (b *recordBatch) Append(v ...any) error { b.appended = append(b.appended, v); return nil }
func (b *recordBatch) Send() error           { b.sent = true; return nil }

type prepConn struct {
	driver.Conn
	query string
	batch *recordBatch
}

func (c *prepConn) PrepareBatch(_ context.Context, query string, _ ...driver.PrepareBatchOption) (driver.Batch, error) {
	c.query = query
	return c.batch, nil
}

// insertColumns extracts the explicit column list from "INSERT INTO t (a, b)".
func insertColumns(t *testing.T, query string) []string {
	t.Helper()
	open := strings.Index(query, "(")
	end := strings.Index(query, ")")
	if open < 0 || end < open {
		t.Fatalf("insert has no explicit column list: %q", query)
	}
	parts := strings.Split(query[open+1:end], ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// Every flush must name its columns and append exactly that many values, and
// every named column must exist in the DDL. This is the only unit-level guard
// against a positional insert drifting from the table — live ClickHouse is
// the alternative detector, and it fires in production.
func TestFlushColumnParity(t *testing.T) {
	cy := testCycle(time.Now())
	cy.Hops = []probe.Hop{{Index: 2, IP: "10.0.0.2", Sent: 3, Lost: 1, Unreach: "host-unreachable"}}

	flushes := []struct {
		name string
		ddl  string
		run  func(*Writer, context.Context) error
	}{
		{"cycles", ddlProbeCycle, func(w *Writer, ctx context.Context) error {
			return w.flushCycles(ctx, []any{cy})
		}},
		{"rtts", ddlProbeRTT, func(w *Writer, ctx context.Context) error {
			return w.flushRTTs(ctx, []any{rttRow{ts: cy.Time, target: "gw", group: "core", source: "master"}})
		}},
		{"hops", ddlProbeHop, func(w *Writer, ctx context.Context) error {
			return w.flushHops(ctx, []any{hopRow{cycle: cy, hop: cy.Hops[0]}})
		}},
		{"http", ddlProbeHTTP, func(w *Writer, ctx context.Context) error {
			return w.flushHTTP(ctx, []any{httpRow{ts: cy.Time, target: "gw", group: "core", source: "master"}})
		}},
	}
	for _, f := range flushes {
		t.Run(f.name, func(t *testing.T) {
			conn := &prepConn{batch: &recordBatch{}}
			w := &Writer{conn: conn}
			if err := f.run(w, context.Background()); err != nil {
				t.Fatal(err)
			}
			cols := insertColumns(t, conn.query)
			for _, c := range cols {
				// Whole-word match on a DDL column line: plain Contains passes
				// a misnamed "probe" against "probe_type"/"probe_cycle".
				re := regexp.MustCompile(`(?m)^\s+` + regexp.QuoteMeta(c) + `\s`)
				if !re.MatchString(f.ddl) {
					t.Fatalf("insert names column %q the DDL lacks", c)
				}
			}
			if len(conn.batch.appended) == 0 {
				t.Fatal("nothing appended")
			}
			for _, args := range conn.batch.appended {
				if len(args) != len(cols) {
					t.Fatalf("appended %d values for %d named columns", len(args), len(cols))
				}
			}
			if !conn.batch.sent {
				t.Fatal("batch never sent")
			}
		})
	}
}

// The hop flush must actually carry the annotations at the columns it names —
// parity alone passes if a field is omitted from both sides.
func TestFlushHopsCarriesAnnotations(t *testing.T) {
	cy := testCycle(time.Now())
	hop := probe.Hop{Index: 2, IP: "10.0.0.2", Sent: 3, Lost: 1, Unreach: "admin-prohibited", TargetReply: true}
	conn := &prepConn{batch: &recordBatch{}}
	w := &Writer{conn: conn}
	if err := w.flushHops(context.Background(), []any{hopRow{cycle: cy, hop: hop}}); err != nil {
		t.Fatal(err)
	}
	cols := insertColumns(t, conn.query)
	args := conn.batch.appended[0]
	byCol := map[string]any{}
	for i, c := range cols {
		byCol[c] = args[i]
	}
	if byCol["unreach"] != "admin-prohibited" {
		t.Fatalf("unreach column = %v, want admin-prohibited (cols %v)", byCol["unreach"], cols)
	}
	if byCol["target_reply"] != uint8(1) {
		t.Fatalf("target_reply column = %v, want uint8(1)", byCol["target_reply"])
	}
	if byCol["hop_addr"] != "10.0.0.2" {
		t.Fatalf("hop_addr column = %v", byCol["hop_addr"])
	}
	// Pins the identity-field mapping so a hopRow reshape cannot silently
	// swap columns that happen to share a type.
	if byCol["target_id"] != "gw" || byCol["target_group"] != "core" || byCol["source"] != "master" {
		t.Fatalf("identity columns scrambled: id=%v group=%v source=%v",
			byCol["target_id"], byCol["target_group"], byCol["source"])
	}
}
