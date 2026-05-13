# Design: Cache stale-on-error + Y-axis solo rescale

Date: 2026-05-13

## Background

Two issues reported:
1. MTR heatmap shows a 502 error when Influx is slow/down, discarding previously-loaded data that was still visible.
2. In multi-source "all" view, soloing one source in the legend doesn't rescale the Y-axis — the peak of a different source's spike keeps the visible data compressed.

## Part 1 — Stale-on-error (backend + frontend)

### Backend: `internal/storage/cache.go`

The `CachingReader` already caches `QueryHopsTimeline` with singleflight deduplication and quantized keys. The gap: both `hopsLookup` and `lookup` (cycles) **eagerly evict** expired entries from the LRU map on discovery, so by the time the inner Influx query finishes and fails, the stale entry is already gone.

**Changes:**

`hopsLookup` — remove the eviction-on-expiry lines:
```go
// Before:
if c.nowFn().After(e.expires) {
    c.hopsOrder.Remove(elem)
    delete(c.hopsItems, key)
    return nil, false
}
// After: just return false, leave entry in map for error fallback
if c.nowFn().After(e.expires) {
    return nil, false
}
```

`lookup` (cycles) — identical change.

`runHopsLeader` — on inner error, copy stale entry if present, signal waiters with stale data + nil error:
```go
var stale []HopPoint
c.hopsMu.Lock()
if err == nil {
    c.hopsStoreLocked(key, hops, ttl)
} else if elem, ok := c.hopsItems[key]; ok {
    e := elem.Value.(*hopsCacheEntry)
    stale = make([]HopPoint, len(e.points))
    copy(stale, e.points)
}
delete(c.hopsInflight, key)
c.hopsMu.Unlock()

if err != nil && stale != nil {
    call.points = stale  // call.err stays nil
} else {
    call.points = hops
    call.err = err
}
close(call.done)
```

`runCyclesLeader` — same pattern (uses `[]CyclePoint`).

**LRU behaviour:** Stale entries are not MoveToFront'd on lookup, so they naturally drift to the back and get evicted by fresh inserts when the LRU is full. This is correct — stale fallback is best-effort, doesn't starve fresh entries.

**Source isolation:** Different source values produce different cache keys. Switching from "all" (source="") to "master" (source="master") adds a new entry; the "all" entry remains in the LRU until evicted by LRU pressure or process restart.

**Existing tests:** All pass unchanged. The `ErrorBypassesCache` tests start with a cold cache (no prior success), so there's nothing stale to serve — errors still propagate as before.

**New tests:**
- `TestCachingReader_HopsTimeline_ServesStaleCacheOnError`: warm entry → advance clock past TTL → inner returns error → stale data returned, no error
- `TestCachingReader_Cycles_ServesStaleCacheOnError`: same pattern for cycles

### Frontend: `ui/src/MtrHeatmap.tsx`

`hops` state is NOT cleared on error (only `err` is set), so previous data is already in memory. The bug is the render guard: `if (err) return <error>` fires before the canvas.

**Change:** Only replace the view with the error message when there's no data to show. When `hops` exists and is non-empty, render the canvas normally and add a small `"stale"` badge.

```tsx
// Old: always renders error instead of canvas
if (err) return <div className="error">{err}</div>;

// New: only replace view when no data available
if (err && (hops === null || hops.length === 0)) {
    return <div className="error">{err}</div>;
}
// ... rest of guards unchanged ...
// Canvas section:
return (
  <div ref={wrapRef} ...>
    <canvas ref={canvasRef} style={{ display: "block" }} />
    {err && (
      <div className="stale-badge">stale</div>  // positioned top-right
    )}
  </div>
);
```

The stale badge is a small absolute-positioned pill with muted styling so it doesn't obscure the heatmap.

## Part 2 — Y-axis rescale on source solo

### Problem

When a user clicks a source name in the legend (setting `soloSource`), the other sources' series are hidden but the Y scale still covers all sources' data range. This compresses the visible source into a small region of the chart when another source has a high-latency spike.

### `ui/src/SmokeBarChart.tsx`

`buildSources` already computes `yRange` from all sources. Add per-source ranges:

```ts
type Built = {
  ...
  yRange: [number, number];          // existing: global range (all sources)
  sourceYRanges: Map<string, [number, number]>;  // new: per-source
};
```

In `buildSources`, for each source:
```ts
let srcLo = Infinity, srcHi = -Infinity;
for (const p of pts) {
    if (p.LossPct >= 100) continue;
    if (p.Min > 0 && p.Min < srcLo) srcLo = p.Min;
    if (p.Max > srcHi) srcHi = p.Max;
}
if (isFinite(srcLo) && isFinite(srcHi)) {
    const pad = Math.max(1, (srcHi - srcLo) * 0.1);
    sourceYRanges.set(name, [Math.max(0, srcLo - pad), srcHi + pad]);
}
```

Extend the existing `soloSource` effect to update `yRangeRef` and call `u.setScale`:
```ts
useEffect(() => {
    soloIdxRef.current = soloSource != null ? built.sources.indexOf(soloSource) : null;
    const range = soloSource != null
        ? (built.sourceYRanges.get(soloSource) ?? built.yRange)
        : built.yRange;
    yRangeRef.current = range;
    const u = plotRef.current;
    if (u) {
        u.setScale("y", { min: range[0], max: range[1] });
        u.redraw(false, true);
    }
}, [soloSource, built.sources, built.yRange, built.sourceYRanges]);
```

### `ui/src/SmokeChart.tsx`

`buildAligned` returns `Built`. Add `sourceYRanges: Map<string, [number, number]>`.

Per-source range computed from each source's sorted points (same min/max logic as SmokeBarChart).

The existing `soloSource` effect (which calls `setSeries`) gains a Y-scale update:
```ts
useEffect(() => {
    const u = plotRef.current;
    if (!u) return;
    u.batch(() => {
        for (let i = 1; i < u.series.length; i++) { ... }  // existing
    });
    // New: rescale Y
    internalScaleRef.current = true;
    if (soloSource != null) {
        const r = built.sourceYRanges.get(soloSource);
        if (r) u.setScale("y", { min: r[0], max: r[1] });
    } else {
        u.setScale("y", { min: undefined, max: undefined });  // reset to auto
    }
    internalScaleRef.current = false;
}, [hidden, soloSource, sourcesKey]);
```

The `internalScaleRef` guard prevents the `setScale` hook from misinterpreting the programmatic Y rescale as a user zoom gesture.

## Files changed

- `internal/storage/cache.go` — stale-on-error for hops and cycles
- `internal/storage/cache_test.go` — 2 new tests
- `ui/src/MtrHeatmap.tsx` — render stale canvas + badge
- `ui/src/SmokeChart.tsx` — per-source Y range + soloSource rescale
- `ui/src/SmokeBarChart.tsx` — per-source Y range + soloSource rescale
