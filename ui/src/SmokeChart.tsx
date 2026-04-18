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

  useEffect(() => {
    if (!divRef.current) return;
    if (plotRef.current) {
      plotRef.current.destroy();
      plotRef.current = null;
    }
    if (points.length === 0) return;

    const ts = points.map((p) => Math.floor(new Date(p.Time).getTime() / 1000));
    const min = points.map((p) => p.Min);
    const max = points.map((p) => p.Max);
    const p5 = points.map((p) => p.P5);
    const p95 = points.map((p) => p.P95);
    const p25 = points.map((p) => p.P25);
    const p75 = points.map((p) => p.P75);
    const median = points.map((p) => p.Median);

    // Series indices: 0=x, 1=min, 2=p5, 3=p25, 4=median (p50), 5=p75, 6=p95, 7=max.
    // Ascending percentile order so the legend reads p5 → p25 → p50 → p75 → p95.
    const data: AlignedData = [ts, min, p5, p25, median, p75, p95, max];

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

    plotRef.current = new uPlot(opts, data, divRef.current);
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
