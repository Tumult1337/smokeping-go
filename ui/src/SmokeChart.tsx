import { useEffect, useMemo, useRef, useState } from "react";
import uPlot, { type Options, type AlignedData, type Series, type Band } from "uplot";
import type { CyclePoint } from "./api";
import { PALETTE } from "./palette";

interface Props {
  points: CyclePoint[];
  height?: number;
  fromSec?: number;
  toSec?: number;
  onCyclePick?: (timeSec: number) => void;
  onZoomChange?: (window: { from: number; to: number } | null) => void;
}

// Layered smoke band: min/max (lightest) → p5/p95 → p25/p75 (darkest fill),
// median line on top. uPlot's native "band" feature fills the area between
// two series, which is exactly what we need — no custom drawing.
//
// Multi-source targets ("all" view) fan out into one band-stack per source
// sharing a single x-axis. Each source gets its own colour from the palette
// and its own set of 7 series; nulls at timestamps where that source didn't
// probe are bridged with spanGaps so fills don't break across the interleave.
export function SmokeChart({ points, height = 320, fromSec, toSec, onCyclePick, onZoomChange }: Props) {
  const divRef = useRef<HTMLDivElement | null>(null);
  const plotRef = useRef<uPlot | null>(null);
  const onCyclePickRef = useRef(onCyclePick);
  onCyclePickRef.current = onCyclePick;
  const onZoomChangeRef = useRef(onZoomChange);
  onZoomChangeRef.current = onZoomChange;
  const internalScaleRef = useRef(false);
  // Track the requested window so the setScale hook can tell "drag-zoom inside
  // the pinned range" from "scale already matches the pin" without relying on
  // data extent (sparse data would collapse the zoom check to a false reset).
  const requestedWindowRef = useRef<{ from?: number; to?: number }>({});
  requestedWindowRef.current = { from: fromSec, to: toSec };

  const built = useMemo(() => buildAligned(points), [points]);
  // Stable signature of the source set. Only when this changes do we have to
  // tear down uPlot — series/bands topology depends on the source count, but
  // in-place setData handles value updates.
  const sourcesKey = built.sources.join("|");

  // Cursor idx drives the custom legend below the chart. null = cursor off the
  // plot; the legend falls back to the last data index (uPlot-default "live"
  // behaviour) so a static chart still reads meaningful numbers.
  const [cursorIdx, setCursorIdx] = useState<number | null>(null);
  // Flat series indices the user toggled off in the legend. Reset whenever
  // the source set changes so the mapping stays sane after rebuild.
  const [hidden, setHidden] = useState<Set<number>>(new Set());
  useEffect(() => {
    setHidden(new Set());
  }, [sourcesKey]);

  useEffect(() => {
    if (!divRef.current) return;

    const opts: Options = {
      width: divRef.current.clientWidth,
      height,
      scales: {
        x: { time: true },
        y: { auto: true },
      },
      axes: [
        { stroke: "#8a93a6", grid: { stroke: "#1f2430" } },
        {
          stroke: "#8a93a6",
          grid: { stroke: "#1f2430" },
          label: "ms",
          labelSize: 30,
        },
      ],
      series: built.series,
      bands: built.bands,
      // Built-in legend hidden — we render a per-source row below the chart
      // with NAME first and all percentile readouts inline.
      legend: { show: false },
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
      if (t != null) cb(t);
    };
    over.addEventListener("mousedown", onMouseDown);
    over.addEventListener("click", onClick);
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
      over.removeEventListener("mousedown", onMouseDown);
      over.removeEventListener("click", onClick);
      plotRef.current?.destroy();
      plotRef.current = null;
    };
    // sourcesKey rebuilds the chart when the set of sources changes; data-only
    // updates flow through the setData effect below so refreshes don't flash.
  }, [height, sourcesKey]);

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
  }, [built, fromSec, toSec]);

  // Apply the hidden-series set to uPlot. Runs on every mount (in case the
  // chart was just rebuilt — e.g. the 5173-dev HMR reload) and on every
  // toggle. Any series not in `hidden` is re-shown, so un-clicking restores.
  useEffect(() => {
    const u = plotRef.current;
    if (!u) return;
    u.batch(() => {
      for (let i = 1; i < u.series.length; i++) {
        u.setSeries(i, { show: !hidden.has(i) });
      }
    });
  }, [hidden, sourcesKey]);

  // Resolve the index the legend should show. Cursor-hover wins; otherwise
  // fall back to the last column so idle state reads the latest values.
  const xCol = built.data[0] as number[] | undefined;
  const lastIdx = xCol && xCol.length > 0 ? xCol.length - 1 : null;
  const legendIdx = cursorIdx != null ? cursorIdx : lastIdx;

  return (
    <div className="chart-host" style={{ minHeight: height }}>
      <div ref={divRef} style={{ width: "100%" }} />
      {points.length === 0 && <div className="chart-empty">No data in range</div>}
      {points.length > 0 && (
        <div className="smoke-legend">
          {built.sources.map((src, srcIdx) => {
            const palette = PALETTE[srcIdx % PALETTE.length];
            const base = 1 + srcIdx * PCT_LABELS.length;
            return (
              <div className="smoke-legend-row" key={src || `src-${srcIdx}`}>
                <span
                  className="smoke-legend-name"
                  style={{ color: palette.stroke }}
                >
                  {src || "—"}
                </span>
                {PCT_LABELS.map((label, j) => {
                  const col = built.data[base + j] as (number | null)[] | undefined;
                  const v = legendIdx != null && col ? col[legendIdx] : null;
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

type Built = {
  sources: string[];
  data: AlignedData;
  series: Series[];
  bands: Band[];
};

const PCT_KEYS = ["Min", "P5", "P25", "Median", "P75", "P95", "Max"] as const;
const PCT_LABELS = ["min", "p5", "p25", "median", "p75", "p95", "max"] as const;

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
    };
  }

  const bySource = new Map<string, CyclePoint[]>();
  for (const p of points) {
    const key = p.Source ?? "";
    let arr = bySource.get(key);
    if (!arr) {
      arr = [];
      bySource.set(key, arr);
    }
    arr.push(p);
  }
  const sources = [...bySource.keys()].sort();
  // Only prefix legend labels when there's something to disambiguate — a plain
  // single-source chart should read "min / p5 / median / …" like it always has.
  const prefixed = sources.length > 1;

  const tsSet = new Set<number>();
  for (const [, arr] of bySource) {
    for (const p of arr) tsSet.add(Math.floor(new Date(p.Time).getTime() / 1000));
  }
  const xs = [...tsSet].sort((a, b) => a - b);
  const xIdx = new Map<number, number>();
  xs.forEach((t, i) => xIdx.set(t, i));

  const data: (number | null)[][] = [xs];
  const series: Series[] = [xSeries];
  const bands: Band[] = [];

  sources.forEach((name, srcIdx) => {
    const palette = PALETTE[srcIdx % PALETTE.length];
    const cols: (number | null)[][] = PCT_KEYS.map(() => xs.map(() => null));
    for (const p of bySource.get(name)!) {
      const i = xIdx.get(Math.floor(new Date(p.Time).getTime() / 1000));
      if (i == null) continue;
      PCT_KEYS.forEach((k, c) => {
        cols[c][i] = p[k];
      });
    }
    cols.forEach((c) => data.push(c));
    series.push(...seriesFor(prefixed ? name : "", palette));
  });

  bands.push(...bandsFor(sources.length));

  return {
    sources,
    data: data as AlignedData,
    series,
    bands,
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
