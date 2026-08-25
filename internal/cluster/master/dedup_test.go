package master

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/cluster"
	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/scheduler"
	"github.com/tumult/gosmokeping/internal/slavehealth"
)

// A conformant slave may push cluster.MaxCyclesPerBatch cycles and requeue the
// whole batch when its ack is lost, so the window must recognise every one of
// them on redelivery — a window sized below that refuses to see a duplicate
// the producer is entitled to send.
func TestDedupWindowCoversTheLargestAcceptableBatch(t *testing.T) {
	d := newCycleDedup()
	for i := 0; i < cluster.MaxCyclesPerBatch; i++ {
		if !d.admit("edge-1", "g/t", int64(i)) {
			t.Fatalf("first delivery of cycle %d refused", i)
		}
	}
	for i := 0; i < cluster.MaxCyclesPerBatch; i++ {
		if d.admit("edge-1", "g/t", int64(i)) {
			t.Errorf("redelivered cycle %d admitted; window does not cover a full batch", i)
		}
	}
}

// One past the window is what the window is: the oldest identity is forgotten,
// which is the bounded-memory half of the guard.
func TestDedupForgetsPastTheWindow(t *testing.T) {
	d := newCycleDedup()
	for i := 0; i <= cluster.MaxCyclesPerBatch; i++ {
		if !d.admit("edge-1", "g/t", int64(i)) {
			t.Fatalf("first delivery of cycle %d refused", i)
		}
	}
	if !d.admit("edge-1", "g/t", 0) {
		t.Error("oldest identity still remembered one past the window")
	}
	if d.admit("edge-1", "g/t", int64(cluster.MaxCyclesPerBatch)) {
		t.Error("newest identity forgotten one past the window")
	}
}

// Two sources probing one host produce the same (target, timestamp) identity
// and both rows must be stored, so the window is keyed per source.
func TestDedupIsPerSource(t *testing.T) {
	d := newCycleDedup()
	if !d.admit("edge-1", "g/t", 100) {
		t.Fatal("edge-1 first delivery refused")
	}
	if !d.admit("edge-2", "g/t", 100) {
		t.Error("edge-2 refused an identity only edge-1 had reported")
	}
	if d.admit("edge-1", "g/t", 100) {
		t.Error("edge-1 redelivery admitted")
	}
}

// Two targets probed by one source share a cycle timestamp whenever their
// schedules line up, so the target is part of the identity, not decoration.
func TestDedupIsPerTarget(t *testing.T) {
	d := newCycleDedup()
	if !d.admit("edge-1", "g/t1", 100) {
		t.Fatal("g/t1 first delivery refused")
	}
	if !d.admit("edge-1", "g/t2", 100) {
		t.Error("g/t2 refused an identity only g/t1 had reported")
	}
	if !d.admit("edge-1", "other/t1", 100) {
		t.Error("other/t1 refused; the group is not part of the identity")
	}
	if d.admit("edge-1", "g/t1", 100) {
		t.Error("g/t1 redelivery admitted")
	}
}

// A backlog delivered after an outage carries timestamps older than cycles
// already stored, and every one is a real measurement — so the guard is a
// window over identities, never a high-water mark.
func TestDedupAdmitsAnUnseenOlderTimestamp(t *testing.T) {
	d := newCycleDedup()
	if !d.admit("edge-1", "g/t", 5000) {
		t.Fatal("newest cycle refused")
	}
	if !d.admit("edge-1", "g/t", 1000) {
		t.Error("older unseen cycle refused; the guard is behaving as a floor")
	}
}

func TestDedupEvictsLeastRecentlyUsedSourcePastTheCap(t *testing.T) {
	d := newCycleDedup()
	for i := 0; i < dedupMaxSources; i++ {
		d.admit(fmt.Sprintf("edge-%d", i), "g/t", 1)
	}
	// Re-touch every source but the first, making it the eviction victim.
	for i := 1; i < dedupMaxSources; i++ {
		d.admit(fmt.Sprintf("edge-%d", i), "g/t", 2)
	}
	d.admit("edge-new", "g/t", 1)

	if len(d.bySource) != dedupMaxSources {
		t.Errorf("tracking %d sources, want %d", len(d.bySource), dedupMaxSources)
	}
	if _, ok := d.bySource["edge-0"]; ok {
		t.Error("least recently used source survived eviction")
	}
	if d.admit("edge-1", "g/t", 2) {
		t.Error("a recently used source was evicted instead")
	}
	// Eviction costs the source its dedup state, never its data: the whole
	// point of failing open here is that a peer pushed out of the map keeps
	// ingesting rather than going silent.
	if !d.admit("edge-0", "g/t", 1) {
		t.Error("evicted source refused; eviction is failing closed and muting a peer")
	}
}

func TestDedupAdmitIsRaceFree(t *testing.T) {
	d := newCycleDedup()
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 200; n++ {
				if d.admit("edge-1", "g/t", int64(n)) {
					mu.Lock()
					admitted++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	if admitted != 200 {
		t.Errorf("admitted %d of 200 distinct identities exactly once", admitted)
	}
}

type recordingSink struct {
	mu     sync.Mutex
	cycles []scheduler.Cycle
}

func (r *recordingSink) OnCycle(_ context.Context, c scheduler.Cycle) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cycles = append(r.cycles, c)
}

func (r *recordingSink) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.cycles)
}

// Health targets live outside the stored config, so they resolve through the
// mesh snapshot rather than AllTargets — the guard has to cover that arm too
// or a redelivered slave-health cycle still double-writes.
func TestIngestBatchDedupsHealthTargets(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	store := config.NewStore("", &config.Config{Cluster: &config.Cluster{Token: "tok"}})
	sink := &recordingSink{}
	peers := []slavehealth.Peer{{Name: "edge-2", Addr: netip.MustParseAddr("10.0.0.2")}}
	set := slavehealth.NewSet(peers, nil)
	srv := NewServer(log, store, NewRegistry(log), sink, func() *slavehealth.Set { return set })

	at := time.Now().UTC()
	batch := cluster.CycleBatch{Source: "edge-1", Cycles: []cluster.CyclePayload{{
		Time: at, Group: slavehealth.Group, Name: "edge-2", Sent: 10,
	}}}

	if n, dup := srv.ingestBatch(nil, batch); n != 1 || dup != 0 {
		t.Fatalf("first ingest: accepted=%d duplicate=%d, want 1/0", n, dup)
	}
	if n, dup := srv.ingestBatch(nil, batch); n != 0 || dup != 1 {
		t.Fatalf("redelivery: accepted=%d duplicate=%d, want 0/1", n, dup)
	}
	if got := sink.len(); got != 1 {
		t.Errorf("sink saw %d cycles, want 1", got)
	}
}

// A cycle the master cannot resolve is dropped before the window sees it, so a
// stale slave's targets cannot burn the window slots a live one needs.
func TestIngestBatchDoesNotSpendWindowOnUnknownTargets(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	store := config.NewStore("", &config.Config{Cluster: &config.Cluster{Token: "tok"}})
	srv := NewServer(log, store, NewRegistry(log), &recordingSink{}, nil)

	batch := cluster.CycleBatch{Source: "edge-1", Cycles: []cluster.CyclePayload{{
		Time: time.Now().UTC(), Group: "gone", Name: "t1", Sent: 10,
	}}}
	if n, dup := srv.ingestBatch(nil, batch); n != 0 || dup != 0 {
		t.Fatalf("unknown target: accepted=%d duplicate=%d, want 0/0", n, dup)
	}
	if w, ok := srv.dedup.bySource["edge-1"]; ok && len(w.ring) != 0 {
		t.Errorf("unresolvable cycle consumed %d window slots", len(w.ring))
	}
}

// The ack reports the split so an operator reading it can tell a slave whose
// batches are landing from one whose acks are being lost.
func TestHandleCyclesReportsDuplicatesInTheAck(t *testing.T) {
	srv := newTestServer()
	srv.registry.Touch("edge-1", "", "", "")

	req := httptest.NewRequest(http.MethodPost, "/cycles", strings.NewReader(`{"cycles":[]}`))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("X-Slave-Name", "edge-1")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("empty batch: %d", rec.Code)
	}
	got := rec.Body.String()
	if !strings.Contains(got, `"accepted":0`) || !strings.Contains(got, `"duplicate":0`) {
		t.Errorf("ack = %s, want both counters present", got)
	}
}

// One batch carrying the same identity twice is the same redelivery arriving
// on one request rather than two, and collapses the same way.
func TestIngestBatchCollapsesAnIntraBatchDuplicate(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	store := config.NewStore("", &config.Config{
		Cluster: &config.Cluster{Token: "tok"},
		Targets: []config.Group{{Group: "g", Targets: []config.Target{{Name: "t", Probe: "icmp"}}}},
	})
	sink := &recordingSink{}
	srv := NewServer(log, store, NewRegistry(log), sink, nil)

	at := time.Now().UTC()
	p := cluster.CyclePayload{Time: at, Group: "g", Name: "t", Sent: 5}
	if n, dup := srv.ingestBatch(nil, cluster.CycleBatch{
		Source: "edge-1", Cycles: []cluster.CyclePayload{p, p},
	}); n != 1 || dup != 1 {
		t.Fatalf("accepted=%d duplicate=%d, want 1/1", n, dup)
	}
	if got := sink.len(); got != 1 {
		t.Errorf("sink saw %d cycles, want 1", got)
	}
}

// The intern table is the window's memory bound rather than a behavioural one,
// so nothing else would notice it growing without limit.
func TestDedupInternTableStaysWithinTheWindow(t *testing.T) {
	d := newCycleDedup()
	for i := 0; i < 4*dedupWindowPerSource; i++ {
		d.admit("edge-1", fmt.Sprintf("g/t%d", i), 1)
	}
	names := len(d.bySource["edge-1"].names)
	if names > dedupWindowPerSource+1 {
		t.Errorf("intern table holds %d keys for a %d-entry window", names, dedupWindowPerSource)
	}
	if names == 0 {
		t.Error("intern table empty; interning is not happening at all")
	}
}
