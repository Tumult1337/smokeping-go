import { useEffect, useRef } from "react";
import uPlot, { type Options, type AlignedData } from "uplot";
import type { CyclePoint } from "./api";

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

// Classic SmokePing / cocopacket-style rendering: for each cycle we paint a
// column the full width of its slot (bars touch, no gaps) and stack symmetric
// percentile pairs every 5% as translucent bands — they accumulate into a
// smooth smoke gradient that darkens around the median. The median tick on
// top is colour-coded by per-cycle loss percentage.
export function SmokeBarChart({ points, height = 320, fromSec, toSec, onCyclePick }: Props) {
  const divRef = useRef<HTMLDivElement | null>(null);
  const plotRef = useRef<uPlot | null>(null);
  // Keep onCyclePick in a ref so swapping the callback doesn't force a full
  // chart rebuild (which would flash + lose hover state).
  const onCyclePickRef = useRef(onCyclePick);
  onCyclePickRef.current = onCyclePick;

  useEffect(() => {
    if (!divRef.current) return;
    if (plotRef.current) {
      plotRef.current.destroy();
      plotRef.current = null;
    }
    if (points.length === 0) return;

    const ts = points.map((p) => Math.floor(new Date(p.Time).getTime() / 1000));
    const mins = points.map((p) => p.Min);
    const maxs = points.map((p) => p.Max);
    const medians = points.map((p) => p.Median);
    const losses = points.map((p) => p.LossPct);

    // Build the stack of percentile pairs for the layered smoke. Any pair
    // that comes back fully zero (older data, before 5%-step percentiles were
    // stored) is dropped so legacy rollups still render something useful.
    const bands = points.map((p) => {
      const all: { lo: number; hi: number; alpha: number }[] = [
        { lo: p.Min, hi: p.Max, alpha: 0.07 },
        { lo: p.P5,  hi: p.P95, alpha: 0.09 },
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

    // Data layout: x then percentiles in ascending order (min, p5, p25, median,
    // p75, p95, max) followed by loss. Order here must match the `series` array
    // below so the hover legend reads left-to-right as the smoke stacks.
    const data: AlignedData = [
      ts,
      mins,
      points.map((p) => p.P5),
      points.map((p) => p.P25),
      medians,
      points.map((p) => p.P75),
      points.map((p) => p.P95),
      maxs,
      points.map((p) => p.LossPct),
    ];

    let yLo = Infinity;
    let yHi = -Infinity;
    for (const v of mins) if (v < yLo) yLo = v;
    for (const v of maxs) if (v > yHi) yHi = v;
    if (!isFinite(yLo) || !isFinite(yHi)) {
      yLo = 0;
      yHi = 1;
    }
    const yPad = Math.max(1, (yHi - yLo) * 0.1);
    const yRange: [number, number] = [Math.max(0, yLo - yPad), yHi + yPad];

    const msFmt = (v: number | null) => (v == null ? "—" : v.toFixed(2));
    const pctFmt = (v: number | null) => (v == null ? "—" : `${v.toFixed(1)}%`);

    const opts: Options = {
      width: divRef.current.clientWidth,
      height,
      scales: {
        x: {
          time: true,
          ...(fromSec != null && toSec != null
            ? { range: () => [fromSec, toSec] as [number, number] }
            : {}),
        },
        y: { auto: false, range: () => yRange },
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
      series: [
        {},
        // points: { show: false } suppresses the white sample markers uPlot
        // otherwise draws on hover / at every data point. We paint our own
        // bars in hooks.draw; the built-in points are redundant noise.
        { label: "min",    stroke: "transparent", points: { show: false }, value: (_u, v) => msFmt(v) },
        { label: "p5",     stroke: "transparent", points: { show: false }, value: (_u, v) => msFmt(v) },
        { label: "p25",    stroke: "transparent", points: { show: false }, value: (_u, v) => msFmt(v) },
        { label: "median", stroke: "transparent", points: { show: false }, value: (_u, v) => msFmt(v) },
        { label: "p75",    stroke: "transparent", points: { show: false }, value: (_u, v) => msFmt(v) },
        { label: "p95",    stroke: "transparent", points: { show: false }, value: (_u, v) => msFmt(v) },
        { label: "max",    stroke: "transparent", points: { show: false }, value: (_u, v) => msFmt(v) },
        { label: "loss",   stroke: "transparent", points: { show: false }, value: (_u, v) => pctFmt(v) },
      ],
      legend: { show: true, live: true },
      cursor: {
        points: { show: false },
        // Keep the vertical x-hair; y-hair off to stay out of the smoke.
        y: false,
      },
      hooks: {
        draw: [
          (u) => {
            const ctx = u.ctx;
            ctx.save();

            const n = ts.length;
            // Each bar spans from the midpoint to its previous neighbour to the
            // midpoint to its next neighbour, so columns always touch without
            // overlap regardless of how uneven the sample cadence is. Endpoint
            // bars mirror their single neighbour's gap so they stay the same
            // width as the adjacent bar rather than jutting off to the axis.
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
              // Floor left / ceil right so consecutive bars share their boundary
              // pixel instead of leaving a sub-pixel gap from independent rounding.
              // Translucent fills make the 1px overlap invisible.
              const x = Math.floor(leftEdge);
              const w = Math.max(1, Math.ceil(rightEdge) - x);

              for (const band of bands[i]) {
                const yHi = u.valToPos(band.hi, "y", true);
                const yLo = u.valToPos(band.lo, "y", true);
                ctx.fillStyle = `rgba(94,234,212,${band.alpha})`;
                ctx.fillRect(x, yHi, w, yLo - yHi);
              }

              // Median tick — 1px line across the cell so dense regions still
              // show a clear centre, matching classic SmokePing.
              const yMed = Math.round(u.valToPos(medians[i], "y", true));
              ctx.fillStyle = lossColor(losses[i]);
              ctx.fillRect(x, yMed, w, 1);
            }
            ctx.restore();
          },
        ],
      },
    };

    plotRef.current = new uPlot(opts, data, divRef.current);
    // Click-to-pick: resolve the hovered sample via cursor.idx and hand its
    // unix timestamp to the parent. Attached on u.over so it catches clicks
    // over the plot area but not axes / padding.
    const over = plotRef.current.over;
    const onClick = () => {
      const u = plotRef.current;
      const cb = onCyclePickRef.current;
      if (!u || !cb) return;
      const idx = u.cursor.idx;
      if (idx == null) return;
      const t = u.data[0][idx] as number | undefined;
      if (t != null) cb(t);
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
  }, [points, height, fromSec, toSec]);

  if (points.length === 0) {
    return <div className="empty">No data in range</div>;
  }
  return <div ref={divRef} style={{ width: "100%" }} />;
}

function lossColor(pct: number): string {
  if (pct <= 0) return "#5eead4";
  if (pct < 5) return "#eab308";
  if (pct < 20) return "#f97316";
  return "#ef4444";
}
