import { useEffect, useMemo, useRef, useState } from "react";
import uPlot, { type Options, type AlignedData, type Series, type Band } from "uplot";
import type { CyclePoint } from "./api";
import { PALETTE } from "./palette";
import { LossStripCanvas, type LossSeries } from "./LossStrip";
import { effectiveMin, sourcesKey as sourcesKeyOf, unixSec } from "./chartUtils";

interface Props {
  points: CyclePoint[];
  height?: number;
  fromSec?: number;
  toSec?: number;
  // "log" switches the y-axis to log10. Useful when a stable target's
  // signal (~1ms) coexists with rare spikes (~300ms) — linear autoscale
  // pins to the spike and squashes the band into a flat line near zero.
  yScale?: "lin" | "log";
  onCyclePick?: (timeSec: number) => void;
  onZoomChange?: (window: { from: number; to: number } | null) => void;
  onSoloChange?: (source: string | null) => void;
  loading?: boolean;
}

// uPlot log scales reject ≤ 0; cycles may legitimately store min=0 for
// sub-millisecond replies. Floor at 0.01ms so the band has somewhere to
// land without obliterating the actual lower extent of the data.
const LOG_Y_FLOOR = 0.01;

// Layered smoke band: min/max (lightest) → p5/p95 → p25/p75 (darkest fill),
// median line on top. uPlot's native "band" feature fills the area between
// two series, which is exactly what we need — no custom drawing.
//
// Multi-source targets ("all" view) fan out into one band-stack per source
// sharing a single x-axis. Each source gets its own colour from the palette
// and its own set of 7 series; nulls at timestamps where that source didn't
// probe are bridged with spanGaps so fills don't break across the interleave.
export function SmokeChart({ points, height = 320, fromSec, toSec, yScale = "lin", onCyclePick, onZoomChange, onSoloChange, loading }: Props) {
  const divRef = useRef<HTMLDivElement | null>(null);
  const plotRef = useRef<uPlot | null>(null);
  const onCyclePickRef = useRef(onCyclePick);
  onCyclePickRef.current = onCyclePick;
  const onZoomChangeRef = useRef(onZoomChange);
  onZoomChangeRef.current = onZoomChange;
  const onSoloChangeRef = useRef(onSoloChange);
  onSoloChangeRef.current = onSoloChange;
  const internalScaleRef = useRef(false);
  // Live mirrors of build state for the draw hook (captured at uPlot
  // construction time; would otherwise see stale data after each rebuild).
  const builtRef = useRef<Built | null>(null);
  const soloSourceRef = useRef<string | null>(null);
  // Track the requested window so the setScale hook can tell "drag-zoom inside
  // the pinned range" from "scale already matches the pin" without relying on
  // data extent (sparse data would collapse the zoom check to a false reset).
  const requestedWindowRef = useRef<{ from?: number; to?: number }>({});
  requestedWindowRef.current = { from: fromSec, to: toSec };

  // Left/right gutter of the uPlot plot area in CSS px. Tracked from u.bbox
  // so the LossStripCanvas canvas covers exactly the same x range as the chart.
  const [plotOffsets, setPlotOffsets] = useState({ left: 34, right: 0 });

  const built = useMemo(() => buildAligned(points), [points]);
  builtRef.current = built;
  // Stable signature of the source set: only a change here forces a uPlot
  // teardown, since series and band topology depend on the source count.
  const sourcesKey = sourcesKeyOf(built.sources);

  // Cursor idx drives hover readouts in the legend. null = cursor off chart.
  const [cursorIdx, setCursorIdx] = useState<number | null>(null);
  // Flat series indices the user toggled off in the legend. Reset whenever
  // the source set changes so the mapping stays sane after rebuild.
  const [hidden, setHidden] = useState<Set<number>>(new Set());
  // Which source name is soloed (others hidden in chart). null = show all.
  const [soloSource, setSoloSource] = useState<string | null>(null);
  soloSourceRef.current = soloSource;
  useEffect(() => {
    setHidden(new Set());
    setSoloSource(null);
    onSoloChangeRef.current?.(null);
  }, [sourcesKey]);

  useEffect(() => {
    if (!divRef.current) return;

    const opts: Options = {
      width: divRef.current.clientWidth,
      height,
      scales: {
        x: { time: true },
        y: yScale === "log"
          ? { auto: true, distr: 3, log: 10 }
          : { auto: true, range: { min: { pad: 0.1 }, max: { pad: 0.1 } } },
      },
      axes: [
        { stroke: "#8a93a6", grid: { stroke: "#1f2430" } },
        {
          stroke: "#8a93a6",
          grid: { stroke: "#1f2430" },
          label: "ms",
          labelSize: 30,
          // One tick per decade in log mode — uPlot's default minor ticks
          // (2/3/5/7 within each decade) make the grid look noisy.
          ...(yScale === "log" ? { splits: decadeSplits } : {}),
        },
      ],
      series: built.series,
      bands: built.bands,
      // Built-in legend hidden — we render a per-source row below the chart
      // with NAME first and all percentile readouts inline.
      legend: { show: false },
      // Disable uPlot's default dblclick (resets scales to data extent) so
      // our own handler owns the gesture; otherwise the stock reset fires
      // first and the setScale hook pushes a bogus zoom range before we
      // clear it.
      cursor: { bind: { dblclick: () => null } },
      hooks: {
        draw: [
          (u) => {
            const dpr = devicePixelRatio || 1;
            const left = Math.round(u.bbox.left / dpr);
            const right = Math.round(u.width - (u.bbox.left + u.bbox.width) / dpr);
            setPlotOffsets((prev) => (prev.left === left && prev.right === right ? prev : { left, right }));
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
            // Scale within 1s of the pinned window means uPlot just re-applied
            // it (round-trip after data refresh) — not a user zoom gesture.
            if (Math.abs(from - reqFrom) <= 1 && Math.abs(to - reqTo) <= 1) return;
            onZoomChangeRef.current?.({ from, to });
          },
        ],
      },
    };

    const empty: AlignedData = [[], ...built.series.slice(1).map(() => [] as number[])] as AlignedData;
    plotRef.current = new uPlot(opts, empty, divRef.current);

    const over = plotRef.current.over;
    // Track mousedown position so a drag-zoom release (which also fires click
    // on the same element) doesn't double as a cycle-pick. 3px threshold
    // matches uPlot's default drag sensitivity — anything larger is a gesture.
    let dragStart: { x: number; y: number } | null = null;
    // Deferred cycle-pick so a dblclick can intercept and clear the zoom
    // instead. 200ms is the usual click/dblclick disambiguation window.
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
      const idx = u.cursor.idx;
      if (idx == null) return;
      const t = u.data[0][idx] as number | undefined;
      if (t == null) return;
      if (pendingClick != null) window.clearTimeout(pendingClick);
      pendingClick = window.setTimeout(() => {
        pendingClick = null;
        cb(t);
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
      // Clear the window pin so the setData effect re-pins x on the freshly
      // constructed uPlot. Without this a yScale toggle (rebuild without
      // window change) leaves the new uPlot's x scale uninitialised.
      pinRef.current = {};
    };
    // sourcesKey rebuilds the chart when the set of sources changes; data-only
    // updates flow through the setData effect below so refreshes don't flash.
    // yScale forces a rebuild because uPlot's scale `distr` is read once at
    // construction time — setScale("y", …) can't switch a scale from lin to
    // log after the fact.
  }, [height, sourcesKey, yScale]);

  // Pin the x scale when the requested window changes (range button, new
  // target). On a plain data refresh within the same window we skip the pin
  // and pass resetScales=false to setData so any drag-zoom the user applied
  // survives the refresh — they shouldn't have to re-zoom every 30s.
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
    // yScale is a dep so a lin↔log toggle (which rebuilds uPlot via the
    // construction effect and clears pinRef in its cleanup) re-enters this
    // effect, sees pinChanged=true on the empty pin, and pins the new
    // uPlot's x scale before the first draw.
  }, [built, fromSec, toSec, yScale]);

  // Apply the hidden-series set (individual toggles) and soloSource (show one
  // source only) to uPlot. sourcesKey in deps covers built.sources changes.
  useEffect(() => {
    const u = plotRef.current;
    if (!u) return;
    u.batch(() => {
      for (let i = 1; i < u.series.length; i++) {
        const srcIdx = Math.floor((i - 1) / PCT_LABELS.length);
        const srcName = built.sources[srcIdx] ?? "";
        const soloHide = soloSource != null && srcName !== soloSource;
        u.setSeries(i, { show: !hidden.has(i) && !soloHide });
      }
    });
    // Rescale Y to the soloed source's range, or back to global when all shown.
    const range = soloSource != null
      ? (built.sourceYRanges.get(soloSource) ?? built.globalYRange)
      : built.globalYRange;
    if (range) {
      const min = yScale === "log" ? Math.max(LOG_Y_FLOOR, range[0]) : range[0];
      u.setScale("y", { min, max: range[1] });
    }
  }, [hidden, soloSource, sourcesKey, built.sourceYRanges, built.globalYRange, yScale]);

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
        <div className="smoke-legend">
          {built.sources.map((src, srcIdx) => {
            const palette = PALETTE[srcIdx % PALETTE.length];
            const base = 1 + srcIdx * PCT_LABELS.length;
            const multi = built.sources.length > 1;
            const dimmed = soloSource != null && src !== soloSource;
            const agg = built.aggregates[srcIdx];
            const aggVals = agg
              ? [agg.min, agg.p5, agg.p25, agg.median, agg.p75, agg.p95, agg.max]
              : PCT_LABELS.map(() => null);
            return (
              <div className={`smoke-legend-row${dimmed ? " dimmed" : ""}`} key={src || `src-${srcIdx}`}>
                {multi ? (
                  <button
                    type="button"
                    className="smoke-legend-name smoke-legend-name-btn"
                    style={{ color: palette.text }}
                    onClick={() => {
                      const next = soloSource === src ? null : src;
                      setSoloSource(next);
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
                {PCT_LABELS.map((label, j) => {
                  const col = built.data[base + j] as (number | null)[] | undefined;
                  // Keyed on the cursor, not on the value: a fully-lost cycle
                  // has null columns on purpose, and falling through to the
                  // window aggregate printed the window's own min/median/max
                  // beside a badge that says "cycle" — the outage reading as
                  // ordinary latency.
                  const v =
                    cursorIdx != null
                      ? col
                        ? col[cursorIdx]
                        : null
                      : aggVals[j];
                  const seriesIdx = base + j;
                  const off = hidden.has(seriesIdx);
                  return (
                    <button
                      type="button"
                      className={`smoke-legend-val${off ? " off" : ""}`}
                      key={label}
                      onClick={() =>
                        setHidden((prev) => {
                          const next = new Set(prev);
                          if (next.has(seriesIdx)) next.delete(seriesIdx);
                          else next.add(seriesIdx);
                          return next;
                        })
                      }
                    >
                      {label}:{" "}
                      <strong>{v == null ? "—" : v.toFixed(1)}</strong>
                    </button>
                  );
                })}
              </div>
            );
          })}
        </div>
      )}
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
};

type Built = {
  sources: string[];
  data: AlignedData;
  series: Series[];
  bands: Band[];
  lossSeries: LossSeries[];
  anyLoss: boolean;
  aggregates: SourceAgg[];
  sourceYRanges: Map<string, [number, number]>;
  globalYRange: [number, number] | null;
};

const PCT_KEYS = ["Min", "P5", "P25", "Median", "P75", "P95", "Max"] as const;
const PCT_LABELS = ["min", "p5", "p25", "median", "p75", "p95", "max"] as const;

// decadeSplits returns one tick per power-of-ten across the visible y range.
// Replaces uPlot's default log splits (which add minor 2/3/5/7 ticks per
// decade) so the grid stays readable at log10.
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

function buildAligned(points: CyclePoint[]): Built {
  const xSeries: Series = {};
  if (points.length === 0) {
    // Keep a single-band topology so the legend doesn't flicker between
    // zero-source and one-source states while loading.
    const palette = PALETTE[0];
    return {
      sources: [""],
      data: [[], [], [], [], [], [], [], []],
      series: [xSeries, ...seriesFor("", palette)],
      bands: bandsFor(1),
      lossSeries: [],
      anyLoss: false,
      aggregates: [{ min: null, p5: null, p25: null, median: null, p75: null, p95: null, max: null }],
      sourceYRanges: new Map(),
      globalYRange: null,
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
  // Only prefix legend labels when there's something to disambiguate — a plain
  // single-source chart should read "min / p5 / median / …" like it always has.
  const prefixed = sources.length > 1;

  const tsSet = new Set<number>();
  for (const [, g] of bySource) {
    for (const sec of g.secs) tsSet.add(sec);
  }
  const xs = [...tsSet].sort((a, b) => a - b);
  const xIdx = new Map<number, number>();
  xs.forEach((t, i) => xIdx.set(t, i));

  const sourceYRanges = new Map<string, [number, number]>();
  let globalLo = Infinity, globalHi = -Infinity;

  const data: (number | null)[][] = [xs];
  const series: Series[] = [xSeries];
  const bands: Band[] = [];
  const lossSeries: LossSeries[] = [];
  const aggregates: SourceAgg[] = [];
  let anyLoss = false;

  sources.forEach((name, srcIdx) => {
    const palette = PALETTE[srcIdx % PALETTE.length];
    const cols: (number | null)[][] = PCT_KEYS.map(() => xs.map(() => null));
    // Server order is the time order: every cycles query is ORDER BY
    // timestamp (raw) or bucket_ts, source (bucketed).
    const { pts, secs } = bySource.get(name)!;
    pts.forEach((p, si) => {
      const i = xIdx.get(secs[si]);
      if (i == null) return;
      // 100%-loss cycles have no valid RTT data; leave as null so spanGaps
      // bridges over them rather than drawing a false dip to 0ms.
      if (p.LossPct >= 100) return;
      PCT_KEYS.forEach((k, c) => {
        cols[c][i] = k === "Min" ? effectiveMin(p) : p[k];
      });
    });
    cols.forEach((c) => data.push(c));
    series.push(...seriesFor(prefixed ? name : "", palette));

    const ts = secs;
    const losses = pts.map((p) => p.LossPct);
    const hasLoss = losses.some((l) => l > 0);
    if (hasLoss) anyLoss = true;
    lossSeries.push({ source: name, ts, losses, hasLoss });

    // Mean of each per-cycle percentile, not a window percentile — averaging
    // p95s suppresses the isolated spikes a p95 exists to surface.
    const valid = pts.filter((p) => p.LossPct < 100);
    if (valid.length > 0) {
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
      });
    } else {
      aggregates.push({ min: null, p5: null, p25: null, median: null, p75: null, p95: null, max: null });
    }

    // Per-source y range for solo rescale.
    let srcLo = Infinity, srcHi = -Infinity;
    for (const p of pts) {
      if (p.LossPct >= 100) continue;
      const effMin = effectiveMin(p);
      if (effMin > 0 && effMin < srcLo) srcLo = effMin;
      if (p.Max > srcHi) srcHi = p.Max;
    }
    if (isFinite(srcLo) && isFinite(srcHi)) {
      const srcPad = Math.max(1, (srcHi - srcLo) * 0.1);
      const r: [number, number] = [Math.max(0, srcLo - srcPad), srcHi + srcPad];
      sourceYRanges.set(name, r);
      if (r[0] < globalLo) globalLo = r[0];
      if (r[1] > globalHi) globalHi = r[1];
    }
  });

  bands.push(...bandsFor(sources.length));

  return {
    sources,
    data: data as AlignedData,
    series,
    bands,
    lossSeries,
    anyLoss,
    aggregates,
    sourceYRanges,
    globalYRange: isFinite(globalLo) && isFinite(globalHi) ? [globalLo, globalHi] : null,
  };
}

// seriesFor returns the 7 series that back one source's smoke stack. Order
// must stay in sync with PCT_KEYS / bandsFor so band indices line up.
function seriesFor(
  name: string,
  palette: { stroke: string; fill: (a: number) => string },
): Series[] {
  const prefix = name ? `${name}/` : "";
  const mk = (label: string, opts: Partial<Series>): Series => ({
    label: `${prefix}${label}`,
    points: { show: false },
    spanGaps: true,
    ...opts,
  });
  return [
    mk(PCT_LABELS[0], { stroke: "transparent", fill: palette.fill(0.08) }),
    mk(PCT_LABELS[1], { stroke: "transparent", fill: palette.fill(0.18) }),
    mk(PCT_LABELS[2], { stroke: "transparent", fill: palette.fill(0.28) }),
    mk(name || PCT_LABELS[3], { stroke: palette.stroke, width: 2 }),
    mk(PCT_LABELS[4], { stroke: "transparent", fill: palette.fill(0.28) }),
    mk(PCT_LABELS[5], { stroke: "transparent", fill: palette.fill(0.18) }),
    mk(PCT_LABELS[6], { stroke: "transparent", fill: palette.fill(0.08) }),
  ];
}

function bandsFor(sourceCount: number): Band[] {
  const out: Band[] = [];
  for (let i = 0; i < sourceCount; i++) {
    const palette = PALETTE[i % PALETTE.length];
    const base = 1 + i * 7; // first series col after x
    // min↔max (outer), p5↔p95, p25↔p75 (darkest).
    out.push({ series: [base + 0, base + 6], fill: palette.fill(0.10) });
    out.push({ series: [base + 1, base + 5], fill: palette.fill(0.18) });
    out.push({ series: [base + 2, base + 4], fill: palette.fill(0.28) });
  }
  return out;
}

