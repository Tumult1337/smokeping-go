import { useEffect, useMemo, useRef } from "react";
import uPlot, { type Options, type AlignedData, type Series } from "uplot";
import type { CyclePoint } from "./api";
import { PALETTE } from "./palette";

interface Props {
  points: CyclePoint[];
  height?: number;
  // Requested window (unix seconds). When set, the x-axis is pinned to
  // [fromSec, toSec] so sparse data doesn't visually collapse the window —
  // clicking 1y vs 30d is otherwise indistinguishable when coverage is thin.
  fromSec?: number;
  toSec?: number;
  // Invoked when the user clicks a bar; receives that cycle's unix timestamp.
  // Used by the MTR cycle-picker to swap the HopsTable to that moment.
  onCyclePick?: (timeSec: number) => void;
}

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
export function SmokeBarChart({ points, height = 320, fromSec, toSec, onCyclePick }: Props) {
  const divRef = useRef<HTMLDivElement | null>(null);
  const plotRef = useRef<uPlot | null>(null);
  // Keep onCyclePick in a ref so swapping the callback doesn't force a full
  // chart rebuild (which would flash + lose hover state).
  const onCyclePickRef = useRef(onCyclePick);
  onCyclePickRef.current = onCyclePick;

  // All data that the draw hook and scale-range callback read from live in
  // refs — the uPlot instance is created once per source set, so closures
  // captured at construction time would go stale after each setData.
  const stacksRef = useRef<SourceStack[]>([]);
  const yRangeRef = useRef<[number, number]>([0, 1]);

  const built = useMemo(() => buildSources(points), [points]);
  const sourcesKey = built.sources.join("|");

  useEffect(() => {
    if (!divRef.current) return;

    const msFmt = (v: number | null) => (v == null ? "—" : v.toFixed(2));
    const pctFmt = (v: number | null) => (v == null ? "—" : `${v.toFixed(1)}%`);

    // Build one legend row per source. Order per source is
    // min/p5/p25/median/p75/p95/max/loss — must match the data column order
    // in buildSources().
    const series: Series[] = [{}];
    // Only prefix labels when disambiguating between sources — a single-source
    // chart should read "min / p5 / …" unchanged.
    const prefixed = built.sources.length > 1;
    built.sources.forEach((name) => {
      const prefix = prefixed && name ? `${name}/` : "";
      const mk = (label: string, fmt: (v: number | null) => string): Series => ({
        label: `${prefix}${label}`,
        stroke: "transparent",
        points: { show: false },
        value: (_u, v) => fmt(v),
      });
      series.push(
        mk("min", msFmt),
        mk("p5", msFmt),
        mk("p25", msFmt),
        mk(prefixed ? name || "median" : "median", msFmt),
        mk("p75", msFmt),
        mk("p95", msFmt),
        mk("max", msFmt),
        mk("loss", pctFmt),
      );
    });

    const opts: Options = {
      width: divRef.current.clientWidth,
      height,
      scales: {
        x: { time: true },
        y: { auto: false, range: () => yRangeRef.current },
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
      series,
      legend: { show: true, live: true },
      cursor: {
        points: { show: false },
        // Keep the vertical x-hair; y-hair off to stay out of the smoke.
        y: false,
      },
      hooks: {
        draw: [
          (u) => {
            const stacks = stacksRef.current;
            if (stacks.length === 0) return;
            const ctx = u.ctx;
            ctx.save();
            for (const stack of stacks) {
              drawStack(u, ctx, stack);
            }
            ctx.restore();
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
    const onClick = () => {
      const u = plotRef.current;
      const cb = onCyclePickRef.current;
      if (!u || !cb) return;
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
      if (best != null) cb(best);
    };
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
      over.removeEventListener("click", onClick);
      plotRef.current?.destroy();
      plotRef.current = null;
    };
    // sourcesKey rebuilds the chart when the set of sources changes; data-only
    // updates flow through the setData effect below.
  }, [height, sourcesKey]);

  useEffect(() => {
    const u = plotRef.current;
    if (!u) return;

    if (built.stacks.length === 0) {
      stacksRef.current = [];
      yRangeRef.current = [0, 1];
      u.setData(built.data);
      return;
    }

    stacksRef.current = built.stacks;
    yRangeRef.current = built.yRange;
    u.setData(built.data);
    // setData already triggers a redraw, but hooks.draw closes over refs we
    // just mutated — force another pass so the fresh stacks land.
    u.redraw(false, true);
  }, [built]);

  useEffect(() => {
    const u = plotRef.current;
    if (!u || fromSec == null || toSec == null) return;
    u.setScale("x", { min: fromSec, max: toSec });
  }, [fromSec, toSec]);

  return (
    <div className="chart-host" style={{ minHeight: height }}>
      <div ref={divRef} style={{ width: "100%" }} />
      {points.length === 0 && <div className="chart-empty">No data in range</div>}
    </div>
  );
}

type Built = {
  sources: string[];
  data: AlignedData;
  stacks: SourceStack[];
  yRange: [number, number];
};

function buildSources(points: CyclePoint[]): Built {
  if (points.length === 0) {
    return {
      sources: [],
      data: [[]],
      stacks: [],
      yRange: [0, 1],
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

  // Union x-axis so the cursor can pick any source's sample. Each source's
  // values stay on its own index domain inside the stack; uPlot only uses
  // the union for cursor + legend alignment.
  const tsSet = new Set<number>();
  for (const [, arr] of bySource) {
    for (const p of arr) tsSet.add(Math.floor(new Date(p.Time).getTime() / 1000));
  }
  const xs = [...tsSet].sort((a, b) => a - b);
  const xIdx = new Map<number, number>();
  xs.forEach((t, i) => xIdx.set(t, i));

  const data: (number | null)[][] = [xs];
  const stacks: SourceStack[] = [];
  let yLo = Infinity;
  let yHi = -Infinity;

  sources.forEach((name, srcIdx) => {
    const palette = PALETTE[srcIdx % PALETTE.length];
    const pts = bySource.get(name)!.slice().sort(
      (a, b) => new Date(a.Time).getTime() - new Date(b.Time).getTime(),
    );

    const ts = pts.map((p) => Math.floor(new Date(p.Time).getTime() / 1000));
    const medians = pts.map((p) => p.Median);
    const losses = pts.map((p) => p.LossPct);

    // Per-cycle percentile stack — any fully-zero pair (legacy rollups before
    // the 5% step) gets filtered so old data still renders something useful.
    const bands: Band[][] = pts.map((p) => {
      const all: Band[] = [
        { lo: p.Min, hi: p.Max, alpha: 0.07 },
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
      if (p.Min < yLo) yLo = p.Min;
      if (p.Max > yHi) yHi = p.Max;
    }

    // Build 8 aligned columns on the union x-axis: min/p5/p25/median/p75/p95/
    // max/loss. Only legend readouts use this; the draw hook works directly
    // off the SourceStack instead. Unused slots are null so hover over a
    // neighbour's slot shows "—" for this source.
    const cols: (number | null)[][] = Array.from({ length: 8 }, () => xs.map(() => null));
    pts.forEach((p, i) => {
      const idx = xIdx.get(ts[i]);
      if (idx == null) return;
      cols[0][idx] = p.Min;
      cols[1][idx] = p.P5;
      cols[2][idx] = p.P25;
      cols[3][idx] = p.Median;
      cols[4][idx] = p.P75;
      cols[5][idx] = p.P95;
      cols[6][idx] = p.Max;
      cols[7][idx] = p.LossPct;
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
  };
}

function drawStack(u: uPlot, ctx: CanvasRenderingContext2D, stack: SourceStack) {
  const { ts, bands: bandsArr, medians, losses } = stack;
  const n = ts.length;
  if (n === 0) return;

  // Each bar spans from the midpoint to its previous neighbour to the
  // midpoint to its next neighbour, so columns always touch without overlap
  // regardless of how uneven the sample cadence is. Endpoint bars mirror
  // their single neighbour's gap.
  const cxs = ts.map((t) => u.valToPos(t, "x", true));

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
    const x = Math.floor(leftEdge);
    const w = Math.max(1, Math.ceil(rightEdge) - x);

    for (const band of bandsArr[i]) {
      const yHi = u.valToPos(band.hi, "y", true);
      const yLo = u.valToPos(band.lo, "y", true);
      ctx.fillStyle = stack.fill(band.alpha);
      ctx.fillRect(x, yHi, w, yLo - yHi);
    }

    const yMed = Math.round(u.valToPos(medians[i], "y", true));
    ctx.fillStyle = lossColor(losses[i], stack.medianColor);
    ctx.fillRect(x, yMed, w, 1);
  }
}

function lossColor(pct: number, okColor: string): string {
  if (pct <= 0) return okColor;
  if (pct < 5) return "#eab308";
  if (pct < 20) return "#f97316";
  return "#ef4444";
}
