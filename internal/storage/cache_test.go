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
	cycles atomic.Int64
	out    []CyclePoint
	err    error
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
	return nil, nil
}
func (f *fakeReader) QueryHopsAt(context.Context, config.TargetRef, time.Time, time.Duration, QueryFilter) ([]HopPoint, error) {
	return nil, nil
}
func (f *fakeReader) QueryHopsTimeline(context.Context, config.TargetRef, time.Time, time.Time, QueryFilter) ([]HopPoint, error) {
	return nil, nil
}

func newRef(group, name string) config.TargetRef {
	return config.TargetRef{Group: group, Target: config.Target{Name: name}}
}

func TestCachingReader_HitsCacheWithinTTL(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	clock := now
	inner := &fakeReader{out: []CyclePoint{{Time: now, Median: 1.5}}}
	c := NewCachingReader(inner, 8)
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
	c := NewCachingReader(inner, 8)
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
	c := NewCachingReader(inner, 8)
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
	c := NewCachingReader(inner, 8)
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
	c := NewCachingReader(inner, 8)
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
	c := NewCachingReader(inner, 2)
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
	c := NewCachingReader(inner, 8)
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
