package clickhouse

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
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
	defer w.Close()          //nolint:errcheck // test cleanup

	// First send fills the channel; the rest must be dropped immediately.
	for i := 0; i < 10; i++ {
		w.OnCycle(context.Background(), testCycle(time.Now()))
	}
	if got := atomic.LoadUint64(&w.dropped[tableProbeCycle]); got != 9 {
		t.Fatalf("expected exactly 9 drops (buffer=1, sends=10), got %d", got)
	}
}

// A full buffer means ClickHouse is stalling, and the buffer is what the
// operator's charts read from when the stall ends — so it must hold the
// stall's newest rows, like the flush-retry backlog and the slave push ring,
// not a frozen snapshot of its first minutes with the incident's tail dropped.
func TestOfferEvictsOldestWhenFull(t *testing.T) {
	w := newTestWriter(t, 2)
	defer w.Close() //nolint:errcheck // test cleanup

	t0 := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		w.OnCycle(context.Background(), testCycle(t0.Add(time.Duration(i)*time.Second)))
	}
	if got := atomic.LoadUint64(&w.dropped[tableProbeCycle]); got != 1 {
		t.Fatalf("dropped = %d, want exactly the evicted oldest row", got)
	}
	var got []time.Time
	for len(w.chans[tableProbeCycle]) > 0 {
		got = append(got, (<-w.chans[tableProbeCycle]).(scheduler.Cycle).Time)
	}
	want := []time.Time{t0.Add(time.Second), t0.Add(2 * time.Second)}
	if len(got) != len(want) || !got[0].Equal(want[0]) || !got[1].Equal(want[1]) {
		t.Fatalf("buffer holds %v, want the newest rows %v", got, want)
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
	defer w.Close()          //nolint:errcheck // test cleanup

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

// Sent is non-zero because OnCycle drops a no-measurement cycle: a fixture
// without it would make every offer/drop assertion vacuously pass.
func testCycle(at time.Time) scheduler.Cycle {
	return scheduler.Cycle{
		Time:      at,
		Target:    config.TargetRef{Group: "core", Target: config.Target{Name: "gw"}},
		Source:    "master",
		Sent:      10,
		LossCount: 1,
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
	conn := &prepConn{batch: &recordBatch{}}
	w := &Writer{log: discard, conn: conn}
	for i := range w.chans {
		// Room for every row, so the flushed count below is deterministic.
		w.chans[i] = make(chan any, 256)
	}
	w.wg.Add(1)
	go w.runTable(ctx, tableProbeHop, 1, time.Millisecond)

	const cycles = 200
	for i := 0; i < cycles; i++ {
		cy := testCycle(time.Now())
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

	// -race is the tearing detector, but the gate does not always run it — so
	// also assert the flushed data: every row arrived, and each carries the
	// stats of the exact three-sample slice the dispatcher was reading.
	if got := w.Dropped()["probe_hop"]; got != 0 {
		t.Fatalf("fixture overflowed its own buffer: %d drops", got)
	}
	if got := len(conn.batch.appended); got != cycles {
		t.Fatalf("flushed %d hop rows, want %d — rows were lost between OnCycle and Send", got, cycles)
	}
	cols := insertColumns(t, conn.query)
	for _, args := range conn.batch.appended {
		byCol := map[string]any{}
		for i, c := range cols {
			byCol[c] = args[i]
		}
		if byCol["rtt_min_us"] != uint32(10000) || byCol["rtt_max_us"] != uint32(30000) ||
			byCol["rtt_median_us"] != uint32(20000) || byCol["rtt_mean_us"] != uint32(20000) {
			t.Fatalf("flush read torn hop RTTs: min=%v max=%v median=%v mean=%v",
				byCol["rtt_min_us"], byCol["rtt_max_us"], byCol["rtt_median_us"], byCol["rtt_mean_us"])
		}
	}
	if !conn.batch.sent {
		t.Fatal("batch never sent")
	}
}

// Ingest accepts an RTT of exactly 0 (cluster.boundRTTs is [0, MaxSampleRTT]),
// and rttMS turned it into a NaN that probe_rtt stored and /rtts could not
// JSON-encode. 0 stores as itself; a negative — which no producer emits — is
// clamped like durUS clamps it.
func TestRTTZeroStoresZeroNotNaN(t *testing.T) {
	w := newTestWriter(t, 4)
	cy := testCycle(time.Now())
	cy.RTTs = []time.Duration{0, 5 * time.Millisecond, -time.Millisecond}
	w.OnCycle(context.Background(), cy)

	want := []float64{0, 5, 0}
	for i, wv := range want {
		select {
		case raw := <-w.chans[tableProbeRTT]:
			row := raw.(rttRow)
			if math.IsNaN(row.rttMS) || math.IsInf(row.rttMS, 0) {
				t.Fatalf("rtt %d queued as non-finite %v", i, row.rttMS)
			}
			if row.rttMS != wv {
				t.Fatalf("rtt %d queued as %v ms, want %v", i, row.rttMS, wv)
			}
		default:
			t.Fatalf("rtt %d never queued", i)
		}
	}
}

// Sent == 0 is no measurement, and flushCycles would store it as loss_pct 0 —
// a fabricated healthy point over a gap. The hop rows are real measurements
// with their own per-hop counters, so they still go.
func TestOnCycleSkipsNoMeasurementCycle(t *testing.T) {
	w := newTestWriter(t, 4)
	cy := testCycle(time.Now())
	cy.Sent, cy.LossCount = 0, 0
	cy.Hops = []probe.Hop{{Index: 1, IP: "10.0.0.1", Sent: 3, Lost: 3}}
	w.OnCycle(context.Background(), cy)

	if n := len(w.chans[tableProbeCycle]); n != 0 {
		t.Fatalf("queued %d cycle rows for a Sent==0 cycle, want 0", n)
	}
	if n := len(w.chans[tableProbeHop]); n != 1 {
		t.Fatalf("queued %d hop rows, want 1", n)
	}
	if got := w.Dropped()["probe_cycle"]; got != 0 {
		t.Fatalf("no-measurement cycle counted as %d buffer drops, want 0", got)
	}

	cy.Sent, cy.LossCount = 3, 3
	w.OnCycle(context.Background(), cy)
	if n := len(w.chans[tableProbeCycle]); n != 1 {
		t.Fatalf("queued %d cycle rows for a measured cycle, want 1", n)
	}
}

// failingConn refuses every batch, the shape of a ClickHouse that is up
// enough to answer but not to accept — a restart, a read-only replica.
type failingConn struct {
	driver.Conn
}

func (failingConn) PrepareBatch(context.Context, string, ...driver.PrepareBatchOption) (driver.Batch, error) {
	return nil, errors.New("clickhouse is down")
}

// While a flush is failing, runTable must stop draining its channel. pending's
// cap is a flat maxRows×flushRetainFactor for all four tables, so draining
// through it discards writerChanCap's per-table sizing exactly when it is
// needed — at the deployed 122-target/20s shape that left probe_hop with ~33
// seconds of backlog against probe_cycle's ~11 minutes, the imbalance the
// per-table sizing exists to remove.
func TestFailingFlushLeavesRowsInThePerTableBuffer(t *testing.T) {
	const bufSize = 64
	w := &Writer{conn: failingConn{}, log: slog.New(slog.DiscardHandler)}
	for i := range w.chans {
		w.chans[i] = make(chan any, bufSize)
	}
	const maxRows = 4
	ctx, cancel := context.WithCancel(context.Background())
	w.wg.Add(1)
	go w.runTable(ctx, tableProbeCycle, maxRows, 20*time.Millisecond)

	// Enough rows to outrun both maxRows and the retain cap several times.
	for i := range bufSize {
		w.offer(tableProbeCycle, testCycle(time.Now().Add(time.Duration(i)*time.Second)))
	}

	// Settle: several ticker periods, so the loop has failed a flush and had
	// every chance to keep draining if it were going to.
	deadline := time.Now().Add(5 * time.Second)
	held, stable := len(w.chans[tableProbeCycle]), 0
	for time.Now().Before(deadline) && stable < 20 {
		time.Sleep(5 * time.Millisecond)
		if n := len(w.chans[tableProbeCycle]); n == held {
			stable++
		} else {
			held, stable = n, 0
		}
	}
	cancel()
	w.wg.Wait()

	// The loop may take up to one pending batch before the first flush fails.
	if want := bufSize - 2*maxRows; held < want {
		t.Fatalf("the channel holds %d of %d rows, want at least %d — the backlog drained into pending, whose cap is the same for all four tables, so writerChanCap's per-table sizing is bypassed exactly when it matters",
			held, bufSize, want)
	}
}

// The batch block reaches runTable, which runs on a goroutine with no
// recover(): a zero period panics time.NewTicker and a non-positive row count
// panics makeslice. NewWriter is exported, so an unvalidated caller must get
// an error. Checked before dialing, which is also what makes it testable —
// the previous guard sat after clickhouse.Open and no unit test could reach it.
func TestBatchIntervalValidatesBothFields(t *testing.T) {
	for _, tc := range []struct {
		rows    int
		iv      string
		wantErr bool
	}{
		{1000, "1s", false},
		{0, "1s", true},
		{-1, "1s", true},
		{config.MaxBatchRows + 1, "1s", true},
		{1000, "0s", true},
		{1000, "-1s", true},
		{1000, "banana", true},
		{1000, "2h", true},
	} {
		_, err := batchInterval(config.ClickHouseBatch{MaxRows: tc.rows, MaxInterval: tc.iv})
		if tc.wantErr && err == nil {
			t.Errorf("max_rows=%d max_interval=%q accepted; runTable panics on it with no recover", tc.rows, tc.iv)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("max_rows=%d max_interval=%q: %v", tc.rows, tc.iv, err)
		}
	}
}

// NewWriter must call batchInterval, and must do it before dialing. Nothing
// pinned the call, so reverting it to the pre-fix inline ParseDuration — which
// never looked at MaxRows — left the whole repo green while the makeslice
// panic it exists to prevent came back. An unreachable address is the probe:
// a bad batch block has to be reported without a server.
func TestNewWriterValidatesTheBatchBlockBeforeDialing(t *testing.T) {
	cfg := config.ClickHouse{
		Addr:  "127.0.0.1:1", // nothing listens; a dial would fail differently
		Batch: config.ClickHouseBatch{MaxRows: -1, MaxInterval: "1s"},
	}
	_, err := NewWriter(context.Background(), slog.New(slog.DiscardHandler), cfg, 20)
	if err == nil {
		t.Fatal("NewWriter accepted max_rows=-1; runTable then panics makeslice on a goroutine with no recover")
	}
	if !strings.Contains(err.Error(), "max_rows") {
		t.Fatalf("err = %v, want the batch block named — a dial error here means the guard runs after the dial and cannot be reached without a server", err)
	}
}
