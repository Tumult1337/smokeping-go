package clickhouse

import (
	"context"
	"io"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/tumult/gosmokeping/internal/alert"
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
			return w.flushHops(ctx, []any{hopRow{
				ts: cy.Time, target: "gw", group: "core", source: "master", hop: cy.Hops[0],
			}})
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
	row := hopRow{ts: cy.Time, target: "gw", group: "core", source: "master", hop: hop}
	if err := w.flushHops(context.Background(), []any{row}); err != nil {
		t.Fatal(err)
	}
	cols := insertColumns(t, conn.query)
	args := conn.batch.appended[0]
	if len(args) != len(cols) {
		t.Fatalf("appended %d values for %d named columns", len(args), len(cols))
	}
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

// Capacities equalize time-to-overflow at the sizing estimate: a table with
// N rows per cycle gets N× the slots, clamped so a hostile pings value
// cannot balloon memory and a tiny one cannot shrink below today's floor.
// At the deployed 122-target/20s install: cycles buffer ≈671s of ClickHouse
// stall; hops at the clamp buffer ≈239s at icmp's 90-row worst case and
// ≈71s at MTR's 300-row worst case — before this change hops died first at
// 96s in the ORDINARY case.
func TestWriterChanCap(t *testing.T) {
	tests := []struct {
		table, pings, want int
	}{
		{tableProbeCycle, 10, 4096},
		{tableProbeRTT, 10, 40960},
		{tableProbeHop, 10, 131072},
		{tableProbeHTTP, 10, 8192},
		{tableProbeRTT, 0, 4096},      // floor: nonsense pings never shrinks below base
		{tableProbeRTT, 1000, 131072}, // ceiling
		{tableProbeCycle, 1000, 4096}, // pings does not inflate tables it doesn't drive
	}
	for _, tc := range tests {
		if got := writerChanCap(tc.table, tc.pings); got != tc.want {
			t.Errorf("writerChanCap(%s, %d) = %d, want %d", tableName(tc.table), tc.pings, got, tc.want)
		}
	}
	chans := newWriterChans(10)
	if cap(chans[tableProbeHop]) != 131072 || cap(chans[tableProbeCycle]) != 4096 {
		t.Fatalf("newWriterChans caps = %d/%d, want 131072/4096",
			cap(chans[tableProbeHop]), cap(chans[tableProbeCycle]))
	}
}

// OnCycle projects the cycle's identity onto every hop row, and hopRow no
// longer carries the cycle to fall back on. TestFlushHopsCarriesAnnotations
// builds its row by hand, so it covers the flush half of the mapping only.
func TestOnCycleProjectsHopIdentity(t *testing.T) {
	w := newTestWriter(t, 4)
	cy := testCycle(time.Now())
	cy.Hops = []probe.Hop{{Index: 2, IP: "10.0.0.2", Sent: 3, Lost: 1}}
	w.OnCycle(context.Background(), cy)

	select {
	case raw := <-w.chans[tableProbeHop]:
		row := raw.(hopRow)
		if !row.ts.Equal(cy.Time) || row.target != "gw" || row.group != "core" || row.source != "master" {
			t.Fatalf("hop identity = %v/%q/%q/%q, want %v/gw/core/master",
				row.ts, row.target, row.group, row.source, cy.Time)
		}
		if row.hop.Index != 2 || row.hop.IP != "10.0.0.2" {
			t.Fatalf("hop payload = %+v", row.hop)
		}
	default:
		t.Fatal("no hop row queued")
	}
}

// The hop flush must leave its caller's RTT slice as it found it: OnCycle
// queues a shallow probe.Hop, so the queued row's RTTs alias the scheduler
// Cycle that Fanout hands to every other sink.
func TestFlushHopsPreservesCallerRTTOrder(t *testing.T) {
	rtts := []time.Duration{30 * time.Millisecond, 10 * time.Millisecond, 20 * time.Millisecond}
	want := slices.Clone(rtts)
	conn := &prepConn{batch: &recordBatch{}}
	w := &Writer{conn: conn}
	row := hopRow{ts: time.Now(), target: "gw", group: "core", source: "master",
		hop: probe.Hop{Index: 1, IP: "10.0.0.1", Sent: 3, RTTs: rtts}}
	if err := w.flushHops(context.Background(), []any{row}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rtts, want) {
		t.Fatalf("flushHops reordered the caller's slice: %v, want %v", rtts, want)
	}
	cols := insertColumns(t, conn.query)
	byCol := map[string]any{}
	for i, c := range cols {
		byCol[c] = conn.batch.appended[0][i]
	}
	if byCol["rtt_min_us"] != uint32(10000) || byCol["rtt_max_us"] != uint32(30000) ||
		byCol["rtt_median_us"] != uint32(20000) || byCol["rtt_mean_us"] != uint32(20000) {
		t.Fatalf("hop stats wrong: min=%v max=%v median=%v mean=%v",
			byCol["rtt_min_us"], byCol["rtt_max_us"], byCol["rtt_median_us"], byCol["rtt_mean_us"])
	}
}

// The order guard above is deterministic; this one is the -race evidence for
// the same sharing. A real OnCycle queues the aliasing row, the real hop
// consumer goroutine flushes it, and the real Discord formatter walks the same
// cycle's hops — the two accesses have no happens-before edge between them.
func TestHopRTTsNotRacedBetweenFlushAndAlertDispatch(t *testing.T) {
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := config.NewStore("", &config.Config{
		Alerts: map[string]config.Alert{"down": {Condition: "loss_pct > 0", Actions: []string{"dc"}}},
		// Port 1 refuses immediately: the embed (and formatHops with it) is
		// built before the POST, which is the read under test.
		Actions: map[string]config.Action{"dc": {Type: "discord", URL: "http://127.0.0.1:1/"}},
	})
	dispatcher := alert.NewDispatcher(discard, store)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &Writer{log: discard, conn: &prepConn{batch: &recordBatch{}}}
	for i := range w.chans {
		w.chans[i] = make(chan any, 64)
	}
	w.wg.Add(1)
	go w.runTable(ctx, tableProbeHop, 1, time.Millisecond)

	for i := 0; i < 200; i++ {
		cy := testCycle(time.Now())
		cy.Sent, cy.LossCount = 3, 3
		cy.Hops = []probe.Hop{{Index: 1, IP: "10.0.0.1", Sent: 3, RTTs: []time.Duration{
			30 * time.Millisecond, 10 * time.Millisecond, 20 * time.Millisecond}}}
		w.OnCycle(ctx, cy)
		dispatcher.Dispatch(ctx, alert.Event{
			Time: cy.Time, Target: cy.Target, AlertName: "down",
			Alert: store.Current().Alerts["down"],
			Prev:  alert.StateOK, Next: alert.StateFiring, Cycle: cy,
		})
	}
	cancel()
	w.wg.Wait()
}
