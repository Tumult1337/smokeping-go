//go:build integration

package clickhouse

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/tumult/gosmokeping/internal/cluster"
	"github.com/tumult/gosmokeping/internal/cluster/master"
	"github.com/tumult/gosmokeping/internal/cluster/slave"
	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/scheduler"
	"github.com/tumult/gosmokeping/internal/stats"
)

// ingestHarness is the production wiring end to end: the master's /cycles
// handler, the same scheduler.Fanout run_node builds, and a real ClickHouse
// Writer behind it. Nothing here is a fake — a duplicate that survives lands
// in the same tables the API reads.
type ingestHarness struct {
	handler  http.Handler
	writer   *Writer
	conn     driver.Conn
	registry *master.Registry
}

func newIngestHarness(t *testing.T, targets []config.Group) (*ingestHarness, func()) {
	t.Helper()
	cfg, cleanup := testDSN(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	if err := Bootstrap(ctx, log, cfg); err != nil {
		cleanup()
		t.Fatalf("bootstrap: %v", err)
	}
	w, err := NewWriter(ctx, log, cfg, 10)
	if err != nil {
		cleanup()
		t.Fatalf("new writer: %v", err)
	}
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{cfg.Addr},
		Auth: clickhouse.Auth{Database: cfg.Database, Username: cfg.Username, Password: cfg.Password},
	})
	if err != nil {
		w.Close()
		cleanup()
		t.Fatalf("open: %v", err)
	}

	store := config.NewStore("", &config.Config{
		Interval: 20 * time.Second,
		Pings:    10,
		Targets:  targets,
		Cluster:  &config.Cluster{Token: "tok", Source: "master"},
	})
	registry := master.NewRegistry(log)
	h := &ingestHarness{
		writer:   w,
		conn:     conn,
		registry: registry,
	}
	h.handler = master.NewServer(log, store, registry, scheduler.Fanout(log, w), nil).Handler()

	return h, func() {
		conn.Close()
		w.Close()
		cleanup()
	}
}

// post ships one batch through the real HTTP handler and returns the decoded
// ack so a caller can assert on accepted/duplicate counts.
func (h *ingestHarness) post(t *testing.T, slave string, batch cluster.CycleBatch) (int, map[string]any) {
	t.Helper()
	body, err := json.Marshal(batch)
	if err != nil {
		t.Fatalf("marshal batch: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/cycles", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("X-Slave-Name", slave)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	var ack map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &ack)
	return rec.Code, ack
}

// flush waits out the writer's 1s batch ticker so every offered row has
// reached ClickHouse before the assertions query it.
func (h *ingestHarness) flush() { time.Sleep(1500 * time.Millisecond) }

func (h *ingestHarness) cycleAgg(t *testing.T, group, name string) (rows, sent uint64) {
	t.Helper()
	var sentSum uint64
	if err := h.conn.QueryRow(context.Background(),
		"SELECT count(), toUInt64(sum(sent)) FROM probe_cycle WHERE target_group = ? AND target_id = ?",
		group, name,
	).Scan(&rows, &sentSum); err != nil {
		t.Fatalf("query probe_cycle: %v", err)
	}
	return rows, sentSum
}

func (h *ingestHarness) tableCount(t *testing.T, table, group, name string) uint64 {
	t.Helper()
	var n uint64
	if err := h.conn.QueryRow(context.Background(),
		"SELECT count() FROM "+table+" WHERE target_group = ? AND target_id = ?",
		group, name,
	).Scan(&n); err != nil {
		t.Fatalf("query %s: %v", table, err)
	}
	return n
}

func testGroups() []config.Group {
	return []config.Group{{
		Group: "g1",
		Targets: []config.Target{
			{Name: "t1", Probe: "icmp", Host: "192.0.2.1"},
			{Name: "t2", Probe: "icmp", Host: "192.0.2.2"},
		},
	}}
}

func samplePayload(at time.Time, group, name string, sent, lost int) cluster.CyclePayload {
	return cluster.CyclePayload{
		Time:      at,
		Group:     group,
		Name:      name,
		ProbeName: "icmp",
		Sent:      sent,
		LossCount: lost,
		RTTs:      []time.Duration{3 * time.Millisecond, 4 * time.Millisecond},
		Summary: stats.Summary{
			Min: 3 * time.Millisecond, Max: 4 * time.Millisecond,
			Mean: 3500 * time.Microsecond, Median: 3500 * time.Microsecond,
		},
		Hops: []cluster.HopDTO{
			{Index: 1, IP: "192.0.2.254", Sent: 3, Lost: 0, RTTs: []time.Duration{time.Millisecond}},
			{Index: 2, IP: "192.0.2.1", TargetReply: true, Sent: 3, Lost: 0, RTTs: []time.Duration{3 * time.Millisecond}},
		},
	}
}

// The defect: PushSink.Requeue resends a batch whose ack was lost, the master
// ingests it again, and every MergeTree table keeps both copies — so the
// bucketed sum(sent) an operator reads as loss percentage doubles off a
// network blip. Assert on the stored aggregate, not on a log line.
func TestClusterIngestDoesNotDoubleWriteARedeliveredBatch(t *testing.T) {
	h, done := newIngestHarness(t, testGroups())
	defer done()

	h.registry.Touch("edge-1", "", "", "")
	at := time.Now().UTC().Truncate(time.Millisecond)
	batch := cluster.CycleBatch{
		Source: "edge-1",
		Cycles: []cluster.CyclePayload{samplePayload(at, "g1", "t1", 20, 5)},
	}

	if code, ack := h.post(t, "edge-1", batch); code != http.StatusOK || ack["accepted"] != float64(1) {
		t.Fatalf("first push: code=%d ack=%v, want 200 accepted=1", code, ack)
	}
	// The identical bytes the slave requeues after a lost ack.
	if code, ack := h.post(t, "edge-1", batch); code != http.StatusOK ||
		ack["accepted"] != float64(0) || ack["duplicate"] != float64(1) {
		t.Fatalf("redelivery: code=%d ack=%v, want 200 accepted=0 duplicate=1", code, ack)
	}
	h.flush()

	rows, sent := h.cycleAgg(t, "g1", "t1")
	if rows != 1 || sent != 20 {
		t.Errorf("probe_cycle: got %d rows summing sent=%d, want 1 row summing 20", rows, sent)
	}
	if n := h.tableCount(t, "probe_rtt", "g1", "t1"); n != 2 {
		t.Errorf("probe_rtt: got %d rows, want 2", n)
	}
	if n := h.tableCount(t, "probe_hop", "g1", "t1"); n != 2 {
		t.Errorf("probe_hop: got %d rows, want 2", n)
	}
}

// The counterpart that fails if dedup starts swallowing real measurements:
// distinct timestamps, distinct targets and distinct sources are all separate
// cycles and every one of them must be stored.
func TestClusterIngestStoresEveryDistinctCycle(t *testing.T) {
	h, done := newIngestHarness(t, testGroups())
	defer done()

	h.registry.Touch("edge-1", "", "", "")
	h.registry.Touch("edge-2", "", "", "")
	at := time.Now().UTC().Truncate(time.Millisecond)

	// Same source, same target, two timestamps.
	if code, _ := h.post(t, "edge-1", cluster.CycleBatch{Cycles: []cluster.CyclePayload{
		samplePayload(at, "g1", "t1", 20, 0),
		samplePayload(at.Add(-20*time.Second), "g1", "t1", 20, 0),
	}}); code != http.StatusOK {
		t.Fatalf("two-timestamp push: %d", code)
	}
	// Same source, same timestamp, a different target in the same group.
	if code, _ := h.post(t, "edge-1", cluster.CycleBatch{Cycles: []cluster.CyclePayload{
		samplePayload(at, "g1", "t2", 20, 0),
	}}); code != http.StatusOK {
		t.Fatalf("second-target push: %d", code)
	}
	// A second slave reporting the very same measurement identity. This is
	// the whole point of per-source keying: two sources probing one host must
	// each keep their own row, or one peer's data mutes the other's.
	if code, _ := h.post(t, "edge-2", cluster.CycleBatch{Cycles: []cluster.CyclePayload{
		samplePayload(at, "g1", "t1", 20, 0),
	}}); code != http.StatusOK {
		t.Fatalf("second-source push: %d", code)
	}
	h.flush()

	if rows, sent := h.cycleAgg(t, "g1", "t1"); rows != 3 || sent != 60 {
		t.Errorf("probe_cycle g1/t1: got %d rows summing sent=%d, want 3 rows summing 60", rows, sent)
	}
	if rows, _ := h.cycleAgg(t, "g1", "t2"); rows != 1 {
		t.Errorf("probe_cycle g1/t2: got %d rows, want 1", rows)
	}
}

// The duplicate above is hand-built. This one comes out of the real producer:
// slave.PushSink drains a batch, the ack is lost, Requeue puts it back at the
// ring's head and the next Drain hands over the very same cycles.
func TestClusterIngestDedupsARealPushSinkRequeue(t *testing.T) {
	h, done := newIngestHarness(t, testGroups())
	defer done()

	h.registry.Touch("edge-1", "", "", "")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sink := slave.NewPushSink(log, config.DefaultBufferBytes)
	sink.SetHopMarkers(true)

	at := time.Now().UTC().Truncate(time.Millisecond)
	for i := 0; i < 3; i++ {
		sink.OnCycle(context.Background(), scheduler.Cycle{
			Time:      at.Add(time.Duration(-i) * 20 * time.Second),
			Target:    config.TargetRef{Group: "g1", Target: config.Target{Name: "t1", Probe: "icmp"}},
			ProbeName: "icmp",
			Source:    "edge-1",
			Sent:      20,
			LossCount: 5,
			RTTs:      []time.Duration{3 * time.Millisecond, 4 * time.Millisecond},
			Summary:   stats.Summary{Min: 3 * time.Millisecond, Max: 4 * time.Millisecond},
		})
	}

	first := sink.Drain(100)
	if len(first) != 3 {
		t.Fatalf("drained %d cycles, want 3", len(first))
	}
	if code, _ := h.post(t, "edge-1", cluster.CycleBatch{Cycles: first}); code != http.StatusOK {
		t.Fatalf("first push: %d", code)
	}
	// The ack never arrives, so the runner requeues and the next flush ships
	// the identical cycles again.
	sink.Requeue(first)
	second := sink.Drain(100)
	code, ack := h.post(t, "edge-1", cluster.CycleBatch{Cycles: second})
	if code != http.StatusOK || ack["accepted"] != float64(0) || ack["duplicate"] != float64(3) {
		t.Fatalf("requeued push: code=%d ack=%v, want 200 accepted=0 duplicate=3", code, ack)
	}
	h.flush()

	if rows, sent := h.cycleAgg(t, "g1", "t1"); rows != 3 || sent != 60 {
		t.Errorf("probe_cycle: got %d rows summing sent=%d, want 3 rows summing 60", rows, sent)
	}
}
