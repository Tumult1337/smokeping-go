package storage

import (
	"context"
	"errors"
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
	err          error
}

func (f *fakeReader) QueryCycles(ctx context.Context, ref config.TargetRef, from, to time.Time, res Resolution, q QueryFilter) ([]CyclePoint, error) {
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
func (f *fakeReader) QueryLatestHops(context.Context, config.TargetRef, QueryFilter) ([]HopPoint, error) {
	f.latestHops.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return f.hops, nil
}
func (f *fakeReader) QueryHopsAt(context.Context, config.TargetRef, time.Time, time.Duration, QueryFilter) ([]HopPoint, error) {
	f.hopsAt.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return f.hops, nil
}
func (f *fakeReader) QueryHopsTimeline(context.Context, config.TargetRef, time.Time, time.Time, QueryFilter) ([]HopPoint, error) {
	f.hopsTimeline.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return f.hops, nil
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

func (s *slowFakeReader) QueryCycles(context.Context, config.TargetRef, time.Time, time.Time, Resolution, QueryFilter) ([]CyclePoint, error) {
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
func (s *slowFakeReader) QueryLatestHops(context.Context, config.TargetRef, QueryFilter) ([]HopPoint, error) {
	s.calls.Add(1)
	<-s.gate
	return s.hops, nil
}
func (s *slowFakeReader) QueryHopsAt(context.Context, config.TargetRef, time.Time, time.Duration, QueryFilter) ([]HopPoint, error) {
	s.calls.Add(1)
	<-s.gate
	return s.hops, nil
}
func (s *slowFakeReader) QueryHopsTimeline(context.Context, config.TargetRef, time.Time, time.Time, QueryFilter) ([]HopPoint, error) {
	s.calls.Add(1)
	<-s.gate
	return s.hops, nil
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

	if _, err := c.QueryCycles(context.Background(), ref, from, to, Resolution1h, QueryFilter{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.QueryCycles(context.Background(), ref, from, to, Resolution1h, QueryFilter{}); err != nil {
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

	if _, err := c.QueryCycles(context.Background(), ref, from, to, Resolution1h, QueryFilter{}); err != nil {
		t.Fatal(err)
	}
	clock = now.Add(cacheTTLLive + time.Second)
	if _, err := c.QueryCycles(context.Background(), ref, from, to, Resolution1h, QueryFilter{}); err != nil {
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

	if _, err := c.QueryCycles(context.Background(), ref, from, to, Resolution1h, QueryFilter{}); err != nil {
		t.Fatal(err)
	}
	clock = now.Add(2 * time.Minute)
	if _, err := c.QueryCycles(context.Background(), ref, from, to, Resolution1h, QueryFilter{}); err != nil {
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
	if _, err := c.QueryCycles(context.Background(), ref, now1.Add(-7*24*time.Hour), now1, Resolution1h, QueryFilter{}); err != nil {
		t.Fatal(err)
	}
	clock = now2
	if _, err := c.QueryCycles(context.Background(), ref, now2.Add(-7*24*time.Hour), now2, Resolution1h, QueryFilter{}); err != nil {
		t.Fatal(err)
	}
	if got := inner.cycles.Load(); got != 1 {
		t.Fatalf("inner calls: got %d want 1 (drift within quantum should reuse entry)", got)
	}
}

func TestCachingReader_DifferentSourcesAreSeparate(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	inner := &fakeReader{out: []CyclePoint{{Time: now}}}
	c := NewCachingReader(inner, 8, 8)
	c.nowFn = func() time.Time { return now }

	ref := newRef("g", "t")
	from := now.Add(-7 * 24 * time.Hour)

	if _, err := c.QueryCycles(context.Background(), ref, from, now, Resolution1h, QueryFilter{Source: "master"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.QueryCycles(context.Background(), ref, from, now, Resolution1h, QueryFilter{Source: "slave-a"}); err != nil {
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
		if _, err := c.QueryCycles(context.Background(), newRef("g", name), from, now, Resolution1h, QueryFilter{}); err != nil {
			t.Fatal(err)
		}
	}
	// Re-query "a" — it was evicted when "c" was inserted, so this should miss.
	if _, err := c.QueryCycles(context.Background(), newRef("g", "a"), from, now, Resolution1h, QueryFilter{}); err != nil {
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
		_, err := c.QueryCycles(context.Background(), ref, from, now, Resolution1h, QueryFilter{})
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
			_, err := c.QueryCycles(context.Background(), ref, from, now, Resolution1h, QueryFilter{})
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
	// for its result. Each Influx query at 7d for a real target is ~13s and
	// allocates ~113MB of JSON, so this matters.
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
		if _, err := c.QueryCycles(context.Background(), ref, from, now, Resolution1h, QueryFilter{}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.QueryHopsTimeline(context.Background(), ref, from, now, QueryFilter{}); err != nil {
			t.Fatal(err)
		}
	}

	// cyclesMax=2: re-querying "a" must miss (evicted by "c").
	if _, err := c.QueryCycles(context.Background(), newRef("g", "a"), from, now, Resolution1h, QueryFilter{}); err != nil {
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
	if _, err := c.QueryCycles(context.Background(), ref, from, now, Resolution1h, QueryFilter{}); err != nil {
		t.Fatal(err)
	}
	// Second call within TTL: hit.
	if _, err := c.QueryCycles(context.Background(), ref, from, now, Resolution1h, QueryFilter{}); err != nil {
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
		_, _ = c.QueryCycles(context.Background(), ref, from, now, Resolution1h, QueryFilter{})
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
		res:      Resolution1h,
		fromUnix: floorUnix(from, cacheKeyFromQuantum),
		toUnix:   ceilUnix(now, cacheKeyToQuantum),
	}
	c.testHookAfterCyclesLookup = func() {
		c.testHookAfterCyclesLookup = nil
		c.store(key, []CyclePoint{{Time: now, Median: 42}}, time.Hour)
	}

	pts, err := c.QueryCycles(context.Background(), ref, from, now, Resolution1h, QueryFilter{})
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
		c.hopsStore(key, []HopPoint{{Time: now, Index: 42}}, time.Hour)
	}

	pts, err := c.QueryHopsTimeline(context.Background(), ref, from, now, QueryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if got := inner.hopsTimeline.Load(); got != 0 {
		t.Fatalf("inner hops calls: got %d want 0 (re-check under inflight lock should serve from cache)", got)
	}
	if len(pts) != 1 || pts[0].Index != 42 {
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
