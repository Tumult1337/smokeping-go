package storage

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
)

type fakeReader struct {
	cycles       atomic.Int64
	hopsTimeline atomic.Int64
	hopsAt       atomic.Int64
	latestHops   atomic.Int64
	out          []CyclePoint
	hops         []HopPoint
	cycleLoss    []CycleCounters
	err          error
}

func (f *fakeReader) QueryCycles(ctx context.Context, ref config.TargetRef, from, to time.Time, q QueryFilter) ([]CyclePoint, error) {
	f.cycles.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return f.out, nil
}
func (f *fakeReader) QueryRTTs(context.Context, config.TargetRef, time.Time, time.Time, QueryFilter) ([]RTTPoint, error) {
	return nil, nil
}
func (f *fakeReader) QueryHTTPSamples(context.Context, config.TargetRef, time.Time, time.Time, QueryFilter) ([]HTTPPoint, error) {
	return nil, nil
}
func (f *fakeReader) QueryLatestHops(context.Context, config.TargetRef, QueryFilter) (HopsResult, error) {
	f.latestHops.Add(1)
	if f.err != nil {
		return HopsResult{}, f.err
	}
	return HopsResult{Hops: f.hops, Cycles: f.cycleLoss}, nil
}
func (f *fakeReader) QueryHopsAt(context.Context, config.TargetRef, time.Time, time.Duration, QueryFilter) (HopsResult, error) {
	f.hopsAt.Add(1)
	if f.err != nil {
		return HopsResult{}, f.err
	}
	return HopsResult{Hops: f.hops, Cycles: f.cycleLoss}, nil
}
func (f *fakeReader) QueryHopsTimeline(context.Context, config.TargetRef, time.Time, time.Time, QueryFilter) (HopsResult, error) {
	f.hopsTimeline.Add(1)
	if f.err != nil {
		return HopsResult{}, f.err
	}
	return HopsResult{Hops: f.hops}, nil
}
func (f *fakeReader) QueryOverview(context.Context, time.Time, time.Time, []config.TargetRef) ([]OverviewSourceRow, error) {
	return nil, nil
}

func newRef(group, name string) config.TargetRef {
	return config.TargetRef{Group: group, Target: config.Target{Name: name}}
}

// slowFakeReader blocks every call on `gate` until the test releases it,
// letting a test fan multiple goroutines into the same in-flight slot before
// any of them complete. Used by the singleflight tests for both cycles and
// hops. The same `calls` counter covers either path; tests use one or the
// other, not both.
type slowFakeReader struct {
	gate     chan struct{}
	calls    atomic.Int64
	hops     []HopPoint
	cyclePts []CyclePoint
}

func (s *slowFakeReader) QueryCycles(context.Context, config.TargetRef, time.Time, time.Time, QueryFilter) ([]CyclePoint, error) {
	s.calls.Add(1)
	<-s.gate
	return s.cyclePts, nil
}
func (s *slowFakeReader) QueryRTTs(context.Context, config.TargetRef, time.Time, time.Time, QueryFilter) ([]RTTPoint, error) {
	return nil, nil
}
func (s *slowFakeReader) QueryHTTPSamples(context.Context, config.TargetRef, time.Time, time.Time, QueryFilter) ([]HTTPPoint, error) {
	return nil, nil
}
func (s *slowFakeReader) QueryLatestHops(context.Context, config.TargetRef, QueryFilter) (HopsResult, error) {
	s.calls.Add(1)
	<-s.gate
	return HopsResult{Hops: s.hops}, nil
}
func (s *slowFakeReader) QueryHopsAt(context.Context, config.TargetRef, time.Time, time.Duration, QueryFilter) (HopsResult, error) {
	s.calls.Add(1)
	<-s.gate
	return HopsResult{Hops: s.hops}, nil
}
func (s *slowFakeReader) QueryHopsTimeline(context.Context, config.TargetRef, time.Time, time.Time, QueryFilter) (HopsResult, error) {
	s.calls.Add(1)
	<-s.gate
	return HopsResult{Hops: s.hops}, nil
}
func (s *slowFakeReader) QueryOverview(context.Context, time.Time, time.Time, []config.TargetRef) ([]OverviewSourceRow, error) {
	return nil, nil
}

func TestCachingReader_HitsCacheWithinTTL(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	clock := now
	inner := &fakeReader{out: []CyclePoint{{Time: now, Median: 1.5}}}
	c := NewCachingReader(inner, 8, 8)
	c.nowFn = func() time.Time { return clock }

	ref := newRef("g", "t")
	from := now.Add(-7 * 24 * time.Hour)
	to := now

	if _, err := c.QueryCycles(context.Background(), ref, from, to, QueryFilter{Step: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.QueryCycles(context.Background(), ref, from, to, QueryFilter{Step: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if got := inner.cycles.Load(); got != 1 {
		t.Fatalf("inner calls: got %d want 1", got)
	}
}

func TestCachingReader_RefetchesAfterTTL(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	clock := now
	inner := &fakeReader{out: []CyclePoint{{Time: now, Median: 1.5}}}
	c := NewCachingReader(inner, 8, 8)
	c.nowFn = func() time.Time { return clock }

	ref := newRef("g", "t")
	from := now.Add(-7 * 24 * time.Hour)
	to := now

	if _, err := c.QueryCycles(context.Background(), ref, from, to, QueryFilter{Step: time.Hour}); err != nil {
		t.Fatal(err)
	}
	clock = now.Add(cacheTTLLive + time.Second)
	if _, err := c.QueryCycles(context.Background(), ref, from, to, QueryFilter{Step: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if got := inner.cycles.Load(); got != 2 {
		t.Fatalf("inner calls: got %d want 2", got)
	}
}

func TestCachingReader_HistoricalGetsLongerTTL(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	clock := now
	inner := &fakeReader{out: []CyclePoint{{Time: now}}}
	c := NewCachingReader(inner, 8, 8)
	c.nowFn = func() time.Time { return clock }

	ref := newRef("g", "t")
	to := now.Add(-7 * 24 * time.Hour)
	from := to.Add(-24 * time.Hour)

	if _, err := c.QueryCycles(context.Background(), ref, from, to, QueryFilter{Step: time.Hour}); err != nil {
		t.Fatal(err)
	}
	clock = now.Add(2 * time.Minute)
	if _, err := c.QueryCycles(context.Background(), ref, from, to, QueryFilter{Step: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if got := inner.cycles.Load(); got != 1 {
		t.Fatalf("inner calls: got %d want 1 (still within historical TTL)", got)
	}
}

func TestCachingReader_QuantizesKey(t *testing.T) {
	// 12:00:01 and 12:00:14 both ceil to 12:00:30 with a 30s `to` quantum;
	// 12:01:00-7d and 12:01:13-7d both floor to the same 5m boundary on `from`.
	// So two refreshes 13s apart with slightly different `from`/`to` should
	// share a cache entry.
	now1 := time.Date(2026, 4, 27, 12, 0, 1, 0, time.UTC)
	now2 := time.Date(2026, 4, 27, 12, 0, 14, 0, time.UTC)
	clock := now1
	inner := &fakeReader{out: []CyclePoint{{Time: now1}}}
	c := NewCachingReader(inner, 8, 8)
	c.nowFn = func() time.Time { return clock }

	ref := newRef("g", "t")
	if _, err := c.QueryCycles(context.Background(), ref, now1.Add(-7*24*time.Hour), now1, QueryFilter{Step: time.Hour}); err != nil {
		t.Fatal(err)
	}
	clock = now2
	if _, err := c.QueryCycles(context.Background(), ref, now2.Add(-7*24*time.Hour), now2, QueryFilter{Step: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if got := inner.cycles.Load(); got != 1 {
		t.Fatalf("inner calls: got %d want 1 (drift within quantum should reuse entry)", got)
	}
}

func TestCachingReader_StepDoesNotCollide(t *testing.T) {
	// The API's `?step=` override makes Step independent of the window: the same
	// from/to can be requested at different bucket widths. Those must NOT share
	// a cache entry — the payload shape differs. Two identical-window queries
	// with different Step should each hit the inner reader.
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	inner := &fakeReader{out: []CyclePoint{{Time: now}}}
	c := NewCachingReader(inner, 8, 8)
	c.nowFn = func() time.Time { return now }

	ref := newRef("g", "t")
	from := now.Add(-7 * 24 * time.Hour)
	if _, err := c.QueryCycles(context.Background(), ref, from, now, QueryFilter{Step: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.QueryCycles(context.Background(), ref, from, now, QueryFilter{Step: 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	if got := inner.cycles.Load(); got != 2 {
		t.Fatalf("inner calls: got %d want 2 (different Step must not collide on one cache entry)", got)
	}
}

func TestCachingReader_DifferentSourcesAreSeparate(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	inner := &fakeReader{out: []CyclePoint{{Time: now}}}
	c := NewCachingReader(inner, 8, 8)
	c.nowFn = func() time.Time { return now }

	ref := newRef("g", "t")
	from := now.Add(-7 * 24 * time.Hour)

	if _, err := c.QueryCycles(context.Background(), ref, from, now, QueryFilter{Source: "master", Step: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.QueryCycles(context.Background(), ref, from, now, QueryFilter{Source: "slave-a", Step: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if got := inner.cycles.Load(); got != 2 {
		t.Fatalf("inner calls: got %d want 2 (sources differ)", got)
	}
}

func TestCachingReader_LRUEvicts(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	inner := &fakeReader{out: []CyclePoint{{Time: now}}}
	c := NewCachingReader(inner, 2, 2)
	c.nowFn = func() time.Time { return now }

	from := now.Add(-7 * 24 * time.Hour)
	for _, name := range []string{"a", "b", "c"} {
		if _, err := c.QueryCycles(context.Background(), newRef("g", name), from, now, QueryFilter{Step: time.Hour}); err != nil {
			t.Fatal(err)
		}
	}
	// Re-query "a" — it was evicted when "c" was inserted, so this should miss.
	if _, err := c.QueryCycles(context.Background(), newRef("g", "a"), from, now, QueryFilter{Step: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if got := inner.cycles.Load(); got != 4 {
		t.Fatalf("inner calls: got %d want 4 (3 inserts + 1 re-query of evicted)", got)
	}
}

func TestCachingReader_ErrorBypassesCache(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	wantErr := errors.New("boom")
	inner := &fakeReader{err: wantErr}
	c := NewCachingReader(inner, 8, 8)
	c.nowFn = func() time.Time { return now }

	ref := newRef("g", "t")
	from := now.Add(-7 * 24 * time.Hour)

	for i := range 3 {
		_, err := c.QueryCycles(context.Background(), ref, from, now, QueryFilter{Step: time.Hour})
		if !errors.Is(err, wantErr) {
			t.Fatalf("call %d: got err %v want %v", i, err, wantErr)
		}
	}
	if got := inner.cycles.Load(); got != 3 {
		t.Fatalf("inner calls: got %d want 3 (errors must not be cached)", got)
	}
}

func TestCachingReader_HopsTimeline_HitsCacheWithinTTL(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	clock := now
	inner := &fakeReader{hops: []HopPoint{{Time: now, Index: 1, IP: "1.1.1.1"}}}
	c := NewCachingReader(inner, 8, 8)
	c.nowFn = func() time.Time { return clock }

	ref := newRef("g", "t")
	from := now.Add(-7 * 24 * time.Hour)
	to := now

	if _, err := c.QueryHopsTimeline(context.Background(), ref, from, to, QueryFilter{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.QueryHopsTimeline(context.Background(), ref, from, to, QueryFilter{}); err != nil {
		t.Fatal(err)
	}
	if got := inner.hopsTimeline.Load(); got != 1 {
		t.Fatalf("inner calls: got %d want 1", got)
	}
}

func TestCachingReader_HopsTimeline_RefetchesAfterTTL(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	clock := now
	inner := &fakeReader{hops: []HopPoint{{Time: now, Index: 1}}}
	c := NewCachingReader(inner, 8, 8)
	c.nowFn = func() time.Time { return clock }

	ref := newRef("g", "t")
	from := now.Add(-7 * 24 * time.Hour)

	if _, err := c.QueryHopsTimeline(context.Background(), ref, from, now, QueryFilter{}); err != nil {
		t.Fatal(err)
	}
	clock = now.Add(cacheTTLLive + time.Second)
	if _, err := c.QueryHopsTimeline(context.Background(), ref, from, now, QueryFilter{}); err != nil {
		t.Fatal(err)
	}
	if got := inner.hopsTimeline.Load(); got != 2 {
		t.Fatalf("inner calls: got %d want 2", got)
	}
}

func TestCachingReader_HopsTimeline_HistoricalGetsLongerTTL(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	clock := now
	inner := &fakeReader{hops: []HopPoint{{Time: now, Index: 1}}}
	c := NewCachingReader(inner, 8, 8)
	c.nowFn = func() time.Time { return clock }

	ref := newRef("g", "t")
	to := now.Add(-7 * 24 * time.Hour)
	from := to.Add(-24 * time.Hour)

	if _, err := c.QueryHopsTimeline(context.Background(), ref, from, to, QueryFilter{}); err != nil {
		t.Fatal(err)
	}
	clock = now.Add(2 * time.Minute)
	if _, err := c.QueryHopsTimeline(context.Background(), ref, from, to, QueryFilter{}); err != nil {
		t.Fatal(err)
	}
	if got := inner.hopsTimeline.Load(); got != 1 {
		t.Fatalf("inner calls: got %d want 1 (still within historical TTL)", got)
	}
}

func TestCachingReader_HopsTimeline_QuantizesKey(t *testing.T) {
	now1 := time.Date(2026, 4, 27, 12, 0, 1, 0, time.UTC)
	now2 := time.Date(2026, 4, 27, 12, 0, 14, 0, time.UTC)
	clock := now1
	inner := &fakeReader{hops: []HopPoint{{Time: now1, Index: 1}}}
	c := NewCachingReader(inner, 8, 8)
	c.nowFn = func() time.Time { return clock }

	ref := newRef("g", "t")
	if _, err := c.QueryHopsTimeline(context.Background(), ref, now1.Add(-7*24*time.Hour), now1, QueryFilter{}); err != nil {
		t.Fatal(err)
	}
	clock = now2
	if _, err := c.QueryHopsTimeline(context.Background(), ref, now2.Add(-7*24*time.Hour), now2, QueryFilter{}); err != nil {
		t.Fatal(err)
	}
	if got := inner.hopsTimeline.Load(); got != 1 {
		t.Fatalf("inner calls: got %d want 1 (drift within quantum should reuse entry)", got)
	}
}

func TestCachingReader_HopsTimeline_DifferentSourcesAreSeparate(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	inner := &fakeReader{hops: []HopPoint{{Time: now, Index: 1}}}
	c := NewCachingReader(inner, 8, 8)
	c.nowFn = func() time.Time { return now }

	ref := newRef("g", "t")
	from := now.Add(-7 * 24 * time.Hour)

	if _, err := c.QueryHopsTimeline(context.Background(), ref, from, now, QueryFilter{Source: "master"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.QueryHopsTimeline(context.Background(), ref, from, now, QueryFilter{Source: "slave-a"}); err != nil {
		t.Fatal(err)
	}
	if got := inner.hopsTimeline.Load(); got != 2 {
		t.Fatalf("inner calls: got %d want 2 (sources differ)", got)
	}
}

func TestCachingReader_HopsTimeline_LRUEvicts(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	inner := &fakeReader{hops: []HopPoint{{Time: now, Index: 1}}}
	// Force a very small hops cap so eviction triggers after 2 inserts.
	c := NewCachingReader(inner, 2, 2)
	c.nowFn = func() time.Time { return now }

	from := now.Add(-7 * 24 * time.Hour)
	for _, name := range []string{"a", "b", "c"} {
		if _, err := c.QueryHopsTimeline(context.Background(), newRef("g", name), from, now, QueryFilter{}); err != nil {
			t.Fatal(err)
		}
	}
	// "a" should have been evicted when "c" was inserted.
	if _, err := c.QueryHopsTimeline(context.Background(), newRef("g", "a"), from, now, QueryFilter{}); err != nil {
		t.Fatal(err)
	}
	if got := inner.hopsTimeline.Load(); got != 4 {
		t.Fatalf("inner calls: got %d want 4 (3 inserts + 1 re-query of evicted)", got)
	}
}

func TestCachingReader_HopsTimeline_ErrorBypassesCache(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	wantErr := errors.New("boom")
	inner := &fakeReader{err: wantErr}
	c := NewCachingReader(inner, 8, 8)
	c.nowFn = func() time.Time { return now }

	ref := newRef("g", "t")
	from := now.Add(-7 * 24 * time.Hour)

	for i := range 3 {
		_, err := c.QueryHopsTimeline(context.Background(), ref, from, now, QueryFilter{})
		if !errors.Is(err, wantErr) {
			t.Fatalf("call %d: got err %v want %v", i, err, wantErr)
		}
	}
	if got := inner.hopsTimeline.Load(); got != 3 {
		t.Fatalf("inner calls: got %d want 3 (errors must not be cached)", got)
	}
}

func TestCachingReader_HopsAt_HitsCacheWithinTTL(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	clock := now
	inner := &fakeReader{hops: []HopPoint{{Time: now, Index: 1}}}
	c := NewCachingReader(inner, 8, 8)
	c.nowFn = func() time.Time { return clock }

	ref := newRef("g", "t")
	at := now.Add(-time.Hour)

	if _, err := c.QueryHopsAt(context.Background(), ref, at, 30*time.Minute, QueryFilter{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.QueryHopsAt(context.Background(), ref, at, 30*time.Minute, QueryFilter{}); err != nil {
		t.Fatal(err)
	}
	if got := inner.hopsAt.Load(); got != 1 {
		t.Fatalf("inner calls: got %d want 1", got)
	}
}

func TestCachingReader_LatestHops_HitsCacheWithinTTL(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	clock := now
	inner := &fakeReader{hops: []HopPoint{{Time: now, Index: 1}}}
	c := NewCachingReader(inner, 8, 8)
	c.nowFn = func() time.Time { return clock }

	ref := newRef("g", "t")

	if _, err := c.QueryLatestHops(context.Background(), ref, QueryFilter{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.QueryLatestHops(context.Background(), ref, QueryFilter{}); err != nil {
		t.Fatal(err)
	}
	if got := inner.latestHops.Load(); got != 1 {
		t.Fatalf("inner calls: got %d want 1", got)
	}
}

func TestCachingReader_Cycles_SingleflightsConcurrentMisses(t *testing.T) {
	// Mirror of the hops singleflight test for cycles. A React mount + range
	// click + auto-refresh tick can fire 3 identical cold-key cycles queries
	// at once; the singleflight should collapse them into one inner call.
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	gate := make(chan struct{})
	inner := &slowFakeReader{gate: gate, cyclePts: []CyclePoint{{Time: now, Median: 1}}}
	c := NewCachingReader(inner, 8, 8)
	c.nowFn = func() time.Time { return now }

	ref := newRef("g", "t")
	from := now.Add(-7 * 24 * time.Hour)

	const N = 8
	errs := make(chan error, N)
	for range N {
		go func() {
			_, err := c.QueryCycles(context.Background(), ref, from, now, QueryFilter{Step: time.Hour})
			errs <- err
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(gate)
	for range N {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if got := inner.calls.Load(); got != 1 {
		t.Fatalf("inner calls: got %d want 1 (singleflight should dedupe concurrent misses)", got)
	}
}

func TestCachingReader_HopsTimeline_SingleflightsConcurrentMisses(t *testing.T) {
	// 8 goroutines hit the same cold key in parallel. A naive cache fires 8
	// inner queries; with singleflight, exactly one runs and the rest wait
	// for its result. A 7d hops query against ClickHouse scans millions of
	// rows, so collapsing the stampede matters.
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	gate := make(chan struct{})
	inner := &slowFakeReader{gate: gate, hops: []HopPoint{{Time: now, Index: 1}}}
	c := NewCachingReader(inner, 8, 8)
	c.nowFn = func() time.Time { return now }

	ref := newRef("g", "t")
	from := now.Add(-7 * 24 * time.Hour)

	const N = 8
	errs := make(chan error, N)
	for range N {
		go func() {
			_, err := c.QueryHopsTimeline(context.Background(), ref, from, now, QueryFilter{})
			errs <- err
		}()
	}
	// Give all goroutines time to enter the cache.
	time.Sleep(50 * time.Millisecond)
	// Release the inner reader.
	close(gate)
	for range N {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if got := inner.calls.Load(); got != 1 {
		t.Fatalf("inner calls: got %d want 1 (singleflight should dedupe concurrent misses)", got)
	}
}

// TestCachingReader_HopsTimeline_LeaderCancellationDoesNotPoisonWaiters
// pins down the contract that pre-fix, the leader's caller cancellation
// (browser nav, server WriteTimeout, AbortController fire) propagated
// ctx.Canceled to every concurrent waiter and discarded the in-flight
// result. With context.WithoutCancel-decoupling the leader's run survives,
// the entry lands in cache, and a request that arrives after the leader
// gave up serves from the warm entry instead of restarting the slow query.
func TestCachingReader_HopsTimeline_LeaderCancellationDoesNotPoisonWaiters(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	gate := make(chan struct{})
	inner := &slowFakeReader{gate: gate, hops: []HopPoint{{Time: now, Index: 1}}}
	c := NewCachingReader(inner, 8, 8)
	c.nowFn = func() time.Time { return now }

	ref := newRef("g", "t")
	from := now.Add(-7 * 24 * time.Hour)

	// Leader caller cancels its ctx mid-flight — simulates the UI navigating
	// away or the server hitting WriteTimeout while the Flux query is still
	// running.
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderErr := make(chan error, 1)
	go func() {
		_, err := c.QueryHopsTimeline(leaderCtx, ref, from, now, QueryFilter{})
		leaderErr <- err
	}()
	// Let the leader register the in-flight slot before cancelling it.
	time.Sleep(20 * time.Millisecond)
	cancelLeader()
	if err := <-leaderErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader err: got %v want context.Canceled", err)
	}

	// Inner query is still blocked on `gate`. Release it now so the detached
	// goroutine completes, stores the entry, and signals.
	close(gate)

	// A subsequent request should serve from the cache without firing a
	// second inner call. Poll briefly because the goroutine completes
	// asynchronously after `gate` closes.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if inner.calls.Load() == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := c.QueryHopsTimeline(context.Background(), ref, from, now, QueryFilter{}); err != nil {
		t.Fatalf("post-cancel fetch: %v", err)
	}
	if got := inner.calls.Load(); got != 1 {
		t.Fatalf("inner calls: got %d want 1 (leader cancellation must not discard the in-flight result)", got)
	}
}

func TestCachingReader_LatestHops_RefetchesAfterTTL(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	clock := now
	inner := &fakeReader{hops: []HopPoint{{Time: now, Index: 1}}}
	c := NewCachingReader(inner, 8, 8)
	c.nowFn = func() time.Time { return clock }

	ref := newRef("g", "t")
	if _, err := c.QueryLatestHops(context.Background(), ref, QueryFilter{}); err != nil {
		t.Fatal(err)
	}
	clock = now.Add(cacheTTLLive + time.Second)
	if _, err := c.QueryLatestHops(context.Background(), ref, QueryFilter{}); err != nil {
		t.Fatal(err)
	}
	if got := inner.latestHops.Load(); got != 2 {
		t.Fatalf("inner calls: got %d want 2 (TTL expired)", got)
	}
}

func TestCachingReader_IndependentCapsForCyclesAndHops(t *testing.T) {
	// hops timeline entries can be ~100MB each at 7d resolution while cycles
	// entries are ~hundreds of KB. The constructor accepts independent caps
	// so the operator can keep many cycle entries cached while bounding hops
	// memory. This test pins the contract: cyclesMax=2 evicts cycles after
	// 3 inserts; hopsMax=8 keeps all 3 hops timelines warm.
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	inner := &fakeReader{
		out:  []CyclePoint{{Time: now}},
		hops: []HopPoint{{Time: now, Index: 1}},
	}
	c := NewCachingReader(inner, 2, 8)
	c.nowFn = func() time.Time { return now }

	from := now.Add(-7 * 24 * time.Hour)
	for _, name := range []string{"a", "b", "c"} {
		ref := newRef("g", name)
		if _, err := c.QueryCycles(context.Background(), ref, from, now, QueryFilter{Step: time.Hour}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.QueryHopsTimeline(context.Background(), ref, from, now, QueryFilter{}); err != nil {
			t.Fatal(err)
		}
	}

	// cyclesMax=2: re-querying "a" must miss (evicted by "c").
	if _, err := c.QueryCycles(context.Background(), newRef("g", "a"), from, now, QueryFilter{Step: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if got := inner.cycles.Load(); got != 4 {
		t.Fatalf("cycles inner calls: got %d want 4 (cap=2 evicted 'a')", got)
	}

	// hopsMax=8: re-querying "a" must hit (still warm).
	if _, err := c.QueryHopsTimeline(context.Background(), newRef("g", "a"), from, now, QueryFilter{}); err != nil {
		t.Fatal(err)
	}
	if got := inner.hopsTimeline.Load(); got != 3 {
		t.Fatalf("hops inner calls: got %d want 3 (cap=8 kept 'a' warm)", got)
	}
}

func TestCachingReader_Stats_TracksCyclesHitsAndMisses(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	inner := &fakeReader{out: []CyclePoint{{Time: now, Median: 1.5}}}
	c := NewCachingReader(inner, 8, 8)
	c.nowFn = func() time.Time { return now }

	ref := newRef("g", "t")
	from := now.Add(-7 * 24 * time.Hour)

	// First call: miss → fills the cache.
	if _, err := c.QueryCycles(context.Background(), ref, from, now, QueryFilter{Step: time.Hour}); err != nil {
		t.Fatal(err)
	}
	// Second call within TTL: hit.
	if _, err := c.QueryCycles(context.Background(), ref, from, now, QueryFilter{Step: time.Hour}); err != nil {
		t.Fatal(err)
	}
	stats := c.Stats()
	if stats.CyclesHits != 1 || stats.CyclesMisses != 1 {
		t.Fatalf("cycles stats: got hits=%d misses=%d, want hits=1 misses=1", stats.CyclesHits, stats.CyclesMisses)
	}
}

func TestCachingReader_Stats_ErrorsCountAsMissNotHit(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	inner := &fakeReader{err: errors.New("boom")}
	c := NewCachingReader(inner, 8, 8)
	c.nowFn = func() time.Time { return now }

	ref := newRef("g", "t")
	from := now.Add(-7 * 24 * time.Hour)

	for range 3 {
		_, _ = c.QueryCycles(context.Background(), ref, from, now, QueryFilter{Step: time.Hour})
	}
	stats := c.Stats()
	if stats.CyclesHits != 0 || stats.CyclesMisses != 3 {
		t.Fatalf("cycles stats on error: got hits=%d misses=%d, want hits=0 misses=3", stats.CyclesHits, stats.CyclesMisses)
	}
}

func TestCachingReader_Cycles_NoRedundantLeaderAfterRace(t *testing.T) {
	// Race: caller A's hopsLookup misses (leader B hasn't stored yet); leader
	// B then stores its result and removes its inflight slot; caller A reaches
	// the inflight check and finds no slot, so without re-checking the cache
	// it becomes a redundant leader and fires a duplicate inner query.
	//
	// The test simulates the race deterministically by having a hook fire
	// between lookup and the inflight check, where the hook stores the entry
	// (the moral equivalent of a leader having just completed). With the fix,
	// the re-check under the inflight lock returns the entry; without it,
	// inner.cycles increments and we'd see >0.
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	inner := &fakeReader{out: []CyclePoint{{Time: now, Median: 99}}}
	c := NewCachingReader(inner, 8, 8)
	c.nowFn = func() time.Time { return now }

	ref := newRef("g", "t")
	from := now.Add(-7 * 24 * time.Hour)

	key := cycleCacheKey{
		group:    "g",
		name:     "t",
		fromUnix: floorUnix(from, cacheKeyFromQuantum),
		toUnix:   ceilUnix(now, cacheKeyToQuantum),
		stepSec:  int64(time.Hour / time.Second),
	}
	c.testHookAfterCyclesLookup = func() {
		c.testHookAfterCyclesLookup = nil
		c.store(key, []CyclePoint{{Time: now, Median: 42}}, time.Hour)
	}

	pts, err := c.QueryCycles(context.Background(), ref, from, now, QueryFilter{Step: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if got := inner.cycles.Load(); got != 0 {
		t.Fatalf("inner cycles calls: got %d want 0 (re-check under inflight lock should serve from cache)", got)
	}
	if len(pts) != 1 || pts[0].Median != 42 {
		t.Fatalf("expected entry stored mid-flight, got %+v", pts)
	}
}

func TestCachingReader_Hops_NoRedundantLeaderAfterRace(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	inner := &fakeReader{hops: []HopPoint{{Time: now, Index: 99}}}
	c := NewCachingReader(inner, 8, 8)
	c.nowFn = func() time.Time { return now }

	ref := newRef("g", "t")
	from := now.Add(-7 * 24 * time.Hour)

	key := hopsCacheKey{
		kind:     hopsKindTimeline,
		group:    "g",
		name:     "t",
		fromUnix: floorUnix(from, cacheKeyFromQuantum),
		toUnix:   ceilUnix(now, cacheKeyToQuantum),
	}
	c.testHookAfterHopsLookup = func() {
		c.testHookAfterHopsLookup = nil
		c.hopsStore(key, HopsResult{Hops: []HopPoint{{Time: now, Index: 42}}}, time.Hour)
	}

	pts, err := c.QueryHopsTimeline(context.Background(), ref, from, now, QueryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if got := inner.hopsTimeline.Load(); got != 0 {
		t.Fatalf("inner hops calls: got %d want 0 (re-check under inflight lock should serve from cache)", got)
	}
	if len(pts.Hops) != 1 || pts.Hops[0].Index != 42 {
		t.Fatalf("expected entry stored mid-flight, got %+v", pts)
	}
}

func TestCachingReader_Stats_TracksHopsHitsAndMisses(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	inner := &fakeReader{hops: []HopPoint{{Time: now, Index: 1}}}
	c := NewCachingReader(inner, 8, 8)
	c.nowFn = func() time.Time { return now }

	ref := newRef("g", "t")
	from := now.Add(-7 * 24 * time.Hour)

	if _, err := c.QueryHopsTimeline(context.Background(), ref, from, now, QueryFilter{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.QueryHopsTimeline(context.Background(), ref, from, now, QueryFilter{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.QueryLatestHops(context.Background(), ref, QueryFilter{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.QueryLatestHops(context.Background(), ref, QueryFilter{}); err != nil {
		t.Fatal(err)
	}

	// 1 timeline miss + 1 timeline hit + 1 latest miss + 1 latest hit = 2 hits, 2 misses.
	stats := c.Stats()
	if stats.HopsHits != 2 || stats.HopsMisses != 2 {
		t.Fatalf("hops stats: got hits=%d misses=%d, want hits=2 misses=2", stats.HopsHits, stats.HopsMisses)
	}
}

func TestCachingReader_HopsTimeline_ServesStaleCacheOnError(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	clock := now
	inner := &fakeReader{hops: []HopPoint{{Time: now, Index: 1, IP: "1.1.1.1"}}}
	c := NewCachingReader(inner, 8, 8)
	c.nowFn = func() time.Time { return clock }

	ref := newRef("g", "t")
	from := now.Add(-7 * 24 * time.Hour)

	// Warm the cache with a successful query.
	if _, err := c.QueryHopsTimeline(context.Background(), ref, from, now, QueryFilter{}); err != nil {
		t.Fatal(err)
	}
	// Advance past the TTL so the entry is stale.
	clock = now.Add(cacheTTLLive + time.Second)
	// Inject a failure into the inner reader.
	inner.err = errors.New("clickhouse down")

	// Stale data must be served silently — no error, correct index.
	hops, err := c.QueryHopsTimeline(context.Background(), ref, from, now, QueryFilter{})
	if err != nil {
		t.Fatalf("got error %v; want stale data served silently", err)
	}
	if len(hops.Hops) != 1 || hops.Hops[0].Index != 1 {
		t.Fatalf("got %+v; want stale hop with Index=1", hops)
	}
	// Inner was called twice: once to warm, once on the stale miss.
	if got := inner.hopsTimeline.Load(); got != 2 {
		t.Fatalf("inner calls: got %d want 2", got)
	}
}

func TestCachingReader_Cycles_ServesStaleCacheOnError(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	clock := now
	inner := &fakeReader{out: []CyclePoint{{Time: now, Median: 1.5}}}
	c := NewCachingReader(inner, 8, 8)
	c.nowFn = func() time.Time { return clock }

	ref := newRef("g", "t")
	from := now.Add(-7 * 24 * time.Hour)

	// Warm the cache.
	if _, err := c.QueryCycles(context.Background(), ref, from, now, QueryFilter{Step: time.Hour}); err != nil {
		t.Fatal(err)
	}
	// Expire the entry.
	clock = now.Add(cacheTTLLive + time.Second)
	inner.err = errors.New("clickhouse down")

	pts, err := c.QueryCycles(context.Background(), ref, from, now, QueryFilter{Step: time.Hour})
	if err != nil {
		t.Fatalf("got error %v; want stale data served silently", err)
	}
	if len(pts) != 1 || pts[0].Median != 1.5 {
		t.Fatalf("got %+v; want stale cycle with Median=1.5", pts)
	}
	if got := inner.cycles.Load(); got != 2 {
		t.Fatalf("inner calls: got %d want 2", got)
	}
}

func TestCachingReader_Cycles_ErrorPropagatesWhenStaleEvicted(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	clock := now
	inner := &fakeReader{out: []CyclePoint{{Time: now, Median: 1.5}}}
	// cap=1 so inserting "b" evicts "a".
	c := NewCachingReader(inner, 1, 1)
	c.nowFn = func() time.Time { return clock }

	refA := newRef("g", "a")
	refB := newRef("g", "b")
	from := now.Add(-7 * 24 * time.Hour)

	// Warm "a".
	if _, err := c.QueryCycles(context.Background(), refA, from, now, QueryFilter{Step: time.Hour}); err != nil {
		t.Fatal(err)
	}
	// Insert "b", which evicts "a" from the LRU.
	if _, err := c.QueryCycles(context.Background(), refB, from, now, QueryFilter{Step: time.Hour}); err != nil {
		t.Fatal(err)
	}
	// Advance past TTL and inject failure.
	clock = now.Add(cacheTTLLive + time.Second)
	inner.err = errors.New("clickhouse down")

	// "a" was evicted — no stale entry exists, so the error must propagate.
	_, err := c.QueryCycles(context.Background(), refA, from, now, QueryFilter{Step: time.Hour})
	if err == nil {
		t.Fatal("got nil error; want propagated error (no stale entry after eviction)")
	}
}

// A leader outlives the request that started it, and every cache-key field is
// request-controlled, so admission is what stops an anonymous caller minting
// goroutines faster than they retire. Waiters must stay exempt: they join a
// leader already paid for, so refusing them would convert a stampede on one
// hot key into errors.
func TestCachingReader_Cycles_BoundsInflightLeaders(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	gate := make(chan struct{})
	inner := &slowFakeReader{gate: gate, cyclePts: []CyclePoint{{Time: now, Median: 1}}}
	c := NewCachingReader(inner, 8, 8)
	c.nowFn = func() time.Time { return now }

	ref := newRef("g", "t")
	// Distinct windows so every request is a distinct key and thus its own leader.
	windowFor := func(i int) time.Time { return now.Add(-time.Duration(i+1) * 24 * time.Hour) }

	saturate := make(chan error, maxInflightLeaders)
	for i := range maxInflightLeaders {
		go func() {
			_, err := c.QueryCycles(context.Background(), ref, windowFor(i), now, QueryFilter{Step: time.Hour})
			saturate <- err
		}()
	}
	// Every leader increments calls before blocking on the gate, so this both
	// waits for saturation and proves all of them registered.
	deadline := time.Now().Add(5 * time.Second)
	for inner.calls.Load() < int64(maxInflightLeaders) {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d leaders started", inner.calls.Load(), maxInflightLeaders)
		}
		time.Sleep(time.Millisecond)
	}

	// A new distinct key is refused rather than detaching another goroutine.
	if _, err := c.QueryCycles(context.Background(), ref, windowFor(maxInflightLeaders), now, QueryFilter{Step: time.Hour}); !errors.Is(err, ErrOverloaded) {
		t.Fatalf("distinct key past the cap: err = %v, want ErrOverloaded", err)
	}
	if got := inner.calls.Load(); got != int64(maxInflightLeaders) {
		t.Fatalf("refused request still reached the inner reader: calls = %d, want %d", got, maxInflightLeaders)
	}

	// A waiter on an already-admitted key is not refused. It has to attach
	// while the map is still saturated, so the gate stays shut until it has,
	// and the precondition is asserted rather than assumed: if the leaders had
	// already retired, the waiter would be admitted as a fresh leader and pass
	// for the wrong reason.
	waiter := make(chan error, 1)
	go func() {
		_, err := c.QueryCycles(context.Background(), ref, windowFor(0), now, QueryFilter{Step: time.Hour})
		waiter <- err
	}()
	time.Sleep(50 * time.Millisecond)
	c.mu.Lock()
	inflight := len(c.inflight)
	c.mu.Unlock()
	if inflight != maxInflightLeaders {
		t.Fatalf("precondition: inflight = %d, want %d still saturated when the waiter attached", inflight, maxInflightLeaders)
	}

	close(gate)
	for range maxInflightLeaders {
		if err := <-saturate; err != nil {
			t.Fatalf("saturating request failed: %v", err)
		}
	}
	select {
	case err := <-waiter:
		if err != nil {
			t.Fatalf("waiter on an admitted key was rejected: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter never completed")
	}

	// Slots are released, so a fresh distinct key is admitted again.
	if _, err := c.QueryCycles(context.Background(), ref, windowFor(maxInflightLeaders+1), now, QueryFilter{Step: time.Hour}); err != nil {
		t.Fatalf("after leaders retired: err = %v, want success", err)
	}
}

// captureLatestReader records the filter each inner QueryLatestHops receives,
// so tests can pin both cache-key behavior and the exact bytes the query ran
// with.
type captureLatestReader struct {
	fakeReader
	mu      sync.Mutex
	filters []QueryFilter
}

func (c *captureLatestReader) QueryLatestHops(ctx context.Context, ref config.TargetRef, f QueryFilter) (HopsResult, error) {
	c.mu.Lock()
	c.filters = append(c.filters, f)
	c.mu.Unlock()
	return c.fakeReader.QueryLatestHops(ctx, ref, f)
}

// The floor the entry was computed under must be part of its key: without it,
// a cached "latest path" keeps serving rows the current floor would exclude,
// and two requests whose floors differ silently share one entry.
func TestCachingReader_LatestHops_FreshnessFloorIsPartOfTheKey(t *testing.T) {
	inner := &captureLatestReader{fakeReader: fakeReader{hops: []HopPoint{{Index: 1, IP: "10.0.0.1"}}}}
	c := NewCachingReader(inner, 8, 8)
	ref := newRef("core", "gw")

	// 1_000_000_030 and 1_000_000_050 floor to the same 60s cell
	// (1_000_000_020); 1_000_000_085 floors to the next (1_000_000_080).
	t1 := time.Unix(1_000_000_030, 0)
	t2 := time.Unix(1_000_000_050, 0)
	t3 := time.Unix(1_000_000_085, 0)

	for _, since := range []time.Time{t1, t2} {
		if _, err := c.QueryLatestHops(context.Background(), ref, QueryFilter{LatestSince: since}); err != nil {
			t.Fatal(err)
		}
	}
	if got := inner.latestHops.Load(); got != 1 {
		t.Fatalf("same-quantum floors must share an entry: %d inner calls, want 1", got)
	}

	if _, err := c.QueryLatestHops(context.Background(), ref, QueryFilter{LatestSince: t3}); err != nil {
		t.Fatal(err)
	}
	if got := inner.latestHops.Load(); got != 2 {
		t.Fatalf("a new quantum must miss: %d inner calls, want 2", got)
	}
}

// The key is computed from the exact bytes the query runs with: the inner
// reader must receive the quantized floor, not the raw per-request value —
// otherwise two requests sharing a key ran different queries.
func TestCachingReader_LatestHops_InnerReceivesQuantizedFloor(t *testing.T) {
	inner := &captureLatestReader{fakeReader: fakeReader{hops: []HopPoint{{Index: 1}}}}
	c := NewCachingReader(inner, 8, 8)

	raw := time.Unix(1_000_000_030, 0)
	if _, err := c.QueryLatestHops(context.Background(), newRef("core", "gw"), QueryFilter{LatestSince: raw}); err != nil {
		t.Fatal(err)
	}
	inner.mu.Lock()
	defer inner.mu.Unlock()
	if len(inner.filters) != 1 {
		t.Fatalf("inner called %d times, want 1", len(inner.filters))
	}
	if want := time.Unix(1_000_000_020, 0); !inner.filters[0].LatestSince.Equal(want) {
		t.Fatalf("inner floor = %v, want quantized %v", inner.filters[0].LatestSince, want)
	}
}

// Zero LatestSince means "no floor" and must stay zero — the documented
// QueryFilter contract; flooring it would key a constant negative unix value.
func TestCachingReader_LatestHops_ZeroFloorStaysZero(t *testing.T) {
	inner := &captureLatestReader{fakeReader: fakeReader{hops: []HopPoint{{Index: 1}}}}
	c := NewCachingReader(inner, 8, 8)
	ref := newRef("core", "gw")

	for range 2 {
		if _, err := c.QueryLatestHops(context.Background(), ref, QueryFilter{}); err != nil {
			t.Fatal(err)
		}
	}
	if got := inner.latestHops.Load(); got != 1 {
		t.Fatalf("zero-floor requests must share one entry: %d inner calls", got)
	}
	inner.mu.Lock()
	defer inner.mu.Unlock()
	if !inner.filters[0].LatestSince.IsZero() {
		t.Fatalf("zero floor mutated to %v", inner.filters[0].LatestSince)
	}
}

// A refused query is a statement about the request, not about the upstream: a
// hop read past its row cap fails identically on every retry. Answering it from
// an expired entry serves a 200 carrying a window the read can no longer
// produce, and forever — only a success ever bumps an entry's expiry.
func TestCachingReader_RefusalIsNotServedFromStaleCache(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	ref := newRef("g", "t")
	from := now.Add(-7 * 24 * time.Hour)

	reads := map[string]func(*CachingReader) error{
		"QueryHopsTimeline": func(c *CachingReader) error {
			_, err := c.QueryHopsTimeline(context.Background(), ref, from, now, QueryFilter{Source: "master"})
			return err
		},
		"QueryLatestHops": func(c *CachingReader) error {
			_, err := c.QueryLatestHops(context.Background(), ref, QueryFilter{})
			return err
		},
		"QueryHopsAt": func(c *CachingReader) error {
			_, err := c.QueryHopsAt(context.Background(), ref, now, 30*time.Minute, QueryFilter{})
			return err
		},
		"QueryCycles": func(c *CachingReader) error {
			_, err := c.QueryCycles(context.Background(), ref, from, now, QueryFilter{})
			return err
		},
	}

	for name, read := range reads {
		t.Run(name, func(t *testing.T) {
			clock := now
			inner := &fakeReader{
				hops: []HopPoint{{Time: now, Index: 1, IP: "1.1.1.1"}},
				out:  []CyclePoint{{Time: now, Median: 1.5}},
			}
			c := NewCachingReader(inner, 8, 8)
			c.nowFn = func() time.Time { return clock }

			if err := read(c); err != nil {
				t.Fatalf("warm: %v", err)
			}
			clock = now.Add(cacheTTLLive + time.Second)
			inner.err = ErrHopsTruncated

			// Twice: the second call also proves the refusal was not itself
			// stored as the entry's new result.
			for i := range 2 {
				if err := read(c); !errors.Is(err, ErrHopsTruncated) {
					t.Fatalf("call %d: err = %v, want the refusal to reach the caller", i, err)
				}
			}

			// An unreachable upstream is a different claim and still resolves
			// to the stale entry — the refusal check must not have widened.
			inner.err = errors.New("clickhouse down")
			if err := read(c); err != nil {
				t.Fatalf("availability failure: err = %v, want the stale entry served", err)
			}
		})
	}
}

// atEchoReader answers a pinned read with the instant it was asked for, so a
// caller can tell which cycle the cache handed back.
type atEchoReader struct {
	Reader
	calls atomic.Int64
}

func (r *atEchoReader) QueryHopsAt(_ context.Context, _ config.TargetRef, at time.Time, _ time.Duration, _ QueryFilter) (HopsResult, error) {
	r.calls.Add(1)
	return HopsResult{
		Hops:   []HopPoint{{Time: at, Index: 1}},
		Cycles: []CycleCounters{{Time: at, Sent: 10}},
	}, nil
}

// Two cycles inside one minute are two different pins. Keying the entry on a
// minute-floored `at` while querying the precise one made the second click
// return the first click's path and counters — the two cloned slices were
// intact, they just described the wrong cycle.
func TestCachingReader_HopsAt_KeysOnTheRequestedCycle(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	inner := &atEchoReader{}
	c := NewCachingReader(inner, 8, 8)
	c.nowFn = func() time.Time { return now }
	ref := newRef("g", "t")

	first := now.Add(-time.Hour).Add(5 * time.Second)
	second := first.Add(40 * time.Second) // same minute, different cycle

	for _, at := range []time.Time{first, second} {
		res, err := c.QueryHopsAt(context.Background(), ref, at, 30*time.Minute, QueryFilter{})
		if err != nil {
			t.Fatalf("at %s: %v", at, err)
		}
		if len(res.Hops) != 1 || !res.Hops[0].Time.Equal(at) {
			t.Fatalf("at %s: got path for %v", at, res.Hops)
		}
		if len(res.Cycles) != 1 || !res.Cycles[0].Time.Equal(at) {
			t.Fatalf("at %s: got counters for %v", at, res.Cycles)
		}
	}
	if got := inner.calls.Load(); got != 2 {
		t.Fatalf("inner calls: got %d, want one per distinct cycle", got)
	}

	// The same pin re-requested — the auto-refresh tick and a remount both do
	// this — must still come off the entry rather than re-query.
	if _, err := c.QueryHopsAt(context.Background(), ref, first, 30*time.Minute, QueryFilter{}); err != nil {
		t.Fatal(err)
	}
	if got := inner.calls.Load(); got != 2 {
		t.Fatalf("inner calls after a repeat pin: got %d, want the entry served", got)
	}

	// The decorator is generic over Reader and may not assume how finely the
	// one underneath resolves `at`; the CH reader separates these two, and a
	// key coarser than the query it fronts serves one pin's path for another.
	sub := first.Add(500 * time.Millisecond)
	res, err := c.QueryHopsAt(context.Background(), ref, sub, 30*time.Minute, QueryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hops) != 1 || !res.Hops[0].Time.Equal(sub) {
		t.Fatalf("sub-second pin got %v, want its own entry", res.Hops)
	}
}

// The pin key must be exactly as fine as the query it fronts, in both
// directions. The reader resolves `at` to the millisecond, so two pins inside
// one millisecond issue the same query and must share the entry — and keying
// in nanoseconds also calls UnixNano outside its defined range, which
// ValidQueryTime's domain reaches.
func TestCachingReader_HopsAt_KeysAtTheResolutionTheReaderHas(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	inner := &atEchoReader{}
	c := NewCachingReader(inner, 8, 8)
	c.nowFn = func() time.Time { return now }
	ref := newRef("g", "t")

	first := now.Add(-time.Hour).Add(5 * time.Second)
	for _, at := range []time.Time{first, first.Add(100 * time.Microsecond)} {
		if _, err := c.QueryHopsAt(context.Background(), ref, at, 30*time.Minute, QueryFilter{}); err != nil {
			t.Fatalf("at %s: %v", at, err)
		}
	}
	if got := inner.calls.Load(); got != 1 {
		t.Fatalf("inner calls: got %d, want 1 — the two pins name one millisecond", got)
	}

	// The far end of the addressable range keys without wrapping.
	far := MaxQueryTime.Add(-time.Hour)
	if _, err := c.QueryHopsAt(context.Background(), ref, far, 30*time.Minute, QueryFilter{}); err != nil {
		t.Fatalf("at %s: %v", far, err)
	}
	if got := inner.calls.Load(); got != 2 {
		t.Fatalf("inner calls: got %d, want 2", got)
	}
}
