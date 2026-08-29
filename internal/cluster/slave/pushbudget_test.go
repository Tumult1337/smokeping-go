package slave

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/cluster"
	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/probe"
	"github.com/tumult/gosmokeping/internal/scheduler"
)

func quietSink(t *testing.T, budget int64) *PushSink {
	t.Helper()
	return NewPushSink(slog.New(slog.DiscardHandler), budget)
}

// mtrCycle is the shape the budget exists for: hopRows responders, each
// answering rounds times.
func mtrCycle(name string, hopRows, rounds int) scheduler.Cycle {
	hops := make([]probe.Hop, hopRows)
	for i := range hops {
		rtts := make([]time.Duration, rounds)
		for j := range rtts {
			rtts[j] = time.Duration(i*rounds+j) * time.Millisecond
		}
		hops[i] = probe.Hop{
			Index: i%30 + 1,
			IP:    fmt.Sprintf("2001:db8:%x:%x:8a2e:370:7334:%x", name[0], i, i+1),
			RTTs:  rtts,
			Sent:  rounds,
		}
	}
	return scheduler.Cycle{
		Time:      time.Now(),
		Target:    config.TargetRef{Group: "core", Target: config.Target{Name: name, Probe: "mtr"}},
		ProbeName: "mtr",
		Sent:      20,
		LossCount: 1,
		RTTs:      []time.Duration{1, 2, 3},
		Hops:      hops,
	}
}

// A cycle too large for the budget is still admitted when it is alone: config
// bounds neither an interval nor a target count, so a payload that can never
// fit would be refused, re-produced and refused again for the life of the
// process. The sink must go over budget rather than go silent.
func TestLonePayloadIsAdmittedPastTheBudget(t *testing.T) {
	// Far under one mtr walk, so admission genuinely has to go over budget.
	s := quietSink(t, payloadStructBytes)
	big := mtrCycle("a", 300, 10)
	s.OnCycle(context.Background(), big)

	if got := s.Len(); got != 1 {
		t.Fatalf("buffered %d payloads, want 1 — the sink refused a cycle it can never fit", got)
	}
	if batch := s.Drain(10); len(batch) != 1 || len(batch[0].Hops) != 300 {
		t.Fatalf("the lone payload was shed: %d payloads, %d hop rows", len(batch), len(batch[0].Hops))
	}
}

// The whole point of the change: under pressure the sink sheds path history
// from the oldest cycles and keeps every cycle's loss and latency. Anything
// that inverts the ladder, or strips the newest, or clears RTTs alongside
// Hops, reddens here.
func TestBudgetShedsHopsFromTheOldestAndKeepsEveryMeasurement(t *testing.T) {
	const cycles = 40
	one := payloadHeapBytes(cluster.FromCycle(mtrCycle("a", 30, 10)))
	// Room for the ring plus roughly a third of the hop payloads, so the
	// ladder must shed but never has to drop.
	budget := int64(initialRingEntries*4)*payloadStructBytes + one*cycles/3

	s := quietSink(t, budget)
	for i := range cycles {
		s.OnCycle(context.Background(), mtrCycle(string(rune('a'+i)), 30, 10))
	}

	batch := s.Drain(cycles)
	if len(batch) != cycles {
		t.Fatalf("drained %d payloads, want all %d — the sink dropped a cycle while hop rows were still sheddable", len(batch), cycles)
	}
	for i, p := range batch {
		if p.Sent != 20 || p.LossCount != 1 || len(p.RTTs) != 3 {
			t.Fatalf("payload %d lost its measurement: sent=%d loss=%d rtts=%d", i, p.Sent, p.LossCount, len(p.RTTs))
		}
	}
	// Shedding runs oldest-first, so the surviving hops form a suffix.
	firstWithHops := -1
	for i, p := range batch {
		if len(p.Hops) > 0 {
			firstWithHops = i
			break
		}
	}
	if firstWithHops <= 0 {
		t.Fatalf("first payload still carrying hops is at index %d, want a strictly positive index — nothing was shed, or the newest was shed", firstWithHops)
	}
	for i := firstWithHops; i < len(batch); i++ {
		if len(batch[i].Hops) == 0 {
			t.Fatalf("payload %d has no hops but payload %d does — shedding is not oldest-first", i, firstWithHops)
		}
	}
}

// Once every buffered cycle is hopless the ladder has nothing left to shed, so
// it must fall through to dropping rather than spin. The sink is deliberately
// given a budget below its own ring array, which is the case that deadlocked
// under mu before the loop gained its size>0 exit.
func TestLadderTerminatesWhenNothingIsLeftToShed(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Under the ring's own array, which is inside the budget and which
		// neither arm can reclaim. The loop's only exit here is the empty
		// buffer, so this is the case that spun while holding mu.
		s := quietSink(t, payloadStructBytes)
		for i := range 200 {
			s.OnCycle(context.Background(), mtrCycle(string(rune('a'+i%26)), 20, 10))
		}
		if got := s.Len(); got != 1 {
			t.Errorf("buffered %d payloads, want the 1 admitted past the budget", got)
		}
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the shed ladder did not terminate")
	}
}

// The accounting has to survive a round trip, or a long-lived sink drifts
// until it refuses everything or bounds nothing.
func TestBytesConservesAcrossDrain(t *testing.T) {
	s := quietSink(t, config.DefaultBufferBytes)
	empty := s.Bytes()

	var inserted int64
	for i := range 20 {
		c := mtrCycle(string(rune('a'+i)), 12, 4)
		inserted += payloadHeapBytes(cluster.FromCycle(c))
		s.OnCycle(context.Background(), c)
	}
	if got := s.Bytes() - empty; got != inserted {
		t.Fatalf("Bytes() rose by %d, want %d", got, inserted)
	}

	half := s.Drain(10)
	var drained int64
	for _, p := range half {
		drained += payloadHeapBytes(p)
	}
	if got := s.Bytes() - empty; got != inserted-drained {
		t.Fatalf("after draining %d bytes Bytes() holds %d, want %d", drained, got, inserted-drained)
	}

	s.Drain(10)
	if got := s.Bytes(); got != empty {
		t.Fatalf("Bytes() = %d after draining everything, want the empty-ring %d", got, empty)
	}
}

// A slice's backing array is what the process holds, not its length. RTTs is
// the only lever: FromCycle copies hops with make(..., len(...)), so a
// cap>len hop slice cannot survive into the payload, while c.RTTs is passed
// through by reference.
func TestAccountingCountsCapacityNotLength(t *testing.T) {
	s := quietSink(t, config.DefaultBufferBytes)
	empty := s.Bytes()

	rtts := make([]time.Duration, 4, 1024)
	c := scheduler.Cycle{
		Time:      time.Now(),
		Target:    config.TargetRef{Group: "core", Target: config.Target{Name: "gw"}},
		ProbeName: "icmp",
		RTTs:      rtts,
	}
	s.OnCycle(context.Background(), c)

	want := int64(len("core")) + int64(len("gw")) + int64(len("icmp")) + 1024*durationBytes
	if got := s.Bytes() - empty; got != want {
		t.Fatalf("Bytes() rose by %d, want %d — the accounting read len, not cap", got, want)
	}
}

// The ring's own array is inside the budget, so growth is gated by it. Without
// that gate a doubling ring walks past the ceiling with no payload heap
// involved at all.
func TestGrowthStaysInsideTheBudget(t *testing.T) {
	// Sized so the *doubling* is what crosses the ceiling: 512 entries fit
	// with room to spare, 1024 do not. Payloads are kept tiny so the array
	// term, not their heap, is what the gate has to refuse.
	budget := int64(initialRingEntries*12) * payloadStructBytes
	s := quietSink(t, budget)

	for i := range 5000 {
		s.OnCycle(context.Background(), scheduler.Cycle{
			Time:      time.Now(),
			Target:    config.TargetRef{Group: "core", Target: config.Target{Name: string(rune('a' + i%26))}},
			ProbeName: "icmp",
		})
		if got := s.Bytes(); got > budget && s.Len() > 1 {
			t.Fatalf("Bytes() = %d past the %d budget with %d payloads buffered", got, budget, s.Len())
		}
	}
}

// A shed payload still has to be something the master accepts, or the sink
// trades a dropped cycle for a rejected batch.
func TestShedPayloadStillPassesIngestValidation(t *testing.T) {
	one := payloadHeapBytes(cluster.FromCycle(mtrCycle("a", 30, 10)))
	// Room for the ring and one payload's hops, so the second insert must shed
	// the first. The precondition below refuses to run if it did not.
	budget := int64(initialRingEntries)*payloadStructBytes + one + one/4
	s := quietSink(t, budget)
	s.OnCycle(context.Background(), mtrCycle("a", 30, 10))
	s.OnCycle(context.Background(), mtrCycle("b", 30, 10))

	batch := s.Drain(10)
	shed := 0
	for _, p := range batch {
		if len(p.Hops) == 0 {
			shed++
		}
	}
	if shed == 0 {
		t.Fatal("no payload was shed, so this proves nothing about a shed one")
	}
	b := cluster.CycleBatch{Source: "tokyo-1", Cycles: batch}
	if err := b.Validate(time.Now()); err != nil {
		t.Fatalf("a shed batch is refused at ingest: %v", err)
	}
}

// Requeue puts an older batch back at the head. It grows through the same gate
// OnCycle uses, so it too sheds before it drops — a count-full requeue that
// could not grow would have to drop whole cycles while hop rows were still
// sheddable.
func TestRequeueShedsBeforeItDrops(t *testing.T) {
	one := payloadHeapBytes(cluster.FromCycle(mtrCycle("a", 30, 10)))
	// Room for the ring and seven payloads' hops: six live cycles plus six
	// requeued is twelve, so the requeue must reclaim, and the slack is what
	// lets shedding alone satisfy it without a drop.
	budget := int64(initialRingEntries)*payloadStructBytes + one*7

	s := quietSink(t, budget)
	for i := range 6 {
		s.OnCycle(context.Background(), mtrCycle(string(rune('a'+i)), 30, 10))
	}
	batch := s.Drain(6)
	for _, p := range batch {
		if len(p.Hops) == 0 {
			t.Fatal("the batch to requeue was already shed, so it cannot pressure the requeue")
		}
	}
	for i := range 6 {
		s.OnCycle(context.Background(), mtrCycle(string(rune('n'+i)), 30, 10))
	}
	shedBefore := strippedCount(s)
	s.Requeue(batch)
	if strippedCount(s) == shedBefore {
		t.Fatal("Requeue's reclaim loop never ran, so nothing here exercises its ladder")
	}

	if got := s.Len(); got != 12 {
		t.Fatalf("buffered %d payloads after requeue, want 12 — a cycle was dropped while hop rows were still sheddable", got)
	}
	out := s.Drain(12)
	if out[0].Name != "a" {
		t.Fatalf("head is %q, want the requeued batch's first cycle %q", out[0].Name, "a")
	}
}

// The accounting is mutated under mu from three entry points. Run with -race.
func TestConcurrentAccountingHoldsUnderRace(t *testing.T) {
	s := quietSink(t, config.DefaultBufferBytes)
	var wg sync.WaitGroup

	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				s.OnCycle(context.Background(), mtrCycle(string(rune('a'+(w+i)%26)), 6, 3))
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 400 {
			s.Drain(7)
		}
	}()
	wg.Wait()

	for len(s.Drain(0)) > 0 {
	}
	if got := s.Bytes(); got != int64(len(s.buf))*payloadStructBytes {
		t.Fatalf("Bytes() = %d with an empty ring, want the array term %d — the accounting drifted",
			got, int64(len(s.buf))*payloadStructBytes)
	}
}

// One tick must clear a backlog. Shipping batchLimit per tick left the 30
// minutes of outage the sink now holds taking a further 14 minutes to reach
// the master at the deployed rate.
func TestOneTickClearsAWholeBacklog(t *testing.T) {
	var pushes atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pushes.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := NewRunner(slog.New(slog.DiscardHandler), &config.Config{
		Cluster: &config.Cluster{MasterURL: srv.URL, Token: "tok", Name: "tokyo-1"},
	}, "v9")
	for i := range 350 {
		r.sink.OnCycle(context.Background(), scheduler.Cycle{
			Time:   time.Now(),
			Target: config.TargetRef{Group: "core", Target: config.Target{Name: string(rune('a' + i%26))}},
		})
	}

	// pushLoop itself, not a copy of its condition in the test body. The tick
	// is 200 ms and the deadline 350 ms, so one tick can carry all four
	// batches but four ticks cannot happen: reverting the loop to one flush
	// per tick needs 800 ms and reddens here.
	r.pushEvery = 200 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.pushLoop(ctx) }()

	deadline := time.After(350 * time.Millisecond)
	for pushes.Load() < 4 {
		select {
		case <-deadline:
			t.Fatalf("issued %d pushes within one tick, want all 4 — the backlog is not draining in a single tick", pushes.Load())
		case err := <-done:
			t.Fatalf("pushLoop returned early: %v", err)
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if got := pushes.Load(); got != 4 {
		t.Fatalf("issued %d pushes for 350 cycles at a batch limit of %d, want 4", got, r.batchLimit)
	}
	if got := r.sink.Len(); got != 0 {
		t.Fatalf("%d cycles left buffered after the tick's drain", got)
	}
	cancel()
	<-done
}

// A master answering 5xx must see exactly one attempt per tick. Reporting
// progress on a requeue would turn the backlog drain into an unbounded retry
// loop against a master that is already struggling.
func TestFailingMasterGetsOneAttemptPerTick(t *testing.T) {
	var pushes atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pushes.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := NewRunner(slog.New(slog.DiscardHandler), &config.Config{
		Cluster: &config.Cluster{MasterURL: srv.URL, Token: "tok", Name: "tokyo-1"},
	}, "v9")
	for i := range 350 {
		r.sink.OnCycle(context.Background(), scheduler.Cycle{
			Time:   time.Now(),
			Target: config.TargetRef{Group: "core", Target: config.Target{Name: string(rune('a' + i%26))}},
		})
	}

	// One tick of the production loop, long enough that a hot loop would be
	// unmistakable and a second tick impossible.
	r.pushEvery = 500 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Millisecond)
	defer cancel()
	if err := r.pushLoop(ctx); err != nil {
		t.Fatalf("pushLoop: %v", err)
	}
	if got := pushes.Load(); got != 1 {
		t.Fatalf("issued %d pushes against a failing master in one tick, want 1", got)
	}
	if got := r.sink.Len(); got != 350 {
		t.Fatalf("%d cycles buffered after the failed push, want all 350 requeued", got)
	}
}

// hopBearingBelowNewest counts buffered payloads that still carry hop rows,
// excluding the most recent. Reaches into the ring because the invariant below
// has to hold at every insert, and draining to observe it would destroy the
// state being asserted. The newest is excluded because makeRoom runs before
// the insert, so the payload being admitted is not in the ring when the ladder
// decides what to shed.
func hopBearingBelowNewest(s *PushSink) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for i := 0; i < s.size-1; i++ {
		if len(s.at(i).Hops) > 0 {
			n++
		}
	}
	return n
}

func droppedCount(s *PushSink) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

// The ladder's central promise, asserted after every single insert rather than
// at the end: a cycle is never dropped while another buffered cycle still has
// hop rows to give up. stripIdx is documented as a hint, but a hint that
// drifts past the live entries turns stripOldestHops into a no-op and the sink
// starts dropping measurements it could have kept.
func TestNoCycleIsDroppedWhileHopsRemain(t *testing.T) {
	one := payloadHeapBytes(cluster.FromCycle(mtrCycle("a", 30, 10)))
	budget := int64(initialRingEntries)*payloadStructBytes + one*3

	s := quietSink(t, budget)
	prev := 0
	check := func(stage string, step int) {
		t.Helper()
		now := droppedCount(s)
		if now > prev {
			if left := hopBearingBelowNewest(s); left > 0 {
				t.Fatalf("%s step %d: dropped %d cycles while %d older buffered cycles still carry hops",
					stage, step, now-prev, left)
			}
		}
		prev = now
	}

	for i := range 120 {
		s.OnCycle(context.Background(), mtrCycle(string(rune('a'+i%26)), 30, 10))
		check("insert", i)
	}
	if prev == 0 {
		t.Fatal("no cycle was ever dropped, so this proves nothing about the drop arm")
	}

	// Drain takes from the shed head, so this batch is hopless and small; the
	// hop-bearing requeue is TestRequeueRestartsSheddingAtTheHead's case. What
	// this phase covers is that a requeue does not break the drop invariant.
	batch := s.Drain(3)
	for i := range 20 {
		s.OnCycle(context.Background(), mtrCycle(string(rune('a'+i%26)), 30, 10))
		check("refill", i)
	}
	s.Requeue(batch)
	check("requeue", 0)
}

// Requeue grows through the same gate OnCycle uses. Without that, a ring full
// by count has to drop a cycle to make a slot even when the budget has room,
// and the batch being put back is the older data the master has not seen.
func TestRequeueGrowsWhenTheRingIsFullByCount(t *testing.T) {
	s := quietSink(t, config.DefaultBufferBytes)
	small := func(i int) scheduler.Cycle {
		return scheduler.Cycle{
			Time:   time.Now(),
			Target: config.TargetRef{Group: "core", Target: config.Target{Name: fmt.Sprintf("t%d", i)}},
		}
	}
	for i := range initialRingEntries {
		s.OnCycle(context.Background(), small(i))
	}
	batch := s.Drain(initialRingEntries)
	for i := range initialRingEntries {
		s.OnCycle(context.Background(), small(1000+i))
	}
	if got := len(s.buf); got != initialRingEntries {
		t.Fatalf("ring holds %d entries, want the initial %d — this case needs a ring that is full by count", got, initialRingEntries)
	}
	s.Requeue(batch)

	if got := s.Len(); got != 2*initialRingEntries {
		t.Fatalf("buffered %d after requeueing %d into a full ring, want %d — Requeue dropped instead of growing",
			got, len(batch), 2*initialRingEntries)
	}
	// Len alone would also pass on a ring that never grew and silently
	// overwrote itself, so the contents are checked too: the requeued batch
	// first, then the cycles that arrived during the retry, none lost.
	out := s.Drain(0)
	if len(out) != 2*initialRingEntries {
		t.Fatalf("drained %d payloads, want %d", len(out), 2*initialRingEntries)
	}
	for i := range initialRingEntries {
		if want := fmt.Sprintf("t%d", i); out[i].Name != want {
			t.Fatalf("payload %d is %q, want the requeued %q — the ring overwrote itself", i, out[i].Name, want)
		}
		if want := fmt.Sprintf("t%d", 1000+i); out[initialRingEntries+i].Name != want {
			t.Fatalf("payload %d is %q, want %q", initialRingEntries+i, out[initialRingEntries+i].Name, want)
		}
	}
}

// hopNames is the set of buffered cycles that still carry hop rows.
func hopNames(s *PushSink) map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]bool{}
	for i := 0; i < s.size; i++ {
		if e := s.at(i); len(e.Hops) > 0 {
			out[e.Name] = true
		}
	}
	return out
}

func strippedCount(s *PushSink) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stripped
}

// Every prepend shifts each live entry's offset by one, so stripIdx no longer
// describes the entries it was measured against. Without the reset the walk
// resumes deep inside the ring and sheds a cycle behind the requeued batch,
// while the batch sitting at the head — the oldest data the master has not
// seen — keeps its hops.
//
// The surviving-hops set is deliberately not asserted to be a suffix: a
// requeue puts older cycles in front of an already-shed prefix, so position
// and shed order legitimately diverge. What must hold is that the next cycle
// shed is one of the requeued ones.
func TestRequeueRestartsSheddingAtTheHead(t *testing.T) {
	one := payloadHeapBytes(cluster.FromCycle(mtrCycle("a", 30, 10)))
	budget := int64(initialRingEntries)*payloadStructBytes + one*8
	s := quietSink(t, budget)

	// Six untouched hop-bearing cycles, drained before any pressure exists.
	requeued := map[string]bool{}
	for i := range 6 {
		name := string(rune('a' + i))
		requeued[name] = true
		s.OnCycle(context.Background(), mtrCycle(name, 30, 10))
	}
	batch := s.Drain(6)
	for _, p := range batch {
		if len(p.Hops) == 0 {
			t.Fatal("the batch to requeue was shed before it was drained, so it cannot exercise the reset")
		}
	}

	// Fill until shedding is running, so stripIdx sits well past the head.
	for i := range 30 {
		s.OnCycle(context.Background(), mtrCycle(string(rune('n'+i%13)), 30, 10))
	}
	if strippedCount(s) == 0 {
		t.Fatal("nothing was shed before the requeue, so stripIdx never advanced")
	}

	s.Requeue(batch)
	before := hopNames(s)
	for name := range requeued {
		if !before[name] {
			t.Fatalf("requeued cycle %q had no hops on arrival", name)
		}
	}

	// Force one more shed, whichever cycle it lands on.
	mark := strippedCount(s)
	for i := 0; strippedCount(s) == mark && i < 20; i++ {
		s.OnCycle(context.Background(), mtrCycle(string(rune('A'+i)), 30, 10))
	}
	if strippedCount(s) == mark {
		t.Fatal("no further shed happened, so nothing about the resumed walk is observable")
	}

	after := hopNames(s)
	for name := range requeued {
		if !after[name] {
			return // a requeued cycle was shed: the walk resumed at the head
		}
	}
	t.Fatalf("all %d requeued cycles kept their hops while a cycle behind them was shed — the walk did not resume at the head", len(requeued))
}

// Every path that moves a payload in or out of the ring adjusts heap, and a
// single missed adjustment is invisible until the sink has been running for
// days: an uncredited drop makes it shed cycles it had room for, an uncharged
// requeue makes it hold more than its budget. Draining to empty is the one
// assertion that covers all of them at once, so it runs after a workload that
// sheds, drops, drains and requeues.
func TestAccountingReturnsToEmptyAfterEveryPath(t *testing.T) {
	one := payloadHeapBytes(cluster.FromCycle(mtrCycle("a", 30, 10)))
	budget := int64(initialRingEntries)*payloadStructBytes + one*3
	s := quietSink(t, budget)
	empty := s.Bytes()

	for i := range 150 {
		s.OnCycle(context.Background(), mtrCycle(string(rune('a'+i%26)), 30, 10))
	}
	if droppedCount(s) == 0 || strippedCount(s) == 0 {
		t.Fatalf("workload shed %d and dropped %d — it must exercise both arms",
			strippedCount(s), droppedCount(s))
	}
	batch := s.Drain(2)
	for i := range 10 {
		s.OnCycle(context.Background(), mtrCycle(string(rune('n'+i%13)), 30, 10))
	}
	s.Requeue(batch)
	for i := range 10 {
		s.OnCycle(context.Background(), mtrCycle(string(rune('A'+i)), 30, 10))
	}

	for len(s.Drain(16)) > 0 {
	}
	if got := s.Bytes(); got != empty {
		t.Fatalf("Bytes() = %d with an empty ring, want %d — the accounting drifted by %d",
			got, empty, got-empty)
	}
}

// Requeue on an over-budget sink must be a no-op for an empty batch. Without
// its length guard it runs the reclaim loop against a zero-byte request, which
// on a sink already past its budget evicts every buffered cycle.
func TestRequeueOfNothingKeepsTheBuffer(t *testing.T) {
	// The lone-payload escape leaves this sink deliberately over budget.
	s := quietSink(t, payloadStructBytes)
	s.OnCycle(context.Background(), mtrCycle("a", 30, 10))
	if s.Bytes() <= s.Budget() {
		t.Fatalf("Bytes() = %d is inside the %d budget, so this proves nothing", s.Bytes(), s.Budget())
	}

	s.Requeue(nil)
	if got := s.Len(); got != 1 {
		t.Fatalf("Requeue(nil) left %d payloads buffered, want 1", got)
	}
	s.Requeue([]cluster.CyclePayload{})
	if got := s.Len(); got != 1 {
		t.Fatalf("Requeue of an empty slice left %d payloads buffered, want 1", got)
	}
}

// The counter names what the operator loses. Counting a cycle that had no hop
// rows to give up reports path history shed that never existed.
func TestStrippedCountsOnlyCyclesThatHadHops(t *testing.T) {
	one := payloadHeapBytes(cluster.FromCycle(mtrCycle("a", 30, 10)))
	budget := int64(initialRingEntries)*payloadStructBytes + one*3
	s := quietSink(t, budget)

	const cycles = 30
	for i := range cycles {
		s.OnCycle(context.Background(), mtrCycle(string(rune('a'+i%26)), 30, 10))
	}
	// Requeue resets the walk to the head, where the already-shed cycles sit.
	// Those are the entries the skip guard has to walk over rather than count.
	s.Requeue(s.Drain(2))
	for i := range 10 {
		s.OnCycle(context.Background(), mtrCycle(string(rune('n'+i%13)), 30, 10))
	}
	shed := strippedCount(s)
	if shed == 0 {
		t.Fatal("nothing was shed")
	}
	hopless := 0
	for _, p := range s.Drain(0) {
		if len(p.Hops) == 0 {
			hopless++
		}
	}
	if shed != hopless+droppedCount(s) {
		t.Fatalf("stripped=%d but only %d buffered cycles are hopless and %d were dropped — the counter includes cycles that had no hops",
			shed, hopless, droppedCount(s))
	}
}

// The hop terms are the dominant part of the accounting and the reason the
// budget exists, so one assertion computes them independently rather than
// through payloadHeapBytes. Every other test derives its expected bytes from
// the function under test, which holds under any symmetric mis-accounting:
// deleting the address or the RTT term left the whole suite green.
func TestHopAccountingMatchesIndependentArithmetic(t *testing.T) {
	const (
		rows = 7
		addr = "2001:db8:85a3:1:8a2e:370:7334:9"
		rtts = 3
	)
	s := quietSink(t, config.DefaultBufferBytes)
	empty := s.Bytes()

	hops := make([]probe.Hop, rows)
	for i := range hops {
		hops[i] = probe.Hop{Index: i + 1, IP: addr, Unreach: "admin", RTTs: make([]time.Duration, rtts)}
	}
	s.OnCycle(context.Background(), scheduler.Cycle{
		Time:      time.Now(),
		Target:    config.TargetRef{Group: "core", Target: config.Target{Name: "gw"}},
		ProbeName: "mtr",
		Hops:      hops,
	})

	want := int64(len("core")+len("gw")+len("mtr")) +
		rows*hopStructBytes +
		rows*int64(len(addr)) +
		rows*int64(len("admin")) +
		rows*rtts*durationBytes
	if got := s.Bytes() - empty; got != want {
		t.Fatalf("Bytes() rose by %d, want %d — %d rows of a %d-byte address, a %d-byte unreach label and %d samples each",
			got, want, rows, len(addr), len("admin"), rtts)
	}
}

// The ring only ever grew, and shedding is what drives that growth, so one
// hopless backlog left most of the budget spent on empty slots for the life of
// the process and every later outage kept less path history than the first.
func TestRingGivesCapacityBackAfterABacklogDrains(t *testing.T) {
	const budget = 8 << 20
	hopless := func(i int) scheduler.Cycle {
		return scheduler.Cycle{
			Time:   time.Now(),
			Target: config.TargetRef{Group: "core", Target: config.Target{Name: fmt.Sprintf("t%d", i)}},
		}
	}
	mtr := func(i int) scheduler.Cycle { return mtrCycle(string(rune('a'+i%26)), 30, 10) }

	fresh := quietSink(t, budget)
	for i := range 600 {
		fresh.OnCycle(context.Background(), mtr(i))
	}
	freshHops := 0
	for _, p := range fresh.Drain(0) {
		if len(p.Hops) > 0 {
			freshHops++
		}
	}

	aged := quietSink(t, budget)
	for i := range 20000 {
		aged.OnCycle(context.Background(), hopless(i))
	}
	grown := len(aged.buf)
	for len(aged.Drain(512)) > 0 {
	}
	if grown <= initialRingEntries {
		t.Fatalf("the hopless backlog grew the ring to %d entries, so there is nothing to give back", grown)
	}
	if got := len(aged.buf); got >= grown {
		t.Fatalf("ring holds %d entries after draining to empty, grew to %d — capacity was never returned", got, grown)
	}
	for i := range 600 {
		aged.OnCycle(context.Background(), mtr(i))
	}
	agedHops := 0
	for _, p := range aged.Drain(0) {
		if len(p.Hops) > 0 {
			agedHops++
		}
	}
	if agedHops != freshHops {
		t.Fatalf("a sink that had drained a hopless backlog kept hops on %d cycles against a fresh sink's %d — the ring ratcheted the budget down",
			agedHops, freshHops)
	}
}

// makeRoom sheds and drops in a loop, so the counters step over a multiple of
// shedLogEvery rather than landing on one. An exact-modulus test skipped most
// of its own lines, and these are the slave's only signal that measurements
// are being dropped.
func TestEveryShedThresholdIsReported(t *testing.T) {
	var buf strings.Builder
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	one := payloadHeapBytes(cluster.FromCycle(mtrCycle("a", 30, 10)))
	s := NewPushSink(log, int64(initialRingEntries)*payloadStructBytes+one*2)

	// Alternating shapes make one admission shed or drop several cycles, which
	// is the stride an exact-modulus check steps over.
	for i := range 4000 {
		if i%3 == 0 {
			s.OnCycle(context.Background(), mtrCycle(string(rune('a'+i%26)), 30, 10))
			continue
		}
		s.OnCycle(context.Background(), scheduler.Cycle{
			Time:   time.Now(),
			Target: config.TargetRef{Group: "core", Target: config.Target{Name: "t"}},
		})
	}

	lines := strings.Count(buf.String(), "push buffer")
	want := strippedCount(s)/shedLogEvery + droppedCount(s)/shedLogEvery
	if droppedCount(s) == 0 || strippedCount(s) == 0 {
		t.Fatalf("workload shed %d and dropped %d — it must exercise both counters", strippedCount(s), droppedCount(s))
	}
	if lines != want {
		t.Fatalf("emitted %d warnings for %d shed and %d dropped cycles, want %d",
			lines, strippedCount(s), droppedCount(s), want)
	}
}
