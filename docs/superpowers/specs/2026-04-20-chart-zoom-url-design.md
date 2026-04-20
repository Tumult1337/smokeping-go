# Chart zoom → shareable URL

## Problem

Drag-zooming a chart in the UI narrows the visible window, but:

1. The zoom isn't reflected in the URL, so you can't share an exact slice — only
   the pre-baked ranges (1h, 6h, 24h, …) are shareable.
2. Auto-refresh (30s tick) re-pins the x-scale to the server-echoed `fromSec`
   / `toSec`. Because the unzoomed echo has `to = now`, it slides forward each
   tick and the re-pin silently resets any drag-zoom the user applied.
3. Zoom is purely visual — the underlying fetch still spans the full relative
   range, so a tight zoom doesn't unlock finer-resolution data from the raw
   bucket.

## Goal

- Zooming in triggers a refetch with the tight window, so resolution matches
  the visible span (`storage.PickResolution` already handles this server-side).
- The zoom window is encoded in the URL for sharing.
- Auto-refresh preserves the zoom — refetches happen, but the viewport
  doesn't jump.
- MTR heatmap and hops timeline follow the same zoom window.

## Non-goals

- Persisting zoom across target switches (switching target clears zoom).
- Pushing a history entry per zoom step (we keep `replaceState`).
- Changing how the `-1h` / `-6h` / … range selector works when not zoomed.

## Architecture — overlay zoom on top of `range`

`App.tsx` gains one new piece of state:

```ts
const [zoom, setZoom] = useState<{from: number; to: number} | null>(initialUrl.zoom);
```

Semantics:

- `zoom == null` — behave as today. Cycle/HTTP/hops fetches pass `from=<range>`
  (a relative duration like `-24h`) with no `to`. Server echoes a sliding
  `[now - span, now]`. Auto-refresh slides the window forward each tick.
- `zoom != null` — fetches pass `from=<z0>&to=<z1>` as absolute unix seconds.
  Server echoes the same `[z0, z1]` every time. Auto-refresh refetches the
  same fixed window; any newly written in-range data appears without the
  viewport moving.

The relative `range` remains in state and in the URL as the "default" the user
was looking at before zooming. Clicking a range button clears the zoom and
returns to that relative window.

### Clearing zoom

- Range button click → `setZoom(null)`.
- Target switch → folded into the existing `isKeyChange` reset.
- Source chip change → keep the zoom (user is still looking at the same slice,
  just filtering data).
- Chart `onZoomChange(null)` (user double-click reset or zoom ≥ data extent).
- Toolbar "reset zoom" button, visible only when `zoom != null`.

### Data plumbing

A small helper in `App.tsx`:

```ts
const windowArgs: { from: string; to?: string } = zoom
  ? { from: String(zoom.from), to: String(zoom.to) }
  : { from: range };
```

Used by:

- `getCycles(id, windowArgs.from, windowArgs.to, resolution, source)`
- `getHttpSamples(id, windowArgs.from, windowArgs.to, source)`

`MtrHeatmap` and `HopsTable` already take `fromSec`/`toSec` — those come from
`cycles.from` / `cycles.to` (server echo) exactly as today, so they
automatically track zoom.

`getHops` (latest MTR) doesn't take a window and is unaffected.

## Detecting drag-zoom inside the chart

Each uPlot chart (`SmokeChart`, `SmokeBarChart`, `HttpChart`) grows one new
prop:

```ts
onZoomChange?: (window: {from: number; to: number} | null) => void;
```

The chart does NOT need a separate `zoom` prop — App already hands it the
correct `fromSec`/`toSec` for the window it should render (zoom values when
zoomed, server echo otherwise). Those are stable across refreshes in the
zoomed case, so the existing pin logic naturally stops re-pinning.

We distinguish user drag-zoom from our own programmatic `setScale` via a ref
flag. uPlot's `setScale` hook fires synchronously inside `u.setScale(...)`, so
flipping the flag immediately around the call is race-free:

```ts
const internalScaleRef = useRef(false);

// programmatic pin
internalScaleRef.current = true;
u.setScale("x", { min: pinFrom, max: pinTo });
internalScaleRef.current = false;
```

The hook:

```ts
setScale: [(u, key) => {
  if (key !== "x" || internalScaleRef.current) return;
  const min = u.scales.x.min;
  const max = u.scales.x.max;
  if (min == null || max == null) return;
  const xs = u.data[0] as number[] | undefined;
  if (!xs || xs.length === 0) return;
  const from = Math.floor(min);
  const to   = Math.ceil(max);
  const dataFrom = xs[0];
  const dataTo   = xs[xs.length - 1];
  if (from <= dataFrom && to >= dataTo) onZoomChangeRef.current?.(null);
  else onZoomChangeRef.current?.({ from, to });
}]
```

"Reset" = user zoom reaches or exceeds the data extent. uPlot's default
double-click resets to the data extent, which matches this condition.

`onZoomChange` is held in a ref so callback identity changes don't rebuild the
chart (same pattern as `onCyclePickRef`).

## Pinning behavior

The chart's existing pin effect keeps the same shape — it already reacts to
`fromSec`/`toSec` changes. The only change is wrapping the programmatic
`setScale` call with the `internalScaleRef` flag so the new `setScale` hook
doesn't misfire during pinning:

```ts
const pinChanged =
  pinRef.current.from !== fromSec || pinRef.current.to !== toSec;
pinRef.current = { from: fromSec, to: toSec };

u.batch(() => {
  if (pinChanged && fromSec != null && toSec != null) {
    internalScaleRef.current = true;
    u.setScale("x", { min: fromSec, max: toSec });
    internalScaleRef.current = false;
  }
  u.setData(built.data, false);
});
```

Auto-refresh behavior under each state (the subtle fix is that `fromSec` /
`toSec` already come out stable when zoomed, because App drives the fetch
with absolute bounds):

- **Unzoomed**: App fetches with relative `from=-24h`. Server echoes a
  sliding `[now - 24h, now]`. `fromSec`/`toSec` advance each tick,
  `pinChanged` is true, we re-pin to the sliding window. Matches today's
  behavior.
- **Zoomed**: App fetches with absolute `from=z0&to=z1`. Server echoes
  `[z0, z1]` identically on every refresh, so `fromSec`/`toSec` are stable.
  `pinChanged` false → no re-pin → no viewport jump.
- **Zoomed, user drags again**: `setScale` hook fires (internal flag is
  false) → `onZoomChange({from,to})` → App updates `zoom` → refetch returns
  new bounds → `pinChanged` true → we re-pin. The re-pin sets
  `internalScaleRef=true` while uPlot runs the hook, so no feedback loop.

## URL format

Two new query params, added alongside existing `target`, `range`, `mode`,
`source`, `t`:

- `z0=<unix_seconds>` — absolute window start
- `z1=<unix_seconds>` — absolute window end

Both present or both absent. A malformed or half-present pair is ignored
(`readUrlState` returns `zoom: null`).

Example shareable URL:

```
?target=home/router&range=-7d&z0=1745280000&z1=1745286000&mode=bars&source=pi&t=1745282400
```

"Target `home/router`, user was in the 7d range, zoomed into a 100-minute
slice, bar mode, filtered to source `pi`, MTR pinned to a cycle inside the
slice."

### Reading

Extend `readUrlState`:

```ts
type UrlState = {
  // … existing fields …
  zoom: { from: number; to: number } | null;
};

const z0Raw = p.get("z0"); const z1Raw = p.get("z1");
const z0 = z0Raw ? Number(z0Raw) : NaN;
const z1 = z1Raw ? Number(z1Raw) : NaN;
const zoom =
  Number.isFinite(z0) && Number.isFinite(z1) && z1 > z0
    ? { from: z0, to: z1 }
    : null;
```

### Writing

Extend the URL-sync effect:

```ts
if (zoom) {
  p.set("z0", String(zoom.from));
  p.set("z1", String(zoom.to));
}
```

Still `replaceState` — dragging through zoom levels must not balloon browser
history.

### Initial load with `z0`/`z1`

The existing fetch effect already depends on the window derivation. On mount
with zoom in the URL, the first fetch goes out with the absolute bounds, the
server returns data for `[z0, z1]`, and the chart's pin effect applies the
zoom window on first render. No flicker, no double-fetch.

### Stale zoom outside retention

If `[z0, z1]` falls outside the server's retention windows (raw: 7d, 30d
rollups, etc.), the server returns an empty point list. The existing "No
data in range" empty state renders. We do not special-case this — clicking
a range button (which clears zoom) recovers.

## UI affordances

- **Reset-zoom button** in the toolbar, next to the refresh button, visible
  only when `zoom != null`. Text: `reset zoom` (or icon `⤢`). Click →
  `setZoom(null)`.
- No other UI changes — range buttons, source chips, refresh, auto toggle
  all continue to work; they just interact with zoom as described in
  "Clearing zoom" above.

## Affected files

- `ui/src/App.tsx` — new `zoom` state, URL read/write, plumbing to fetches
  and charts, reset-zoom toolbar button.
- `ui/src/SmokeChart.tsx` — `onZoomChange`, setScale hook, updated pin
  effect.
- `ui/src/SmokeBarChart.tsx` — same pattern as SmokeChart.
- `ui/src/HttpChart.tsx` — same pattern.

No API / backend changes. The cycles/http/hops endpoints already accept
absolute unix-second `from`/`to` values (see `parseTimeParam` in
`internal/api/api.go`).

## Testing

Manual, since the touched code is all UI:

- Drag-zoom the latency chart → URL gains `z0`/`z1`, MTR heatmap narrows to
  match, hops table stays on latest (or on the existing pin if set).
- Open a URL with `z0`/`z1` set in a fresh tab → chart opens at the zoom
  window, same data as in the source tab.
- Leave the zoomed view idle for a minute → auto-refresh fires, viewport
  stays put.
- Double-click the chart → zoom clears, URL loses `z0`/`z1`, returns to the
  relative range.
- Click a different range button while zoomed → zoom clears, behaves as a
  plain range change.
- Zoom into a narrow (≤1h) slice of a `-30d` view → resolution downgrades
  from `1h` rollup to `raw` (visible in the "Latency — X resolution" title).
- Set `z0`/`z1` to a window outside retention → "No data in range".
