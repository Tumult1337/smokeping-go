import { useEffect, useRef } from "react";
import uPlot, { type Options, type AlignedData } from "uplot";
import type { CyclePoint } from "./api";

interface Props {
  points: CyclePoint[];
  height?: number;
  fromSec?: number;
  toSec?: number;
  onCyclePick?: (timeSec: number) => void;
}

// Layered smoke band: min/max (lightest) → p5/p95 → p25/p75 (darkest fill),
// median line on top. uPlot's native "band" feature fills the area between
// two series, which is exactly what we need — no custom drawing.
export function SmokeChart({ points, height = 320, fromSec, toSec, onCyclePick }: Props) {
  const divRef = useRef<HTMLDivElement | null>(null);
  const plotRef = useRef<uPlot | null>(null);
  const onCyclePickRef = useRef(onCyclePick);
  onCyclePickRef.current = onCyclePick;

  // Create uPlot once. Data and scale updates flow through setData / setScale
  // below so the DOM node never unmounts on refresh — otherwise the wrapper
  // collapses mid-frame and the page scrolls.
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
      series: [
        {},
        { label: "min", stroke: "transparent", fill: "rgba(94,234,212,0.08)", points: { show: false } },
        { label: "p5", stroke: "transparent", fill: "rgba(94,234,212,0.18)", points: { show: false } },
        { label: "p25", stroke: "transparent", fill: "rgba(94,234,212,0.32)", points: { show: false } },
        { label: "median", stroke: "#5eead4", width: 2, points: { show: false } },
        { label: "p75", stroke: "transparent", fill: "rgba(94,234,212,0.32)", points: { show: false } },
        { label: "p95", stroke: "transparent", fill: "rgba(94,234,212,0.18)", points: { show: false } },
        { label: "max", stroke: "transparent", fill: "rgba(94,234,212,0.08)", points: { show: false } },
      ],
      bands: [
        { series: [1, 7], fill: "rgba(94,234,212,0.10)" },
        { series: [2, 6], fill: "rgba(94,234,212,0.18)" },
        { series: [3, 5], fill: "rgba(94,234,212,0.28)" },
      ],
      legend: { live: true },
    };

    const empty: AlignedData = [[], [], [], [], [], [], [], []];
    plotRef.current = new uPlot(opts, empty, divRef.current);

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
  }, [height]);

  useEffect(() => {
    const u = plotRef.current;
    if (!u) return;
    if (points.length === 0) {
      u.setData([[], [], [], [], [], [], [], []]);
      return;
    }
    const ts = points.map((p) => Math.floor(new Date(p.Time).getTime() / 1000));
    const data: AlignedData = [
      ts,
      points.map((p) => p.Min),
      points.map((p) => p.P5),
      points.map((p) => p.P25),
      points.map((p) => p.Median),
      points.map((p) => p.P75),
      points.map((p) => p.P95),
      points.map((p) => p.Max),
    ];
    u.setData(data);
  }, [points]);

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
