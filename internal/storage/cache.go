package storage

import (
	"container/list"
	"context"
	"sync"
	"sync/atomic"
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

// CachingReader wraps a Reader with two LRU+singleflight decorators: one for
// QueryCycles, one for the three hops query paths. QueryRTTs and
// QueryHTTPSamples pass through unchanged because they hit narrow raw-only
// windows where caching wins little. The decorator is safe for concurrent
// use; concurrent identical misses share one inner-reader call via
// singleflight (see fetchCycles / fetchHops).
type CachingReader struct {
	inner Reader
	max   int

	mu       sync.Mutex
	items    map[cycleCacheKey]*list.Element
	order    *list.List // front = most recently used
	inflight map[cycleCacheKey]*cycleInflight

	// hopsMax bounds the hops LRU separately from cycles because each hops
	// entry (a 7d timeline) can be hundreds of KB to a few MB — much bigger
	// than a cycles entry. Set via NewCachingReader's hopsMax parameter.
	hopsMax      int
	hopsMu       sync.Mutex
	hopsItems    map[hopsCacheKey]*list.Element
	hopsOrder    *list.List
	hopsInflight map[hopsCacheKey]*hopsInflight

	// Hit/miss counters. Updated atomically; read via Stats(). A miss is
	// counted on any path that reaches the inner reader (cold lookup, expired
	// entry, error from inner). A hit is counted only when the cached slice
	// is returned without consulting the inner reader.
	cyclesHits, cyclesMisses atomic.Int64
	hopsHits, hopsMisses     atomic.Int64

	// nowFn lets tests freeze time without monkey-patching time.Now.
	nowFn func() time.Time

	// Test hooks fired between the initial cache lookup and the inflight lock
	// acquisition. Production code never sets these. The race tests use them
	// to deterministically simulate a leader completing in the gap between
	// caller A's lookup and inflight check.
	testHookAfterCyclesLookup func()
	testHookAfterHopsLookup   func()
}

// CacheStats is a point-in-time snapshot of the LRU's hit/miss counters.
// Counters are monotonic since process start and never reset.
type CacheStats struct {
	CyclesHits, CyclesMisses int64
	HopsHits, HopsMisses     int64
}

// Stats returns a snapshot of cache hit/miss counters. Useful for exposing
// cache effectiveness to operators (Prometheus, status page, etc.).
func (c *CachingReader) Stats() CacheStats {
	return CacheStats{
		CyclesHits:   c.cyclesHits.Load(),
		CyclesMisses: c.cyclesMisses.Load(),
		HopsHits:     c.hopsHits.Load(),
		HopsMisses:   c.hopsMisses.Load(),
	}
}

type hopsCacheKey struct {
	kind                hopsKind
	group, name, source string
	// fromUnix/toUnix used for timeline + hopsAt windows; both zero for latest.
	fromUnix, toUnix int64
}

type hopsKind uint8

const (
	hopsKindLatest hopsKind = iota
	hopsKindAt
	hopsKindTimeline
)

type hopsCacheEntry struct {
	key     hopsCacheKey
	points  []HopPoint
	expires time.Time
}

// hopsInflight dedupes concurrent identical misses. Each hops query at 7d
// against real Influx returns ~600k rows / ~113MB JSON and takes 10-15s, so
// letting N parallel UI fetches stampede the same query (the React mount
// + range-button click + auto-refresh tick easily produce 3) is N× the
// load on Influx and N× the memory footprint. With singleflight only the
// first call runs the query; the rest wait on `done` and copy out `points`.
type hopsInflight struct {
	done   chan struct{}
	points []HopPoint
	err    error
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

// cycleInflight is the cycles analogue of hopsInflight. Same rationale:
// concurrent identical cold-key requests (UI mount + range click + auto-refresh
// tick) shouldn't fan out to N inner queries.
type cycleInflight struct {
	done   chan struct{}
	points []CyclePoint
	err    error
}

// NewCachingReader wraps inner with two LRUs sized independently: cyclesMax
// bounds cached `QueryCycles` results, hopsMax bounds cached hops queries.
// Caps are entry counts, not bytes — and the two are kept separate because
// a cycles entry is ~hundreds of KB while a 7d hops timeline entry can be
// ~100MB, so a unified cap that's safe for hops would starve cycles. Values
// ≤ 0 fall back to sane defaults (256 cycles, 16 hops).
func NewCachingReader(inner Reader, cyclesMax, hopsMax int) *CachingReader {
	if cyclesMax <= 0 {
		cyclesMax = 256
	}
	if hopsMax <= 0 {
		hopsMax = 16
	}
	return &CachingReader{
		inner:        inner,
		max:          cyclesMax,
		items:        make(map[cycleCacheKey]*list.Element, cyclesMax),
		order:        list.New(),
		inflight:     make(map[cycleCacheKey]*cycleInflight),
		hopsMax:      hopsMax,
		hopsItems:    make(map[hopsCacheKey]*list.Element, hopsMax),
		hopsOrder:    list.New(),
		hopsInflight: make(map[hopsCacheKey]*hopsInflight),
		nowFn:        time.Now,
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
		c.cyclesHits.Add(1)
		return pts, nil
	}

	return c.fetchCycles(ctx, key, c.ttlFor(to), func(ctx context.Context) ([]CyclePoint, error) {
		return c.inner.QueryCycles(ctx, ref, from, to, res, f)
	})
}

// fetchCycles is the cycles analogue of fetchHops: cache-or-leader-or-wait
// with context.WithoutCancel-decoupled execution so a cancelling caller can't
// poison concurrent waiters or discard the in-flight result. See fetchHops
// for the full rationale.
func (c *CachingReader) fetchCycles(ctx context.Context, key cycleCacheKey, ttl time.Duration, run func(context.Context) ([]CyclePoint, error)) ([]CyclePoint, error) {
	if c.testHookAfterCyclesLookup != nil {
		c.testHookAfterCyclesLookup()
	}

	c.mu.Lock()
	// Re-check the cache under the same lock that protects inflight. A leader
	// that completed between QueryCycles' initial lookup and now stored its
	// result and removed its inflight slot atomically (see runCyclesLeader);
	// without this re-check we'd see no inflight slot, no cache entry from
	// the stale earlier lookup, and become a redundant leader.
	if elem, ok := c.items[key]; ok {
		e := elem.Value.(*cycleCacheEntry)
		if !c.nowFn().After(e.expires) {
			c.order.MoveToFront(elem)
			out := make([]CyclePoint, len(e.points))
			copy(out, e.points)
			c.mu.Unlock()
			c.cyclesHits.Add(1)
			return out, nil
		}
	}
	c.cyclesMisses.Add(1)

	call, leader := c.inflight[key], false
	if call == nil {
		call = &cycleInflight{done: make(chan struct{})}
		c.inflight[key] = call
		leader = true
	}
	c.mu.Unlock()

	if leader {
		go c.runCyclesLeader(ctx, key, ttl, call, run)
	}

	select {
	case <-call.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if call.err != nil {
		return nil, call.err
	}
	out := make([]CyclePoint, len(call.points))
	copy(out, call.points)
	return out, nil
}

// runCyclesLeader is the cycles analogue of runHopsLeader. Detaches from the
// caller's context so a 60s+ raw-window query survives a browser navigation.
// Stores the result and removes the inflight slot under a single lock
// acquisition so a concurrent caller's re-check observes either both
// (cache hit) or neither (still inflight) — never the in-between state that
// would leak a redundant leader.
func (c *CachingReader) runCyclesLeader(ctx context.Context, key cycleCacheKey, ttl time.Duration, call *cycleInflight, run func(context.Context) ([]CyclePoint, error)) {
	runCtx := context.WithoutCancel(ctx)
	pts, err := run(runCtx)

	c.mu.Lock()
	if err == nil {
		c.storeLocked(key, pts, ttl)
	}
	delete(c.inflight, key)
	c.mu.Unlock()

	call.points = pts
	call.err = err
	close(call.done)
}

func (c *CachingReader) QueryRTTs(ctx context.Context, ref config.TargetRef, from, to time.Time, f QueryFilter) ([]RTTPoint, error) {
	return c.inner.QueryRTTs(ctx, ref, from, to, f)
}

func (c *CachingReader) QueryHTTPSamples(ctx context.Context, ref config.TargetRef, from, to time.Time, f QueryFilter) ([]HTTPPoint, error) {
	return c.inner.QueryHTTPSamples(ctx, ref, from, to, f)
}

func (c *CachingReader) QueryLatestHops(ctx context.Context, ref config.TargetRef, f QueryFilter) ([]HopPoint, error) {
	// Latest is always live: TTL = cacheTTLLive so a fresh cycle replaces the
	// stale entry within ~1 ping interval. fromUnix/toUnix stay zero since
	// the call has no window.
	key := hopsCacheKey{
		kind:   hopsKindLatest,
		group:  ref.Group,
		name:   ref.Target.Name,
		source: f.Source,
	}
	return c.fetchHops(ctx, key, cacheTTLLive, func(ctx context.Context) ([]HopPoint, error) {
		return c.inner.QueryLatestHops(ctx, ref, f)
	})
}

func (c *CachingReader) QueryHopsAt(ctx context.Context, ref config.TargetRef, at time.Time, window time.Duration, f QueryFilter) ([]HopPoint, error) {
	// Quantize `at` so two clicks landing in the same minute share an entry.
	// `window` becomes part of the key so an unusual override doesn't collide.
	key := hopsCacheKey{
		kind:     hopsKindAt,
		group:    ref.Group,
		name:     ref.Target.Name,
		source:   f.Source,
		fromUnix: floorUnix(at, cacheKeyToQuantum),
		toUnix:   int64(window / time.Second),
	}
	return c.fetchHops(ctx, key, c.ttlFor(at), func(ctx context.Context) ([]HopPoint, error) {
		return c.inner.QueryHopsAt(ctx, ref, at, window, f)
	})
}

func (c *CachingReader) QueryHopsTimeline(ctx context.Context, ref config.TargetRef, from, to time.Time, f QueryFilter) ([]HopPoint, error) {
	// Same quantization scheme as cycles: 5m floor on from, 60s ceil on to.
	// This lets the heatmap's 30s auto-refresh tick reuse the cached slice
	// roughly half the time, and identical wide-window views (7d, etc.)
	// always hit warm.
	key := hopsCacheKey{
		kind:     hopsKindTimeline,
		group:    ref.Group,
		name:     ref.Target.Name,
		source:   f.Source,
		fromUnix: floorUnix(from, cacheKeyFromQuantum),
		toUnix:   ceilUnix(to, cacheKeyToQuantum),
	}
	return c.fetchHops(ctx, key, c.ttlFor(to), func(ctx context.Context) ([]HopPoint, error) {
		return c.inner.QueryHopsTimeline(ctx, ref, from, to, f)
	})
}

// fetchHops is the cache + singleflight helper shared by the three hops
// query paths. Callers supply the cache key, the TTL the result should live
// for on success, and a closure that runs the inner reader. Behaviour:
//   - cache hit (within TTL): copy of cached slice, no lock contention with
//     other keys.
//   - in-flight call for the same key: wait on the leader's `done` channel
//     and return a copy of its result (or its error). Avoids duplicate
//     Influx queries when the UI rapidly remounts / changes range / ticks.
//   - cache miss with no leader: spawn one goroutine that runs the query
//     under a context decoupled from any single caller, store on success,
//     signal everyone waiting on `done`. The originating caller becomes a
//     waiter on the same channel, so the leader/waiter paths are unified.
//
// The decoupling is load-bearing: a 7d hops query takes 30-60s, and a
// browser nav / range click / AbortController fire would otherwise cancel
// the leader's ctx, kill the Influx query, and propagate ctx.Canceled to
// every waiter — the slow path would never warm the cache. With
// context.WithoutCancel the in-flight query keeps running on the
// inner-Reader's own HTTP timeout (90s) and the next request lands on a
// warm entry.
//
// Errors are NOT cached (matches QueryCycles): a transient Influx hiccup
// shouldn't poison subsequent fetches.
func (c *CachingReader) fetchHops(ctx context.Context, key hopsCacheKey, ttl time.Duration, run func(context.Context) ([]HopPoint, error)) ([]HopPoint, error) {
	if hops, ok := c.hopsLookup(key); ok {
		c.hopsHits.Add(1)
		return hops, nil
	}

	if c.testHookAfterHopsLookup != nil {
		c.testHookAfterHopsLookup()
	}

	c.hopsMu.Lock()
	// Re-check the cache under the same lock that protects inflight. A leader
	// that completed between this caller's hopsLookup and now stored its
	// result and removed its inflight slot atomically (see runHopsLeader);
	// without this re-check we'd see no inflight slot, no entry from the
	// stale earlier lookup, and become a redundant leader — firing the same
	// 30-60s/100MB Influx query that just completed.
	if elem, ok := c.hopsItems[key]; ok {
		e := elem.Value.(*hopsCacheEntry)
		if !c.nowFn().After(e.expires) {
			c.hopsOrder.MoveToFront(elem)
			out := make([]HopPoint, len(e.points))
			copy(out, e.points)
			c.hopsMu.Unlock()
			c.hopsHits.Add(1)
			return out, nil
		}
	}
	c.hopsMisses.Add(1)

	call, leader := c.hopsInflight[key], false
	if call == nil {
		// No leader yet — register one and become it. Spawn the actual run
		// below after we've dropped the lock.
		call = &hopsInflight{done: make(chan struct{})}
		c.hopsInflight[key] = call
		leader = true
	}
	c.hopsMu.Unlock()

	if leader {
		go c.runHopsLeader(ctx, key, ttl, call, run)
	}

	select {
	case <-call.done:
	case <-ctx.Done():
		// This caller (leader or waiter) gave up — but the goroutine keeps
		// running so other waiters and future requests still benefit.
		return nil, ctx.Err()
	}
	if call.err != nil {
		return nil, call.err
	}
	out := make([]HopPoint, len(call.points))
	copy(out, call.points)
	return out, nil
}

// runHopsLeader executes the inner query under a context detached from any
// single caller (see fetchHops doc), records the result on `call`, removes
// the in-flight slot, and signals waiters. The cache store + inflight delete
// happen under one lock acquisition so a concurrent caller's re-check
// observes either both (cache hit) or neither (still inflight) — never the
// in-between state that would let a redundant leader sneak through. The
// store still happens BEFORE close(done) so a follow-up request on the same
// goroutine hits the cache immediately.
func (c *CachingReader) runHopsLeader(ctx context.Context, key hopsCacheKey, ttl time.Duration, call *hopsInflight, run func(context.Context) ([]HopPoint, error)) {
	runCtx := context.WithoutCancel(ctx)
	hops, err := run(runCtx)

	c.hopsMu.Lock()
	if err == nil {
		c.hopsStoreLocked(key, hops, ttl)
	}
	delete(c.hopsInflight, key)
	c.hopsMu.Unlock()

	call.points = hops
	call.err = err
	close(call.done)
}

func (c *CachingReader) hopsLookup(key hopsCacheKey) ([]HopPoint, bool) {
	c.hopsMu.Lock()
	defer c.hopsMu.Unlock()
	elem, ok := c.hopsItems[key]
	if !ok {
		return nil, false
	}
	e := elem.Value.(*hopsCacheEntry)
	if c.nowFn().After(e.expires) {
		c.hopsOrder.Remove(elem)
		delete(c.hopsItems, key)
		return nil, false
	}
	c.hopsOrder.MoveToFront(elem)
	out := make([]HopPoint, len(e.points))
	copy(out, e.points)
	return out, true
}

func (c *CachingReader) hopsStore(key hopsCacheKey, hops []HopPoint, ttl time.Duration) {
	c.hopsMu.Lock()
	defer c.hopsMu.Unlock()
	c.hopsStoreLocked(key, hops, ttl)
}

// hopsStoreLocked assumes c.hopsMu is held by the caller. Used by
// runHopsLeader so it can store + delete-inflight under one lock acquisition.
func (c *CachingReader) hopsStoreLocked(key hopsCacheKey, hops []HopPoint, ttl time.Duration) {
	expires := c.nowFn().Add(ttl)
	if elem, ok := c.hopsItems[key]; ok {
		e := elem.Value.(*hopsCacheEntry)
		e.points = hops
		e.expires = expires
		c.hopsOrder.MoveToFront(elem)
		return
	}
	e := &hopsCacheEntry{key: key, points: hops, expires: expires}
	elem := c.hopsOrder.PushFront(e)
	c.hopsItems[key] = elem
	for c.hopsOrder.Len() > c.hopsMax {
		oldest := c.hopsOrder.Back()
		if oldest == nil {
			break
		}
		c.hopsOrder.Remove(oldest)
		delete(c.hopsItems, oldest.Value.(*hopsCacheEntry).key)
	}
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
	c.storeLocked(key, pts, ttl)
}

// storeLocked assumes c.mu is held by the caller. Used by runCyclesLeader so
// it can store + delete-inflight under one lock acquisition.
func (c *CachingReader) storeLocked(key cycleCacheKey, pts []CyclePoint, ttl time.Duration) {
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
