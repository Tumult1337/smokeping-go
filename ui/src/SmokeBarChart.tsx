import React, { useEffect, useMemo, useRef, useState } from "react";
import uPlot, { type Options, type AlignedData, type Series } from "uplot";
import type { CyclePoint } from "./api";
import { PALETTE, lossColor } from "./palette";
import { LossStripCanvas, type LossSeries } from "./LossStrip";
import { effectiveMin, sourcesKey as sourcesKeyOf, unixSec, windowLoss } from "./chartUtils";

const BAR_PCT_LABELS = ["min", "p5", "p25", "median", "p75", "p95", "max", "loss"] as const;

interface Props {
  points: CyclePoint[];
  height?: number;
  // Requested window (unix seconds). When set, the x-axis is pinned to
  // [fromSec, toSec] so sparse data doesn't visually collapse the window —
  // clicking 1y vs 30d is otherwise indistinguishable when coverage is thin.
  fromSec?: number;
  toSec?: number;
  // "log" switches the y-axis to log10. Useful when a stable target's
  // signal coexists with rare spikes that compress linear autoscale.
  yScale?: "lin" | "log";
  // Invoked when the user clicks a bar; receives that cycle's unix timestamp.
  // Used by the MTR cycle-picker to swap the HopsTable to that moment.
  onCyclePick?: (timeSec: number) => void;
  onZoomChange?: (window: { from: number; to: number } | null) => void;
  onSoloChange?: (source: string | null) => void;
  loading?: boolean;
}

// uPlot log scales reject ≤ 0; cycles may store min=0 for sub-millisecond
// replies. Floor at 0.01ms so the band has somewhere to land.
const LOG_Y_FLOOR = 0.01;

type Band = { lo: number; hi: number; alpha: number };

// One source's worth of drawable state. The "all" view pushes one of these
// per source into barsRef so the draw hook can paint each stack with its own
// palette without cross-contaminating widths — each source's bar width is
// derived from its own sample cadence, not the global union.
type SourceStack = {
  ts: number[];
  bands: Band[][];
  medians: number[];
  losses: number[];
  fill: (alpha: number) => string;
  medianColor: string;
};

// Classic SmokePing / cocopacket-style rendering: for each cycle we paint a
// column the full width of its slot (bars touch, no gaps) and stack symmetric
// percentile pairs every 5% as translucent bands — they accumulate into a
// smooth smoke gradient that darkens around the median. The median tick on
// top is colour-coded by per-cycle loss percentage. In multi-source "all"
// view, each source gets its own palette entry and is drawn independently.
export function SmokeBarChart({ points, height = 320, fromSec, toSec, yScale = "lin", onCyclePick, onZoomChange, onSoloChange, loading }: Props) {
  const divRef = useRef<HTMLDivElement | null>(null);
  const plotRef = useRef<uPlot | null>(null);
  // Keep onCyclePick in a ref so swapping the callback doesn't force a full
  // chart rebuild (which would flash + lose hover state).
  const onCyclePickRef = useRef(onCyclePick);
  onCyclePickRef.current = onCyclePick;
  const onZoomChangeRef = useRef(onZoomChange);
  onZoomChangeRef.current = onZoomChange;
  const onSoloChangeRef = useRef(onSoloChange);
  onSoloChangeRef.current = onSoloChange;
  const internalScaleRef = useRef(false);
  // Track the requested window so the setScale hook can distinguish a user
  // zoom gesture from uPlot re-applying the pinned range after data refresh.
  // Data extent is the wrong yardstick when probes are sparse within the pin.
  const requestedWindowRef = useRef<{ from?: number; to?: number }>({});
  requestedWindowRef.current = { from: fromSec, to: toSec };

  // All data that the draw hook and scale-range callback read from live in
  // refs — the uPlot instance is created once per source set, so closures
  // captured at construction time would go stale after each setData.
  const stacksRef = useRef<SourceStack[]>([]);
  const yRangeRef = useRef<[number, number]>([0, 1]);
  // Hidden-label set the draw hook consults when deciding whether to paint
  // each band + median. Kept in a ref so the draw closure (captured at uPlot
  // construction) always reads the current value.
  const hiddenRef = useRef<Set<string>>(new Set());
  // Index (into stacks/sources) of the soloed source, or null for all.
  const soloIdxRef = useRef<number | null>(null);
  const soloSourceRef = useRef<string | null>(null);

  const built = useMemo(() => buildSources(points), [points]);
  // Left/right gutter of the uPlot plot area in CSS px, tracked from u.bbox
  // so the LossStripCanvas canvas covers exactly the same x range as the chart.
  const [plotOffsets, setPlotOffsets] = useState({ left: 34, right: 0 });
  // Stable signature of the source set; a change forces a uPlot rebuild
  // because series topology depends on the source count.
  const sourcesKey = sourcesKeyOf(built.sources);

  const [cursorIdx, setCursorIdx] = useState<number | null>(null);
  const [hidden, setHidden] = useState<Set<string>>(new Set());
  const [soloSource, setSoloSource] = useState<string | null>(null);
  useEffect(() => {
    setHidden(new Set());
    setSoloSource(null);
    onSoloChangeRef.current?.(null);
  }, [sourcesKey]);
  useEffect(() => {
    hiddenRef.current = hidden;
    plotRef.current?.redraw(false, true);
  }, [hidden]);
  useEffect(() => {
    soloSourceRef.current = soloSource;
    soloIdxRef.current = soloSource != null ? built.sources.indexOf(soloSource) : null;
    const raw = soloSource != null
      ? (built.sourceYRanges.get(soloSource) ?? built.yRange)
      : built.yRange;
    const range = clampRangeForScale(raw, yScale);
    yRangeRef.current = range;
    const u = plotRef.current;
    if (u) {
      u.setScale("y", { min: range[0], max: range[1] });
      u.redraw(false, true);
    }
  }, [soloSource, built.sources, built.yRange, built.sourceYRanges, yScale]);

  useEffect(() => {
    if (!divRef.current) return;

    // Built-in uPlot series are still required so data columns stay bound;
    // labels are only used by the (hidden) internal legend so they can stay
    // dumb placeholders.
    const series: Series[] = [{}];
    built.sources.forEach((name) => {
      const mk = (label: string): Series => ({
        label: `${name}/${label}`,
        stroke: "transparent",
        points: { show: false },
      });
      for (const label of BAR_PCT_LABELS) series.push(mk(label));
    });

    const opts: Options = {
      width: divRef.current.clientWidth,
      height,
      scales: {
        x: { time: true },
        y: yScale === "log"
          ? { auto: false, distr: 3, log: 10, range: () => yRangeRef.current }
          : { auto: false, range: () => yRangeRef.current },
      },
      axes: [
        { stroke: "#8a93a6", grid: { stroke: "#1f2430" } },
        {
          stroke: "#8a93a6",
          grid: { stroke: "#1f2430" },
          label: "ms",
          labelSize: 30,
          // Default log splits land on every 2/3/5/7 inside each decade;
          // collapse to one tick per decade so the grid reads cleanly.
          // Linear mode keeps uPlot's default linear-distance splits.
          ...(yScale === "log" ? { splits: decadeSplits } : {}),
        },
      ],
      series,
      legend: { show: false },
      cursor: {
        points: { show: false },
        // Keep the vertical x-hair; y-hair off to stay out of the smoke.
        y: false,
        // Disable uPlot's default dblclick (resets scales to data extent) so
        // our own dblclick listener owns the gesture without the stock reset
        // firing first and pushing a bogus range through the setScale hook.
        bind: { dblclick: () => null },
      },
      hooks: {
        draw: [
          (u) => {
            const dpr = devicePixelRatio || 1;
            const left = Math.round(u.bbox.left / dpr);
            const right = Math.round(u.width - (u.bbox.left + u.bbox.width) / dpr);
            setPlotOffsets((prev) => (prev.left === left && prev.right === right ? prev : { left, right }));
            const stacks = stacksRef.current;
            if (stacks.length === 0) return;
            const soloIdx = soloIdxRef.current;
            const ctx = u.ctx;
            ctx.save();
            // Clip to the plot area so bars near the edges don't spill into
            // the axis gutter when the user drag-zooms into a narrow window.
            ctx.beginPath();
            ctx.rect(u.bbox.left, u.bbox.top, u.bbox.width, u.bbox.height);
            ctx.clip();
            const laneCount = soloIdx != null ? 1 : stacks.length;
            let lane = 0;
            for (let si = 0; si < stacks.length; si++) {
              if (soloIdx != null && si !== soloIdx) continue;
              drawStack(u, ctx, stacks[si], hiddenRef.current, lane++, laneCount);
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
            const from = Math.floor(min);
            const to = Math.ceil(max);
            const reqFrom = requestedWindowRef.current.from;
            const reqTo = requestedWindowRef.current.to;
            if (reqFrom == null || reqTo == null) return;
            if (Math.abs(from - reqFrom) <= 1 && Math.abs(to - reqTo) <= 1) return;
            onZoomChangeRef.current?.({ from, to });
          },
        ],
      },
    };

    // Empty columns per series so setData can grow without reshuffling.
    const empty: AlignedData = [[], ...series.slice(1).map(() => [] as number[])] as AlignedData;
    plotRef.current = new uPlot(opts, empty, divRef.current);

    // Click-to-pick: walk every source's own ts array and pick the sample
    // closest to the cursor's x value (in data space). The union-based
    // cursor.idx doesn't help here because each source owns its own index.
    const over = plotRef.current.over;
    // Drag-zoom release fires click on the same element; track mousedown coords
    // so we can suppress the cycle-pick when the user was actually drag-zooming.
    let dragStart: { x: number; y: number } | null = null;
    // Defer single-click cycle-pick so dblclick (zoom reset) can cancel it.
    let pendingClick: number | null = null;
    const onMouseDown = (e: MouseEvent) => {
      dragStart = { x: e.clientX, y: e.clientY };
    };
    const onClick = (e: MouseEvent) => {
      const u = plotRef.current;
      const cb = onCyclePickRef.current;
      if (!u || !cb) return;
      if (dragStart) {
        const dx = Math.abs(e.clientX - dragStart.x);
        const dy = Math.abs(e.clientY - dragStart.y);
        dragStart = null;
        if (dx > 3 || dy > 3) return;
      }
      const xVal = u.posToVal(u.cursor.left ?? -1, "x");
      if (!isFinite(xVal)) return;
      let best: number | null = null;
      let bestDist = Infinity;
      for (const stack of stacksRef.current) {
        for (const t of stack.ts) {
          const d = Math.abs(t - xVal);
          if (d < bestDist) {
            bestDist = d;
            best = t;
          }
        }
      }
      if (best == null) return;
      const picked = best;
      if (pendingClick != null) window.clearTimeout(pendingClick);
      pendingClick = window.setTimeout(() => {
        pendingClick = null;
        cb(picked);
      }, 200);
    };
    const onDblClick = () => {
      if (pendingClick != null) {
        window.clearTimeout(pendingClick);
        pendingClick = null;
      }
      onZoomChangeRef.current?.(null);
    };
    over.addEventListener("mousedown", onMouseDown);
    over.addEventListener("click", onClick);
    over.addEventListener("dblclick", onDblClick);
    const ro = new ResizeObserver(() => {
      if (plotRef.current && divRef.current) {
        plotRef.current.setSize({
          width: divRef.current.clientWidth,
          height,
        });
      }
    });
    ro.observe(divRef.current);
    return () => {
      ro.disconnect();
      if (pendingClick != null) window.clearTimeout(pendingClick);
      over.removeEventListener("mousedown", onMouseDown);
      over.removeEventListener("click", onClick);
      over.removeEventListener("dblclick", onDblClick);
      plotRef.current?.destroy();
      plotRef.current = null;
      // Clear the pin record so the setData effect re-pins the x scale on
      // the freshly-constructed uPlot. Without this, a yScale toggle (which
      // tears down + recreates uPlot but doesn't change fromSec/toSec)
      // leaves the new uPlot's x scale uninitialised — bars then draw at
      // valToPos returns of NaN/Infinity and stay invisible until the next
      // window change repaints.
      pinRef.current = {};
    };
    // sourcesKey rebuilds the chart when the set of sources changes; data-only
    // updates flow through the setData effect below. yScale is in deps because
    // uPlot reads `distr` once at construction — flipping lin↔log requires a
    // full rebuild.
  }, [height, sourcesKey, yScale]);

  // Pin the x scale only when the requested window changes. A plain data
  // refresh passes resetScales=false so user drag-zooms survive the tick.
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
    if (soloSourceRef.current === null) {
      yRangeRef.current = empty
        ? clampRangeForScale([0, 1], yScale)
        : clampRangeForScale(built.yRange, yScale);
    }

    u.batch(() => {
      if (pin) {
        internalScaleRef.current = true;
        u.setScale("x", { min: fromSec, max: toSec });
        internalScaleRef.current = false;
      }
      u.setData(built.data, false);
      // Force y to track the freshly-built range. uPlot only re-invokes the
      // `range: () => yRangeRef.current` callback when pendScales[y] gets set
      // to AUTOSCALE, which requires either sc.min==null (init only) or
      // sc.auto returning true. With auto:false, neither fires after the
      // first setScales, so without this explicit setScale the y axis stays
      // frozen at whatever yRangeRef was when uPlot was constructed —
      // [0, 1] when the first mount happened with empty data.
      if (!empty) {
        u.setScale("y", { min: yRangeRef.current[0], max: yRangeRef.current[1] });
      }
    });
    if (!empty) {
      // setData already triggers a redraw, but hooks.draw closes over refs we
      // just mutated — force another pass so the fresh stacks land.
      u.redraw(false, true);
    }
  }, [built, fromSec, toSec, yScale]);

  return (
    <div className="chart-host" style={{ minHeight: height }}>
      <div ref={divRef} style={{ width: "100%" }} />
      {points.length === 0 &&
        (loading ? (
          <div className="chart-skeleton" role="status" aria-label="Loading…" />
        ) : (
          <div className="chart-empty">No data in range</div>
        ))}
      {points.length > 0 && built.anyLoss && (
        <LossStripCanvas
          lossSeries={soloSource != null
            ? built.lossSeries.filter((_, i) => built.sources[i] === soloSource)
            : built.lossSeries}
          fromSec={fromSec}
          toSec={toSec}
          onCyclePick={onCyclePick}
          plotLeft={plotOffsets.left}
          plotRight={plotOffsets.right}
        />
      )}
      {points.length > 0 && (
        <BarChartLegend
          built={built}
          cursorIdx={cursorIdx}
          hidden={hidden}
          setHidden={setHidden}
          soloSource={soloSource}
          setSoloSource={setSoloSource}
          onSoloChangeRef={onSoloChangeRef}
        />
      )}
    </div>
  );
}

function BarChartLegend({
  built,
  cursorIdx,
  hidden,
  setHidden,
  soloSource,
  setSoloSource,
  onSoloChangeRef,
}: {
  built: Built;
  cursorIdx: number | null;
  hidden: Set<string>;
  setHidden: (updater: (prev: Set<string>) => Set<string>) => void;
  soloSource: string | null;
  setSoloSource: (updater: (prev: string | null) => string | null) => void;
  onSoloChangeRef: React.MutableRefObject<((src: string | null) => void) | undefined>;
}) {
  const toggle = (label: string) =>
    setHidden((prev) => {
      const next = new Set(prev);
      if (next.has(label)) next.delete(label);
      else next.add(label);
      return next;
    });

  return (
    <div className="smoke-legend">
      {built.sources.map((src, srcIdx) => {
        const palette = PALETTE[srcIdx % PALETTE.length];
        const multi = built.sources.length > 1;
        const dimmed = soloSource != null && src !== soloSource;
        const agg = built.aggregates[srcIdx];
        const aggVals: (number | null)[] = agg
          ? [agg.min, agg.p5, agg.p25, agg.median, agg.p75, agg.p95, agg.max, agg.loss]
          : BAR_PCT_LABELS.map(() => null);
        // Union x-axis offset for this source at the cursor position.
        const base = 1 + srcIdx * BAR_PCT_LABELS.length;
        return (
          <div className={`smoke-legend-row${dimmed ? " dimmed" : ""}`} key={src || `src-${srcIdx}`}>
            {multi ? (
              <button
                type="button"
                className="smoke-legend-name smoke-legend-name-btn"
                style={{ color: palette.text }}
                onClick={() => {
                  const next = soloSource === src ? null : src;
                  setSoloSource(() => next);
                  onSoloChangeRef.current?.(next);
                }}
                title={soloSource === src ? "Show all sources" : `Show only ${src || "—"}`}
              >
                {src || "—"}
              </button>
            ) : (
              <span
                className="smoke-legend-name"
                style={{ color: palette.text }}
              >
                {src || "—"}
              </span>
            )}
            <span
              className="smoke-legend-scope"
              title={
                cursorIdx == null
                  ? "Window: percentile columns are the mean of each cycle's percentile; min and max are true extrema."
                  : "One cycle's own percentiles."
              }
            >
              {cursorIdx == null ? "window" : "cycle"}
            </span>
            {BAR_PCT_LABELS.map((label, j) => {
              const col = built.data[base + j] as (number | null)[] | undefined;
              const cursorVal = cursorIdx != null && col ? col[cursorIdx] : null;
              const v = cursorVal != null ? cursorVal : aggVals[j];
              const txt =
                v == null
                  ? "—"
                  : label === "loss"
                  ? `${v.toFixed(1)}%`
                  : v.toFixed(1);
              const off = hidden.has(label);
              return (
                <button
                  type="button"
                  className={`smoke-legend-val${off ? " off" : ""}`}
                  key={label}
                  onClick={() => toggle(label)}
                >
                  {label}: <strong>{txt}</strong>
                </button>
              );
            })}
          </div>
        );
      })}
    </div>
  );
}

type SourceAgg = {
  min: number | null;
  p5: number | null;
  p25: number | null;
  median: number | null;
  p75: number | null;
  p95: number | null;
  max: number | null;
  loss: number | null;
};

type Built = {
  sources: string[];
  data: AlignedData;
  stacks: SourceStack[];
  yRange: [number, number];
  sourceYRanges: Map<string, [number, number]>;
  lossSeries: LossSeries[];
  anyLoss: boolean;
  aggregates: SourceAgg[];
};

function buildSources(points: CyclePoint[]): Built {
  if (points.length === 0) {
    return {
      sources: [],
      data: [[]],
      stacks: [],
      yRange: [0, 1],
      sourceYRanges: new Map(),
      lossSeries: [],
      anyLoss: false,
      aggregates: [],
    };
  }

  const bySource = new Map<string, { pts: CyclePoint[]; secs: number[] }>();
  for (const p of points) {
    const key = p.Source ?? "";
    let g = bySource.get(key);
    if (!g) bySource.set(key, (g = { pts: [], secs: [] }));
    g.pts.push(p);
    g.secs.push(unixSec(p.Time));
  }
  const sources = [...bySource.keys()].sort();

  // Union x-axis so the cursor can pick any source's sample. Each source's
  // values stay on its own index domain inside the stack; uPlot only uses
  // the union for cursor + legend alignment.
  const tsSet = new Set<number>();
  for (const [, g] of bySource) {
    for (const s of g.secs) tsSet.add(s);
  }
  const xs = [...tsSet].sort((a, b) => a - b);
  const xIdx = new Map<number, number>();
  xs.forEach((t, i) => xIdx.set(t, i));

  const data: (number | null)[][] = [xs];
  const stacks: SourceStack[] = [];
  const lossSeries: LossSeries[] = [];
  const aggregates: SourceAgg[] = [];
  let anyLoss = false;
  let yLo = Infinity;
  let yHi = -Infinity;
  const sourceYRanges = new Map<string, [number, number]>();

  sources.forEach((name, srcIdx) => {
    const palette = PALETTE[srcIdx % PALETTE.length];
    // Server order is the time order: every cycles query is ORDER BY
    // timestamp (raw) or bucket_ts, source (bucketed).
    const { pts, secs: ts } = bySource.get(name)!;

    // NaN signals "no valid RTT" (100%-loss cycle) to the draw hook so it
    // skips the median tick rather than drawing it at 0ms.
    const medians = pts.map((p) => p.LossPct >= 100 ? NaN : p.Median);
    const losses = pts.map((p) => p.LossPct);

    // 100%-loss cycles get no bands: their Median/Min/Max are all 0 artifacts.
    const bands: Band[][] = pts.map((p) => {
      if (p.LossPct >= 100) return [];
      const effMin = effectiveMin(p);
      const all: Band[] = [
        { lo: effMin, hi: p.Max, alpha: 0.07 },
        { lo: p.P5, hi: p.P95, alpha: 0.09 },
        { lo: p.P10, hi: p.P90, alpha: 0.11 },
        { lo: p.P15, hi: p.P85, alpha: 0.13 },
        { lo: p.P20, hi: p.P80, alpha: 0.15 },
        { lo: p.P25, hi: p.P75, alpha: 0.17 },
        { lo: p.P30, hi: p.P70, alpha: 0.20 },
        { lo: p.P35, hi: p.P65, alpha: 0.23 },
        { lo: p.P40, hi: p.P60, alpha: 0.26 },
        { lo: p.P45, hi: p.P55, alpha: 0.30 },
      ];
      return all.filter((b) => b.hi > b.lo);
    });

    for (const p of pts) {
      if (p.LossPct >= 100) continue;
      const lo = effectiveMin(p);
      if (lo > 0 && lo < yLo) yLo = lo;
      if (p.Max > yHi) yHi = p.Max;
    }

    // Per-source y range for solo rescale.
    let srcLo = Infinity, srcHi = -Infinity;
    for (const p of pts) {
      if (p.LossPct >= 100) continue;
      const lo = effectiveMin(p);
      if (lo > 0 && lo < srcLo) srcLo = lo;
      if (p.Max > srcHi) srcHi = p.Max;
    }
    if (isFinite(srcLo) && isFinite(srcHi)) {
      const srcPad = Math.max(1, (srcHi - srcLo) * 0.1);
      sourceYRanges.set(name, [Math.max(0, srcLo - srcPad), srcHi + srcPad]);
    }

    // Build 8 aligned columns on the union x-axis: min/p5/p25/median/p75/p95/
    // max/loss. Only legend readouts use this; the draw hook works directly
    // off the SourceStack instead. Unused slots are null so hover over a
    // neighbour's slot shows "—" for this source.
    const cols: (number | null)[][] = Array.from({ length: 8 }, () => xs.map(() => null));
    pts.forEach((p, i) => {
      const idx = xIdx.get(ts[i]);
      if (idx == null) return;
      cols[7][idx] = p.LossPct;
      // 100%-loss cycles have no valid RTT; leave the latency columns null so
      // hover reads "—" rather than presenting an outage as 0.0 ms latency.
      if (p.LossPct >= 100) return;
      cols[0][idx] = effectiveMin(p);
      cols[1][idx] = p.P5;
      cols[2][idx] = p.P25;
      cols[3][idx] = p.Median;
      cols[4][idx] = p.P75;
      cols[5][idx] = p.P95;
      cols[6][idx] = p.Max;
    });
    cols.forEach((c) => data.push(c));

    stacks.push({
      ts,
      bands,
      medians,
      losses,
      fill: palette.fill,
      // The loss-colour helper honours the palette stroke for the zero-loss
      // case; lossy cycles stay yellow/orange/red regardless of source so
      // outages are visually loud.
      medianColor: palette.stroke,
    });

    const hasLoss = losses.some((l) => l > 0);
    if (hasLoss) anyLoss = true;
    lossSeries.push({ source: name, ts, losses, hasLoss });

    const valid = pts.filter((p) => p.LossPct < 100);
    if (valid.length > 0) {
      // Mean of each per-cycle percentile, not a window percentile — averaging
      // p95s suppresses the isolated spikes a p95 exists to surface.
      const avg = (fn: (p: CyclePoint) => number) =>
        valid.reduce((s, p) => s + fn(p), 0) / valid.length;
      const mins = valid
        .map((p) => effectiveMin(p))
        .filter((v) => v > 0);
      aggregates.push({
        min: mins.length > 0 ? Math.min(...mins) : null,
        p5: avg((p) => p.P5),
        p25: avg((p) => p.P25),
        median: avg((p) => p.Median),
        p75: avg((p) => p.P75),
        p95: avg((p) => p.P95),
        max: valid.reduce((m, p) => (p.Max > m ? p.Max : m), -Infinity),
        loss: windowLoss(pts),
      });
    } else {
      aggregates.push({ min: null, p5: null, p25: null, median: null, p75: null, p95: null, max: null, loss: windowLoss(pts) });
    }
  });

  if (!isFinite(yLo) || !isFinite(yHi)) {
    yLo = 0;
    yHi = 1;
  }
  const yPad = Math.max(1, (yHi - yLo) * 0.1);

  return {
    sources,
    data: data as AlignedData,
    stacks,
    yRange: [Math.max(0, yLo - yPad), yHi + yPad],
    sourceYRanges,
    lossSeries,
    anyLoss,
    aggregates,
  };
}

// clampRangeForScale lifts the lower bound into log-safe territory when log
// mode is on. Linear keeps the natural [0, hi] band; log needs strictly > 0
// or uPlot returns NaN positions and the entire chart vanishes.
function clampRangeForScale(
  range: [number, number],
  yScale: "lin" | "log",
): [number, number] {
  if (yScale !== "log") return range;
  return [Math.max(LOG_Y_FLOOR, range[0]), Math.max(range[1], LOG_Y_FLOOR * 10)];
}

// decadeSplits returns one tick per power-of-ten across the visible y range.
// Replaces uPlot's default log splits (which densely fill each decade with
// 2/3/5/7-scaled minor ticks) for a calmer grid that still conveys the
// orders-of-magnitude scale at a glance.
function decadeSplits(_u: uPlot, _axisIdx: number, scaleMin: number, scaleMax: number): number[] {
  const lo = Math.floor(Math.log10(Math.max(scaleMin, LOG_Y_FLOOR)));
  const hi = Math.ceil(Math.log10(Math.max(scaleMax, LOG_Y_FLOOR * 10)));
  const decades: number[] = [];
  for (let i = lo; i <= hi; i++) decades.push(Math.pow(10, i));
  const within = decades.filter((v) => v >= scaleMin && v <= scaleMax);
  // A view that never crosses a decade boundary (e.g. a stable target's
  // 25-35ms band) leaves zero power-of-ten ticks inside range — the axis
  // would render with no labels at all. Fall back to evenly spaced ticks.
  return within.length >= 2 ? within : niceLinearTicks(scaleMin, scaleMax);
}

// niceLinearTicks picks a human-friendly step (1/2/5 × 10^n) and returns
// ticks at multiples of it spanning [min, max] — same heuristic most
// charting libraries use for linear axes, used here as the log-axis
// fallback when the visible range doesn't span a full decade.
function niceLinearTicks(min: number, max: number, targetCount = 5): number[] {
  if (!(max > min)) return [min];
  const rawStep = (max - min) / targetCount;
  const mag = Math.pow(10, Math.floor(Math.log10(rawStep)));
  const norm = rawStep / mag;
  const step = (norm < 1.5 ? 1 : norm < 3 ? 2 : norm < 7 ? 5 : 10) * mag;
  const out: number[] = [];
  for (let v = Math.ceil(min / step) * step; v <= max + step * 1e-9; v += step) {
    out.push(Math.round(v * 1e6) / 1e6);
  }
  return out.length > 0 ? out : [min, max];
}

// Labels that toggle each drawStack band. Index lines up with the filtered
// bands array — hiding either end of a pair drops that band. Intermediate
// 5% bands fold into the nearest legend-visible pair (p10..p90 hides with
// p5/p95, etc.) so a single click collapses the whole band family.
const BAND_PAIRS: { lo: string; hi: string }[] = [
  { lo: "min", hi: "max" },
  { lo: "p5", hi: "p95" },
  { lo: "p5", hi: "p95" },
  { lo: "p5", hi: "p95" },
  { lo: "p5", hi: "p95" },
  { lo: "p25", hi: "p75" },
  { lo: "p25", hi: "p75" },
  { lo: "p25", hi: "p75" },
  { lo: "p25", hi: "p75" },
  { lo: "p25", hi: "p75" },
];

function drawStack(
  u: uPlot,
  ctx: CanvasRenderingContext2D,
  stack: SourceStack,
  hidden: Set<string>,
  laneIndex: number,
  laneCount: number,
) {
  const { ts, bands: bandsArr, medians, losses } = stack;
  const n = ts.length;
  if (n === 0) return;

  // Each bar spans from the midpoint to its previous neighbour to the
  // midpoint to its next neighbour, so columns always touch without overlap
  // regardless of how uneven the sample cadence is. Endpoint bars mirror
  // their single neighbour's gap.
  const cxs = ts.map((t) => u.valToPos(t, "x", true));

  const medHidden = hidden.has("median");
  const lossHidden = hidden.has("loss");

  for (let i = 0; i < n; i++) {
    const cx = cxs[i];
    let leftEdge: number;
    let rightEdge: number;
    if (n === 1) {
      leftEdge = cx - 3;
      rightEdge = cx + 3;
    } else if (i === 0) {
      rightEdge = (cx + cxs[i + 1]) / 2;
      leftEdge = cx - (rightEdge - cx);
    } else if (i === n - 1) {
      leftEdge = (cxs[i - 1] + cx) / 2;
      rightEdge = cx + (cx - leftEdge);
    } else {
      leftEdge = (cxs[i - 1] + cx) / 2;
      rightEdge = (cx + cxs[i + 1]) / 2;
    }
    const slotX = Math.floor(leftEdge);
    const slotW = Math.max(1, Math.ceil(rightEdge) - slotX);
    // Sources share exact bucket-start timestamps, so each lane gets a
    // partition of the slot rather than a full-slot bar overpainting its
    // predecessors. The partition never leaves the slot: flooring a shared
    // lane width to a 1px minimum put lane n at slotX + n even when the slot
    // itself was narrower, drawing bars whole buckets right of the timestamp
    // they describe. Below 1px per lane no visible partition exists, so every
    // lane overlaps the full slot instead and the translucent fills blend.
    const laneW = slotW / laneCount;
    let x = slotX;
    let w = slotW;
    if (laneW >= 1) {
      x = slotX + Math.round(laneIndex * laneW);
      w = slotX + Math.round((laneIndex + 1) * laneW) - x;
    }

    bandsArr[i].forEach((band, b) => {
      const pair = BAND_PAIRS[b];
      if (pair && (hidden.has(pair.lo) || hidden.has(pair.hi))) return;
      const yHi = u.valToPos(band.hi, "y", true);
      const yLo = u.valToPos(band.lo, "y", true);
      ctx.fillStyle = stack.fill(band.alpha);
      ctx.fillRect(x, yHi, w, yLo - yHi);
    });

    if (!medHidden && isFinite(medians[i])) {
      const yMed = Math.round(u.valToPos(medians[i], "y", true));
      // Toggling "loss" off in the legend suppresses the outage coloring so
      // the median tick falls back to the plain palette stroke.
      ctx.fillStyle = lossHidden
        ? stack.medianColor
        : lossColor(losses[i], stack.medianColor);
      ctx.fillRect(x, yMed, w, 1);
    }
  }
}

