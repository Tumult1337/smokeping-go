package storage

import (
	"container/list"
	"context"
	"sync"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
)

// Cache tunables. The UI auto-refresh polls every 30s; the `to` quantum is
// wider (60s) so consecutive polls land in the same cell roughly half the
// time and serve from cache. Setting it equal to the poll cadence would
// produce no overlap at all because adjacent polls would always cross a
// boundary. Going wider than 60s would help more but at the cost of stale
// trailing-edge data on every cache hit.
//
// The `from` quantum is wider still (5m) because the leading edge of a
// "last 7 days" view shifts continuously as wall-clock advances; rounding
// it down to 5-minute boundaries means the same cache key serves many
// auto-refreshes in a row.
const (
	cacheKeyFromQuantum = 5 * time.Minute
	cacheKeyToQuantum   = 60 * time.Second
	// cacheTTLLive matches the `to` quantum so an entry expires roughly
	// when the next cell would replace it anyway. Bigger TTL would keep
	// stale tails alive past the rollup of a new cycle.
	cacheTTLLive     = 60 * time.Second
	cacheTTLHistoric = 5 * time.Minute
	// liveBoundary is how close `to` must be to `now` to count as a live
	// window. One ping cycle of slack keeps fresh-data refreshes short
	// even when the request slightly trails real time.
	liveBoundary = 60 * time.Second
)

// CachingReader wraps a Reader with a small LRU cache for QueryCycles.
// Other read methods pass through unchanged — they're either cheap (status,
// hops-at, latest-hops) or hit narrow raw-only windows where caching wins
// little. The decorator is safe for concurrent use; concurrent identical
// misses both query the inner Reader (no singleflight) — acceptable because
// the 30s key quantum bounds the burst.
type CachingReader struct {
	inner Reader
	max   int

	mu    sync.Mutex
	items map[cycleCacheKey]*list.Element
	order *list.List // front = most recently used

	// nowFn lets tests freeze time without monkey-patching time.Now.
	nowFn func() time.Time
}

type cycleCacheKey struct {
	group, name, source string
	res                 Resolution
	fromUnix, toUnix    int64
}

type cycleCacheEntry struct {
	key     cycleCacheKey
	points  []CyclePoint
	expires time.Time
}

// NewCachingReader wraps inner. max is the LRU capacity (entries, not bytes);
// values ≤ 0 fall back to a sane default of 256.
func NewCachingReader(inner Reader, max int) *CachingReader {
	if max <= 0 {
		max = 256
	}
	return &CachingReader{
		inner: inner,
		max:   max,
		items: make(map[cycleCacheKey]*list.Element, max),
		order: list.New(),
		nowFn: time.Now,
	}
}

func (c *CachingReader) QueryCycles(ctx context.Context, ref config.TargetRef, from, to time.Time, res Resolution, f QueryFilter) ([]CyclePoint, error) {
	key := cycleCacheKey{
		group:    ref.Group,
		name:     ref.Target.Name,
		source:   f.Source,
		res:      res,
		fromUnix: floorUnix(from, cacheKeyFromQuantum),
		toUnix:   ceilUnix(to, cacheKeyToQuantum),
	}

	if pts, ok := c.lookup(key); ok {
		return pts, nil
	}

	pts, err := c.inner.QueryCycles(ctx, ref, from, to, res, f)
	if err != nil {
		return nil, err
	}
	c.store(key, pts, c.ttlFor(to))
	return pts, nil
}

func (c *CachingReader) QueryRTTs(ctx context.Context, ref config.TargetRef, from, to time.Time, f QueryFilter) ([]RTTPoint, error) {
	return c.inner.QueryRTTs(ctx, ref, from, to, f)
}

func (c *CachingReader) QueryHTTPSamples(ctx context.Context, ref config.TargetRef, from, to time.Time, f QueryFilter) ([]HTTPPoint, error) {
	return c.inner.QueryHTTPSamples(ctx, ref, from, to, f)
}

func (c *CachingReader) QueryLatestHops(ctx context.Context, ref config.TargetRef, f QueryFilter) ([]HopPoint, error) {
	return c.inner.QueryLatestHops(ctx, ref, f)
}

func (c *CachingReader) QueryHopsAt(ctx context.Context, ref config.TargetRef, at time.Time, window time.Duration, f QueryFilter) ([]HopPoint, error) {
	return c.inner.QueryHopsAt(ctx, ref, at, window, f)
}

func (c *CachingReader) QueryHopsTimeline(ctx context.Context, ref config.TargetRef, from, to time.Time, f QueryFilter) ([]HopPoint, error) {
	return c.inner.QueryHopsTimeline(ctx, ref, from, to, f)
}

func (c *CachingReader) ttlFor(to time.Time) time.Duration {
	if c.nowFn().Sub(to) < liveBoundary {
		return cacheTTLLive
	}
	return cacheTTLHistoric
}

func (c *CachingReader) lookup(key cycleCacheKey) ([]CyclePoint, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	elem, ok := c.items[key]
	if !ok {
		return nil, false
	}
	e := elem.Value.(*cycleCacheEntry)
	if c.nowFn().After(e.expires) {
		c.order.Remove(elem)
		delete(c.items, key)
		return nil, false
	}
	c.order.MoveToFront(elem)
	out := make([]CyclePoint, len(e.points))
	copy(out, e.points)
	return out, true
}

func (c *CachingReader) store(key cycleCacheKey, pts []CyclePoint, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	expires := c.nowFn().Add(ttl)
	if elem, ok := c.items[key]; ok {
		e := elem.Value.(*cycleCacheEntry)
		e.points = pts
		e.expires = expires
		c.order.MoveToFront(elem)
		return
	}
	e := &cycleCacheEntry{key: key, points: pts, expires: expires}
	elem := c.order.PushFront(e)
	c.items[key] = elem
	for c.order.Len() > c.max {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		c.order.Remove(oldest)
		delete(c.items, oldest.Value.(*cycleCacheEntry).key)
	}
}

func floorUnix(t time.Time, q time.Duration) int64 {
	qs := int64(q / time.Second)
	u := t.Unix()
	return (u / qs) * qs
}

func ceilUnix(t time.Time, q time.Duration) int64 {
	qs := int64(q / time.Second)
	u := t.Unix()
	return ((u + qs - 1) / qs) * qs
}
