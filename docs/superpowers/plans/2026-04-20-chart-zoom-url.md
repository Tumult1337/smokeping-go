# Chart zoom → shareable URL Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the user drag-zoom a chart and have the tight window (a) refetch data at matching resolution, (b) be encoded in the URL for sharing, and (c) survive auto-refresh without the viewport jumping.

**Architecture:** Overlay an absolute `zoom: {from, to} | null` state in `App.tsx` on top of the existing relative `range` selector. When zoom is set, fetches use absolute unix-second bounds (so the server echoes stable `from`/`to`, which already makes the chart's existing pin logic no-op across refreshes). Charts emit `onZoomChange` via uPlot's `setScale` hook, guarded by an `internalScaleRef` flag so programmatic pins don't feedback-loop. URL gains `z0`/`z1` params.

**Tech Stack:** React 18, TypeScript (strict), uPlot, vanilla URLSearchParams. No new dependencies. No backend changes — `parseTimeParam` in `internal/api/api.go` already accepts unix-second `from`/`to`.

**Spec:** `docs/superpowers/specs/2026-04-20-chart-zoom-url-design.md`

---

## File Structure

- **Modify** `ui/src/App.tsx`
  - Extend `readUrlState` to parse `z0`/`z1`.
  - Add `zoom` state.
  - Derive `windowArgs = {from, to?}` from zoom or range.
  - Plumb `windowArgs` into `getCycles` + thread `onZoomChange` into `SmokeChart` / `SmokeBarChart` / `HttpChart`.
  - Clear zoom on target switch and on range-button click.
  - Extend URL-sync effect to write `z0`/`z1`.
  - Add "reset zoom" toolbar button, visible only when `zoom != null`.

- **Modify** `ui/src/SmokeChart.tsx`
  - Add `onZoomChange?: (w: {from:number;to:number} | null) => void` prop.
  - Add `internalScaleRef = useRef(false)` and guard `u.setScale` with it.
  - Register uPlot `setScale` hook that reports user-driven x-range changes.

- **Modify** `ui/src/SmokeBarChart.tsx` — same pattern as SmokeChart.

- **Modify** `ui/src/HttpChart.tsx` — same pattern.

Verification: `cd ui && npx tsc --noEmit` after each task. Manual browser test at the end.

---

## Task 1: Parse `z0`/`z1` from URL at mount

**Files:**
- Modify: `ui/src/App.tsx:26-49` (`UrlState` type and `readUrlState` function)

- [ ] **Step 1: Extend `UrlState` and `readUrlState`**

Replace the existing `UrlState` type and `readUrlState` function in `ui/src/App.tsx` (lines 26-49):

```ts
type UrlState = {
  target: string | null;
  range: Range | null;
  mode: ChartStyle | null;
  source: string | null;
  pickedSec: number | null;
  zoom: { from: number; to: number } | null;
};
function readUrlState(): UrlState {
  if (typeof window === "undefined") {
    return { target: null, range: null, mode: null, source: null, pickedSec: null, zoom: null };
  }
  const p = new URLSearchParams(window.location.search);
  const range = p.get("range") as Range | null;
  const mode = p.get("mode");
  const tRaw = p.get("t");
  const t = tRaw ? Number(tRaw) : NaN;
  const z0Raw = p.get("z0");
  const z1Raw = p.get("z1");
  const z0 = z0Raw ? Number(z0Raw) : NaN;
  const z1 = z1Raw ? Number(z1Raw) : NaN;
  const zoom =
    Number.isFinite(z0) && Number.isFinite(z1) && z1 > z0
      ? { from: z0, to: z1 }
      : null;
  return {
    target: p.get("target"),
    range: range && VALID_RANGES.includes(range) ? range : null,
    mode: mode === "bars" || mode === "band" ? mode : null,
    source: p.get("source"),
    pickedSec: Number.isFinite(t) ? t : null,
    zoom,
  };
}
```

- [ ] **Step 2: Type-check**

Run: `cd ui && npx tsc --noEmit`
Expected: no errors (the new field is added but not yet consumed — TS is fine with that).

- [ ] **Step 3: Commit**

```bash
git add ui/src/App.tsx
git commit -m "Parse z0/z1 zoom params from URL at mount"
```

---

## Task 2: Add `zoom` state and derive window args

**Files:**
- Modify: `ui/src/App.tsx` — near existing state hooks (around line 77-107), and the fetch effect (around line 150-183)

- [ ] **Step 1: Add `zoom` state**

Find the `pickedSec` state line (around line 106):

```ts
  const [pickedSec, setPickedSec] = useState<number | null>(initialUrl.pickedSec);
```

Add a new state line directly after it:

```ts
  const [zoom, setZoom] = useState<{ from: number; to: number } | null>(initialUrl.zoom);
```

- [ ] **Step 2: Derive window args at component scope**

Find the fetch effect (starts around line 150 with `useEffect(() => { if (!selectedId) return;`). Directly ABOVE that effect, add two derived values at component-body scope so both the effect and downstream JSX can read them:

```ts
  const fromArg = zoom ? String(zoom.from) : range;
  const toArg = zoom ? String(zoom.to) : undefined;
```

Then replace the fetch effect body so it becomes:

```ts
  useEffect(() => {
    if (!selectedId) return;
    const zoomKey = zoom ? `${zoom.from}-${zoom.to}` : "";
    const key = `${selectedId}|${range}|${resolution}|${selectedSource ?? ""}|${zoomKey}`;
    const prevKey = fetchKeyRef.current;
    const isKeyChange = prevKey !== key;
    fetchKeyRef.current = key;
    setError(null);
    if (isKeyChange) {
      setCycles(null);
      if (prevKey !== "") setPickedSec(null);
    }
    setRefreshing(true);
    let cancelled = false;
    getCycles(selectedId, fromArg, toArg, resolution, selectedSource ?? undefined)
      .then((c) => {
        if (!cancelled) setCycles(c);
      })
      .catch((e) => {
        if (!cancelled) {
          setError(String(e));
          setCycles(null);
        }
      })
      .finally(() => {
        if (!cancelled) setRefreshing(false);
      });
    return () => {
      cancelled = true;
    };
  }, [selectedId, range, resolution, refreshTick, selectedSource, zoom]);
```

Key changes:
- `fromArg`/`toArg` computed at component body (used here + by `<HttpChart>` in Task 6).
- `zoomKey` added to the fetch-key so zoom-vs-range transitions behave as a key change (clears chart, resets pickedSec on subsequent navigations).
- `zoom` added to the dependency array. `fromArg`/`toArg` are pure derivatives of `range` and `zoom`, both of which are listed, so no missing-deps lint issue in practice.

- [ ] **Step 3: Clear zoom on target switch**

Find the `listTargets()` effect (around line 134-148). The `setSelectedId` callback inside it runs on mount after the first target list lands. This effect does NOT need changes — it sets a target for the first time but clearing zoom there would wipe a valid `z0`/`z1` from the URL before the first fetch. Clearing happens on *user* target changes instead, in `pickTarget` (around line 272-275). Replace that function:

```ts
  const pickTarget = (id: string) => {
    setSelectedId(id);
    setZoom(null);
    setPickedSec(null);
    setSidebarOpen(false);
  };
```

- [ ] **Step 4: Clear zoom on range button click**

Find the `RANGES.filter(...).map((r) => ( <button ... onClick={() => setRange(r.value)}>...` block (around line 345-355). Replace the `onClick` with:

```tsx
                  onClick={() => {
                    setRange(r.value);
                    setZoom(null);
                  }}
```

- [ ] **Step 5: Extend URL-sync effect to write `z0`/`z1`**

Find the URL-sync effect (around line 196-209). Replace the body so the effect becomes:

```ts
  useEffect(() => {
    if (typeof window === "undefined") return;
    const p = new URLSearchParams();
    if (selectedId) p.set("target", selectedId);
    if (range !== "-24h") p.set("range", range);
    if (chartStyle !== "band") p.set("mode", chartStyle);
    if (selectedSource) p.set("source", selectedSource);
    if (pickedSec != null) p.set("t", String(pickedSec));
    if (zoom) {
      p.set("z0", String(zoom.from));
      p.set("z1", String(zoom.to));
    }
    const qs = p.toString();
    const url = `${window.location.pathname}${qs ? `?${qs}` : ""}${window.location.hash}`;
    if (url !== `${window.location.pathname}${window.location.search}${window.location.hash}`) {
      window.history.replaceState(null, "", url);
    }
  }, [selectedId, range, chartStyle, selectedSource, pickedSec, zoom]);
```

- [ ] **Step 6: Type-check**

Run: `cd ui && npx tsc --noEmit`
Expected: no errors. `zoom` is now used; the fetch effect has a new dep.

- [ ] **Step 7: Commit**

```bash
git add ui/src/App.tsx
git commit -m "Add zoom state and route absolute window through fetches"
```

---

## Task 3: Add "reset zoom" toolbar button

**Files:**
- Modify: `ui/src/App.tsx` — toolbar block (around line 330-401)

- [ ] **Step 1: Add the reset-zoom button**

Find the toolbar block. The refresh button + auto checkbox live at the end. Insert a reset-zoom button between the chart-style buttons and the refresh button. Locate the `<button onClick={refresh} ...>` line (around line 375), and add this directly BEFORE it:

```tsx
              {zoom && (
                <button
                  onClick={() => setZoom(null)}
                  title="Reset zoom to selected range"
                >
                  reset zoom
                </button>
              )}
```

- [ ] **Step 2: Type-check**

Run: `cd ui && npx tsc --noEmit`
Expected: no errors.

- [ ] **Step 3: Commit**

```bash
git add ui/src/App.tsx
git commit -m "Add reset-zoom button, shown only when zoomed"
```

---

## Task 4: SmokeChart — report user drag-zoom

**Files:**
- Modify: `ui/src/SmokeChart.tsx`

- [ ] **Step 1: Add `onZoomChange` prop and ref**

Find the `Props` interface at the top of `ui/src/SmokeChart.tsx` (around line 6-12). Replace it with:

```ts
interface Props {
  points: CyclePoint[];
  height?: number;
  fromSec?: number;
  toSec?: number;
  onCyclePick?: (timeSec: number) => void;
  onZoomChange?: (window: { from: number; to: number } | null) => void;
}
```

Find the function signature (around line 22):

```ts
export function SmokeChart({ points, height = 320, fromSec, toSec, onCyclePick }: Props) {
```

Replace with:

```ts
export function SmokeChart({ points, height = 320, fromSec, toSec, onCyclePick, onZoomChange }: Props) {
```

Find the existing `onCyclePickRef` lines (around line 25-26). Add the `onZoomChange` ref directly after them:

```ts
  const onCyclePickRef = useRef(onCyclePick);
  onCyclePickRef.current = onCyclePick;
  const onZoomChangeRef = useRef(onZoomChange);
  onZoomChangeRef.current = onZoomChange;
```

- [ ] **Step 2: Add `internalScaleRef` and `setScale` hook**

Find the `useEffect` that creates uPlot (around line 45). Just inside the effect, above `const opts: Options = {`, add the internal-scale ref declaration is NOT needed here — refs must live at the component top so they survive re-mounts. Instead, go back near the `onZoomChangeRef` declaration and add:

```ts
  const internalScaleRef = useRef(false);
```

Now, in the `opts` object passed to `new uPlot(...)`, find the `hooks` block (around line 69-77). Replace the entire `hooks` object with:

```ts
      hooks: {
        setCursor: [
          (u) => {
            const next = u.cursor.idx ?? null;
            setCursorIdx((prev) => (prev === next ? prev : next));
          },
        ],
        setScale: [
          (u, key) => {
            if (key !== "x") return;
            if (internalScaleRef.current) return;
            const min = u.scales.x.min;
            const max = u.scales.x.max;
            if (min == null || max == null) return;
            const xs = u.data[0] as number[] | undefined;
            if (!xs || xs.length === 0) return;
            const from = Math.floor(min);
            const to = Math.ceil(max);
            const dataFrom = xs[0];
            const dataTo = xs[xs.length - 1];
            if (from <= dataFrom && to >= dataTo) onZoomChangeRef.current?.(null);
            else onZoomChangeRef.current?.({ from, to });
          },
        ],
      },
```

- [ ] **Step 3: Guard the programmatic `setScale` with the flag**

Find the pin effect (around line 116-128). Replace the `u.batch(...)` body so the effect becomes:

```ts
  const pinRef = useRef<{ from?: number; to?: number }>({});
  useEffect(() => {
    const u = plotRef.current;
    if (!u) return;
    const pinChanged =
      pinRef.current.from !== fromSec || pinRef.current.to !== toSec;
    pinRef.current = { from: fromSec, to: toSec };
    const pin = pinChanged && fromSec != null && toSec != null;
    u.batch(() => {
      if (pin) {
        internalScaleRef.current = true;
        u.setScale("x", { min: fromSec, max: toSec });
        internalScaleRef.current = false;
      }
      u.setData(built.data, false);
    });
  }, [built, fromSec, toSec]);
```

- [ ] **Step 4: Wire SmokeChart's zoom callback from App**

Switch to `ui/src/App.tsx`. Find the `<SmokeChart ... />` usage (around line 452-458). Replace with:

```tsx
                  <SmokeChart
                    points={points}
                    fromSec={fromSec}
                    toSec={toSec}
                    onCyclePick={setPickedSec}
                    onZoomChange={setZoom}
                  />
```

- [ ] **Step 5: Type-check**

Run: `cd ui && npx tsc --noEmit`
Expected: no errors.

- [ ] **Step 6: Commit**

```bash
git add ui/src/SmokeChart.tsx ui/src/App.tsx
git commit -m "SmokeChart: report drag-zoom via setScale hook"
```

---

## Task 5: SmokeBarChart — report user drag-zoom

**Files:**
- Modify: `ui/src/SmokeBarChart.tsx`

- [ ] **Step 1: Add `onZoomChange` prop and ref**

Find the `Props` interface (around line 8-19). Replace it with:

```ts
interface Props {
  points: CyclePoint[];
  height?: number;
  fromSec?: number;
  toSec?: number;
  onCyclePick?: (timeSec: number) => void;
  onZoomChange?: (window: { from: number; to: number } | null) => void;
}
```

Find the function signature (around line 42):

```ts
export function SmokeBarChart({ points, height = 320, fromSec, toSec, onCyclePick }: Props) {
```

Replace with:

```ts
export function SmokeBarChart({ points, height = 320, fromSec, toSec, onCyclePick, onZoomChange }: Props) {
```

Find the `onCyclePickRef` lines (around line 47-48). Add directly after:

```ts
  const onCyclePickRef = useRef(onCyclePick);
  onCyclePickRef.current = onCyclePick;
  const onZoomChangeRef = useRef(onZoomChange);
  onZoomChangeRef.current = onZoomChange;
```

- [ ] **Step 2: Add `internalScaleRef`**

Find the `yRangeRef` declaration (around line 54). Add directly after:

```ts
  const internalScaleRef = useRef(false);
```

- [ ] **Step 3: Add `setScale` hook**

Find the `hooks` block inside `opts` (around line 115-139). Replace the entire `hooks` object with:

```ts
      hooks: {
        draw: [
          (u) => {
            const stacks = stacksRef.current;
            if (stacks.length === 0) return;
            const ctx = u.ctx;
            ctx.save();
            ctx.beginPath();
            ctx.rect(u.bbox.left, u.bbox.top, u.bbox.width, u.bbox.height);
            ctx.clip();
            for (const stack of stacks) {
              drawStack(u, ctx, stack, hiddenRef.current);
            }
            ctx.restore();
          },
        ],
        setCursor: [
          (u) => {
            const next = u.cursor.idx ?? null;
            setCursorIdx((prev) => (prev === next ? prev : next));
          },
        ],
        setScale: [
          (u, key) => {
            if (key !== "x") return;
            if (internalScaleRef.current) return;
            const min = u.scales.x.min;
            const max = u.scales.x.max;
            if (min == null || max == null) return;
            const xs = u.data[0] as number[] | undefined;
            if (!xs || xs.length === 0) return;
            const from = Math.floor(min);
            const to = Math.ceil(max);
            const dataFrom = xs[0];
            const dataTo = xs[xs.length - 1];
            if (from <= dataFrom && to >= dataTo) onZoomChangeRef.current?.(null);
            else onZoomChangeRef.current?.({ from, to });
          },
        ],
      },
```

- [ ] **Step 4: Guard the programmatic `setScale` with the flag**

Find the pin effect (around line 191-213). Replace the `u.batch(...)` body so the effect becomes:

```ts
  const pinRef = useRef<{ from?: number; to?: number }>({});
  useEffect(() => {
    const u = plotRef.current;
    if (!u) return;
    const pinChanged =
      pinRef.current.from !== fromSec || pinRef.current.to !== toSec;
    pinRef.current = { from: fromSec, to: toSec };
    const pin = pinChanged && fromSec != null && toSec != null;
    const empty = built.stacks.length === 0;

    stacksRef.current = empty ? [] : built.stacks;
    yRangeRef.current = empty ? [0, 1] : built.yRange;

    u.batch(() => {
      if (pin) {
        internalScaleRef.current = true;
        u.setScale("x", { min: fromSec, max: toSec });
        internalScaleRef.current = false;
      }
      u.setData(built.data, false);
    });
    if (!empty) {
      u.redraw(false, true);
    }
  }, [built, fromSec, toSec]);
```

- [ ] **Step 5: Wire SmokeBarChart's zoom callback from App**

Switch to `ui/src/App.tsx`. Find the `<SmokeBarChart ... />` usage (around line 459-465). Replace with:

```tsx
                  <SmokeBarChart
                    points={points}
                    fromSec={fromSec}
                    toSec={toSec}
                    onCyclePick={setPickedSec}
                    onZoomChange={setZoom}
                  />
```

- [ ] **Step 6: Type-check**

Run: `cd ui && npx tsc --noEmit`
Expected: no errors.

- [ ] **Step 7: Commit**

```bash
git add ui/src/SmokeBarChart.tsx ui/src/App.tsx
git commit -m "SmokeBarChart: report drag-zoom via setScale hook"
```

---

## Task 6: HttpChart — report user drag-zoom

**Files:**
- Modify: `ui/src/HttpChart.tsx`

- [ ] **Step 1: Add `onZoomChange` prop and ref**

Find the `Props` interface (around line 6-14). Replace it with:

```ts
interface Props {
  targetId: string;
  range: string;
  refreshTick: number;
  height?: number;
  fromSec?: number;
  toSec?: number;
  source?: string;
  onZoomChange?: (window: { from: number; to: number } | null) => void;
}
```

Find the function signature (around line 35-43). Replace with:

```ts
export function HttpChart({
  targetId,
  range,
  refreshTick,
  height = 260,
  fromSec,
  toSec,
  source,
  onZoomChange,
}: Props) {
```

Find the existing `paletteRef` / `pointsRef` declarations (around line 60-63). Add directly after:

```ts
  const onZoomChangeRef = useRef(onZoomChange);
  onZoomChangeRef.current = onZoomChange;
  const internalScaleRef = useRef(false);
```

- [ ] **Step 2: Add `setScale` hook**

Find the `hooks` block inside `opts` (around line 116-153). The existing block only has a `draw` hook. Replace the entire `hooks` object with:

```ts
      hooks: {
        draw: [
          (u) => {
            const ctx = u.ctx;
            const xs = u.data[0] as number[];
            const ys = u.data[1] as number[];
            const sts = u.data[2] as number[];
            const pts = pointsRef.current;
            const pal = paletteRef.current;
            if (xs.length === 0) return;
            const barW = 3;
            const capH = 3;
            ctx.save();
            ctx.beginPath();
            ctx.rect(u.bbox.left, u.bbox.top, u.bbox.width, u.bbox.height);
            ctx.clip();
            for (let i = 0; i < xs.length; i++) {
              const x = u.valToPos(xs[i], "x", true);
              const y = u.valToPos(ys[i], "y", true);
              const y0 = u.valToPos(0, "y", true);
              const src = pts[i]?.Source ?? "";
              const body = pal.get(src)?.stroke ?? colorFor(sts[i]);
              ctx.fillStyle = body;
              ctx.fillRect(x - barW / 2, y, barW, y0 - y);
              ctx.fillStyle = colorFor(sts[i]);
              ctx.fillRect(x - barW / 2, y, barW, Math.min(capH, y0 - y));
            }
            ctx.restore();
          },
        ],
        setScale: [
          (u, key) => {
            if (key !== "x") return;
            if (internalScaleRef.current) return;
            const min = u.scales.x.min;
            const max = u.scales.x.max;
            if (min == null || max == null) return;
            const xs = u.data[0] as number[] | undefined;
            if (!xs || xs.length === 0) return;
            const from = Math.floor(min);
            const to = Math.ceil(max);
            const dataFrom = xs[0];
            const dataTo = xs[xs.length - 1];
            if (from <= dataFrom && to >= dataTo) onZoomChangeRef.current?.(null);
            else onZoomChangeRef.current?.({ from, to });
          },
        ],
      },
```

- [ ] **Step 3: Guard the programmatic `setScale` with the flag**

Find the pin effect (around line 177-204). Replace the `u.batch(...)` body so the effect becomes:

```ts
  const pinRef = useRef<{ from?: number; to?: number }>({});
  useEffect(() => {
    const u = plotRef.current;
    if (!u) return;
    const pinChanged =
      pinRef.current.from !== fromSec || pinRef.current.to !== toSec;
    pinRef.current = { from: fromSec, to: toSec };
    const pin = pinChanged && fromSec != null && toSec != null;
    let data: AlignedData;
    if (points.length === 0) {
      data = [[], [], []];
    } else {
      const ts = points.map((p) => Math.floor(new Date(p.Time).getTime() / 1000));
      let rttMax = 1;
      for (const p of points) if (p.RTT > rttMax) rttMax = p.RTT;
      const failHeight = Math.max(rttMax * 0.15, 1);
      const rtts = points.map((p) => (p.Status === 0 ? failHeight : p.RTT));
      const statuses = points.map((p) => p.Status);
      data = [ts, rtts, statuses];
    }
    u.batch(() => {
      if (pin) {
        internalScaleRef.current = true;
        u.setScale("x", { min: fromSec, max: toSec });
        internalScaleRef.current = false;
      }
      u.setData(data, false);
    });
  }, [points, fromSec, toSec]);
```

- [ ] **Step 4: Wire HttpChart's zoom callback from App**

Switch to `ui/src/App.tsx`. Find the `<HttpChart ... />` usage (around line 436-446). Replace with:

```tsx
                <HttpChart
                  targetId={selected.id}
                  range={range}
                  refreshTick={refreshTick}
                  fromSec={fromSec}
                  toSec={toSec}
                  source={sourceParam}
                  onZoomChange={setZoom}
                />
```

- [ ] **Step 5: HttpChart — route its own fetch through the zoom window**

`HttpChart` does its own fetch (`getHttpSamples`). It currently only gets `range`, not the absolute bounds. Extend its Props so it can also receive `fromArg`/`toArg` from App.

Still in `ui/src/HttpChart.tsx`, revise the Props interface (defined in Step 1 above) to also include `fromArg`/`toArg`:

```ts
interface Props {
  targetId: string;
  range: string;
  refreshTick: number;
  height?: number;
  fromSec?: number;
  toSec?: number;
  source?: string;
  onZoomChange?: (window: { from: number; to: number } | null) => void;
  // Absolute window override. When set, supersedes `range` for the fetch.
  fromArg?: string;
  toArg?: string;
}
```

Update the destructure accordingly:

```ts
export function HttpChart({
  targetId,
  range,
  refreshTick,
  height = 260,
  fromSec,
  toSec,
  source,
  onZoomChange,
  fromArg,
  toArg,
}: Props) {
```

Find the fetch effect (around line 65-81). Replace the `getHttpSamples(...)` call with:

```ts
    getHttpSamples(targetId, fromArg ?? range, toArg, source)
      .then((r) => {
        if (!cancelled) setPoints(r.points ?? []);
      })
      .catch((e) => {
        if (!cancelled) {
          setError(String(e));
          setPoints([]);
        }
      });
```

And add `fromArg`, `toArg` to the effect's dep array:

```ts
  }, [targetId, range, refreshTick, source, fromArg, toArg]);
```

Back in `ui/src/App.tsx` — `fromArg`/`toArg` are already declared at component-body scope from Task 2. Pass them into `<HttpChart>`:

```tsx
                <HttpChart
                  targetId={selected.id}
                  range={range}
                  refreshTick={refreshTick}
                  fromSec={fromSec}
                  toSec={toSec}
                  source={sourceParam}
                  onZoomChange={setZoom}
                  fromArg={fromArg}
                  toArg={toArg}
                />
```

- [ ] **Step 6: Type-check**

Run: `cd ui && npx tsc --noEmit`
Expected: no errors.

- [ ] **Step 7: Commit**

```bash
git add ui/src/HttpChart.tsx ui/src/App.tsx
git commit -m "HttpChart: report drag-zoom and route fetch through zoom window"
```

---

## Task 7: Manual verification in the browser

**Files:** none

- [ ] **Step 1: Build the UI**

Run: `cd ui && npm run build`
Expected: `dist/` populated without errors.

- [ ] **Step 2: Build the binary**

Run: `make build`
Expected: `./gosmokeping` binary built.

- [ ] **Step 3: Start the dev vite server for hot iteration**

Run in one terminal: `cd ui && npm run dev`
Run in another terminal: `./gosmokeping -config config.json` (or whichever dev config is set up).

Open `http://localhost:5173/` — the page should proxy `/api` to the Go backend.

- [ ] **Step 4: Latency chart — drag-zoom in band mode**

- Pick a target with data for the last 7d.
- Click the `7d` range button.
- Drag-select a narrow window (e.g., a 30-minute slice) on the band chart.
- Expected:
  - URL gets `?target=…&range=-7d&z0=<unix>&z1=<unix>`.
  - Chart shows only that window.
  - "Latency — X resolution" title downgrades to `raw` (or `1h` depending on window width).
  - Reset-zoom button appears in the toolbar.
  - MTR heatmap below narrows to the same window (for ICMP/MTR targets).

- [ ] **Step 5: Auto-refresh preserves zoom**

- With a zoom active, wait at least one auto-refresh tick (30s).
- Expected: viewport does NOT jump; chart re-renders in place when new data arrives.

- [ ] **Step 6: Share the URL**

- Copy the URL with `z0`/`z1` into a fresh browser tab.
- Expected: loads directly at the zoom window, same data as the source tab.

- [ ] **Step 7: Double-click reset**

- Double-click the chart.
- Expected: URL loses `z0`/`z1`, reset-zoom button disappears, view returns to the `range`-selector window.

- [ ] **Step 8: Range button clears zoom**

- Drag-zoom to a narrow slice.
- Click a different range button (e.g., `24h`).
- Expected: zoom cleared, URL loses `z0`/`z1`, view shows the new range.

- [ ] **Step 9: Repeat for bar mode**

- Switch to `bars`.
- Drag-zoom, verify URL updates, reset via button + range button.

- [ ] **Step 10: Repeat for HTTP targets**

- Pick an HTTP target.
- Drag-zoom on the HTTP chart.
- Verify URL gets `z0`/`z1`; verify fresh tab opens at the same zoom.

- [ ] **Step 11: Out-of-retention zoom**

- Hand-craft a URL with `z0`/`z1` far in the past (e.g., a year ago for a target newer than that).
- Expected: "No data in range" empty state, no errors in console.

- [ ] **Step 12: Commit any follow-up fixes discovered**

If any of the above steps revealed an issue, fix it and commit.
