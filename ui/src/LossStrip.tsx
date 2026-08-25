import { useCallback, useEffect, useMemo, useRef } from "react";
import { lossColor, paletteForSorted } from "./palette";

export const LOSS_STRIP_H = 6;

export type LossSeries = {
  source: string;
  ts: number[];
  losses: number[];
  hasLoss: boolean;
};

// LossStripCanvas renders a loss lane as a standalone element below a chart.
// X-positions are computed linearly from fromSec/toSec; cell widths follow the
// neighbour-midpoint rule so cells always touch without overlap.
// plotLeft (CSS px) should match the uPlot chart's left gutter (u.bbox.left/dpr)
// so the strip's x-axis aligns pixel-for-pixel with the chart above.
// One lane per lossy source, keyed to the chart palette by a gutter swatch.
export function LossStripCanvas({
  lossSeries,
  fromSec,
  toSec,
  onCyclePick,
  plotLeft,
  plotRight,
}: {
  lossSeries: LossSeries[];
  fromSec: number | undefined;
  toSec: number | undefined;
  onCyclePick?: (timeSec: number) => void;
  plotLeft?: number;
  plotRight?: number;
}) {
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const wrapRef = useRef<HTMLDivElement>(null);

  const effectiveSeries = useMemo(
    () => lossSeries.filter((s) => s.hasLoss),
    [lossSeries],
  );
  // Indexed over every source, lossy or not, because the chart assigns its
  // palette by position in the full sorted source list.
  const palette = useMemo(
    () => paletteForSorted(lossSeries.map((s) => s.source)),
    [lossSeries],
  );

  const effectiveSeriesRef = useRef(effectiveSeries);
  const fromSecRef = useRef(fromSec);
  const toSecRef = useRef(toSec);
  effectiveSeriesRef.current = effectiveSeries;
  fromSecRef.current = fromSec;
  toSecRef.current = toSec;

  const draw = useCallback(() => {
    const canvas = canvasRef.current;
    if (!canvas) return;
    const ls = effectiveSeriesRef.current;
    const from = fromSecRef.current;
    const to = toSecRef.current;
    if (ls.length === 0 || from == null || to == null) return;
    const w = canvas.clientWidth;
    if (w === 0) return;
    const stripH = ls.length * LOSS_STRIP_H;
    const dpr = devicePixelRatio || 1;
    canvas.width = Math.round(w * dpr);
    canvas.height = Math.round(stripH * dpr);
    const ctx = canvas.getContext("2d")!;
    ctx.scale(dpr, dpr);
    const span = to - from;
    const valToX = (t: number) => ((t - from) / span) * w;
    ctx.fillStyle = "rgba(255, 255, 255, 0.04)";
    ctx.fillRect(0, 0, w, stripH);
    ls.forEach((src, row) => {
      const { ts, losses } = src;
      const n = ts.length;
      if (n === 0) return;
      const cxs = ts.map((t) => valToX(t));
      const rowTop = row * LOSS_STRIP_H;
      for (let i = 0; i < n; i++) {
        if (losses[i] <= 0) continue;
        const cx = cxs[i];
        let lo: number, hi: number;
        if (n === 1) { lo = cx - 3; hi = cx + 3; }
        else if (i === 0) { hi = (cx + cxs[i + 1]) / 2; lo = cx - (hi - cx); }
        else if (i === n - 1) { lo = (cxs[i - 1] + cx) / 2; hi = cx + (cx - lo); }
        else { lo = (cxs[i - 1] + cx) / 2; hi = (cx + cxs[i + 1]) / 2; }
        const x = Math.max(0, Math.floor(lo));
        const cw = Math.max(1, Math.min(w - x, Math.ceil(hi) - x));
        ctx.fillStyle = lossColor(losses[i], "transparent");
        ctx.fillRect(x, rowTop, cw, LOSS_STRIP_H);
      }
    });
  }, []);

  useEffect(() => { draw(); }, [effectiveSeries, fromSec, toSec, draw, plotLeft]);

  useEffect(() => {
    const wrap = wrapRef.current;
    if (!wrap) return;
    const ro = new ResizeObserver(draw);
    ro.observe(wrap);
    return () => ro.disconnect();
  }, [draw]);

  const onCyclePickRef = useRef(onCyclePick);
  onCyclePickRef.current = onCyclePick;

  const handleClick = useCallback((e: React.MouseEvent<HTMLCanvasElement>) => {
    const from = fromSecRef.current;
    const to = toSecRef.current;
    const cb = onCyclePickRef.current;
    if (from == null || to == null || !cb) return;
    const rect = e.currentTarget.getBoundingClientRect();
    const t = from + ((e.clientX - rect.left) / rect.width) * (to - from);
    let bestT: number | null = null;
    let bestDist = Infinity;
    for (const src of effectiveSeriesRef.current) {
      for (const ts of src.ts) {
        const d = Math.abs(ts - t);
        if (d < bestDist) { bestDist = d; bestT = ts; }
      }
    }
    if (bestT != null) cb(bestT);
  }, []);

  if (effectiveSeries.length === 0 || fromSec == null || toSec == null) return null;

  // The "loss" label width matches the chart's left gutter (plotLeft) minus the
  // flex gap so the canvas starts at the same x position as the plot area above.
  const GAP = 6;
  const labelWidth = plotLeft != null ? Math.max(0, plotLeft - GAP) : 28;

  return (
    <div
      ref={wrapRef}
      style={{
        display: "flex",
        alignItems: "stretch",
        gap: GAP,
        borderTop: "1px solid #2a3142",
        paddingTop: 3,
        marginTop: 2,
      }}
    >
      <div
        style={{
          flexShrink: 0,
          width: labelWidth,
          position: "relative",
          height: effectiveSeries.length * LOSS_STRIP_H,
        }}
      >
        {effectiveSeries.length > 1 ? (
          effectiveSeries.map((s, i) => (
            <span
              key={s.source || "—"}
              title={`loss — ${s.source || "—"}`}
              style={{
                position: "absolute",
                right: 0,
                top: i * LOSS_STRIP_H + (LOSS_STRIP_H - 6) / 2,
                width: 6,
                height: 6,
                borderRadius: 1,
                background: palette.get(s.source)?.stroke ?? "#8a93a6",
              }}
            />
          ))
        ) : (
          <span
            style={{
              position: "absolute",
              right: 0,
              top: 0,
              fontSize: 10,
              lineHeight: `${effectiveSeries.length * LOSS_STRIP_H}px`,
              color: "var(--text-muted)",
            }}
          >
            loss
          </span>
        )}
      </div>
      <canvas
        ref={canvasRef}
        style={{
          flex: 1,
          minWidth: 0,
          height: effectiveSeries.length * LOSS_STRIP_H,
          cursor: "pointer",
          display: "block",
          ...(plotRight != null && plotRight > 0 ? { marginRight: plotRight } : {}),
        }}
        onClick={handleClick}
      />
    </div>
  );
}
