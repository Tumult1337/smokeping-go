import { useCallback, useEffect, useMemo, useRef } from "react";
import { colorFor, isSuccess } from "./httpStatus";
import { paletteForSorted } from "./palette";
import { unixSec } from "./chartUtils";
import type { HttpPoint } from "./api";

export const STATUS_STRIP_H = 7;

type Row = { source: string; ts: number[]; statuses: number[] };

// StatusStrip is the centerpiece of the HTTP detail view: a gapless status
// timeline below the RTT bars. One row per source; every sample paints a cell
// (no gaps) so a run of 5xx/network-error reads as a loud solid block rather
// than an invisible 3px cap on a bar. Color encodes the HTTP status class only
// — source identity lives in the bars and the per-row swatch, keeping one
// dimension per visual element.
//
// X-mapping, cell-width (neighbour-midpoint) and gutter-alignment all follow
// LossStrip's conventions so the strip lines up pixel-for-pixel under the chart.
export function StatusStrip({
  points,
  fromSec,
  toSec,
  plotLeft,
  plotRight,
}: {
  points: HttpPoint[];
  fromSec: number | undefined;
  toSec: number | undefined;
  plotLeft?: number;
  plotRight?: number;
}) {
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const wrapRef = useRef<HTMLDivElement>(null);

  // Rows ordered by sorted source name so colours match paletteForSorted
  // everywhere on the page.
  const rows = useMemo((): Row[] => {
    const m = new Map<string, { pts: HttpPoint[]; secs: number[] }>();
    for (const p of points) {
      const k = p.Source ?? "";
      let g = m.get(k);
      if (!g) m.set(k, (g = { pts: [], secs: [] }));
      g.pts.push(p);
      g.secs.push(unixSec(p.Time));
    }
    // Server order is the time order: /http is ORDER BY timestamp, seq.
    return [...m.keys()].sort().map((source) => ({
      source,
      ts: m.get(source)!.secs,
      statuses: m.get(source)!.pts.map((p) => p.Status),
    }));
  }, [points]);

  const palette = useMemo(
    () => paletteForSorted(rows.map((r) => r.source)),
    [rows],
  );

  const rowsRef = useRef(rows);
  const fromRef = useRef(fromSec);
  const toRef = useRef(toSec);
  rowsRef.current = rows;
  fromRef.current = fromSec;
  toRef.current = toSec;

  const draw = useCallback(() => {
    const canvas = canvasRef.current;
    if (!canvas) return;
    const rs = rowsRef.current;
    const from = fromRef.current;
    const to = toRef.current;
    if (rs.length === 0 || from == null || to == null) return;
    const w = canvas.clientWidth;
    if (w === 0) return;
    const stripH = rs.length * STATUS_STRIP_H;
    const dpr = devicePixelRatio || 1;
    canvas.width = Math.round(w * dpr);
    canvas.height = Math.round(stripH * dpr);
    const ctx = canvas.getContext("2d")!;
    ctx.scale(dpr, dpr);
    const span = to - from || 1;
    const valToX = (t: number) => ((t - from) / span) * w;
    // Faint backing so a row with sparse samples still reads as a lane.
    ctx.fillStyle = "rgba(255, 255, 255, 0.04)";
    ctx.fillRect(0, 0, w, stripH);

    // Minimum width for a failure mark. With dense sampling (tens of thousands
    // of samples over the window) a single failure's natural cell is sub-pixel
    // and gets overdrawn by the adjacent success cell — the very invisibility
    // we're fixing. Failures are drawn in a second pass at this minimum width
    // on top of the success tiling, so one bad request can't be swallowed.
    const FAIL_MIN_W = 3;
    rs.forEach((row, ri) => {
      const { ts, statuses } = row;
      const n = ts.length;
      if (n === 0) return;
      const cxs = ts.map(valToX);
      const rowTop = ri * STATUS_STRIP_H;
      // Pass 1: gapless status tiling (neighbour-midpoint cell widths).
      for (let i = 0; i < n; i++) {
        const cx = cxs[i];
        let lo: number, hi: number;
        if (n === 1) {
          lo = cx - 3;
          hi = cx + 3;
        } else if (i === 0) {
          hi = (cx + cxs[i + 1]) / 2;
          lo = cx - (hi - cx);
        } else if (i === n - 1) {
          lo = (cxs[i - 1] + cx) / 2;
          hi = cx + (cx - lo);
        } else {
          lo = (cxs[i - 1] + cx) / 2;
          hi = (cx + cxs[i + 1]) / 2;
        }
        const x = Math.max(0, Math.floor(lo));
        const cw = Math.max(1, Math.min(w - x, Math.ceil(hi) - x));
        ctx.fillStyle = colorFor(statuses[i]);
        ctx.fillRect(x, rowTop, cw, STATUS_STRIP_H);
      }
      // Pass 2: redraw failures (4xx/5xx/network-error) on top at a guaranteed
      // minimum width so they stay loud regardless of sample density.
      for (let i = 0; i < n; i++) {
        if (isSuccess(statuses[i])) continue;
        const x = Math.max(0, Math.min(w - FAIL_MIN_W, Math.round(cxs[i]) - Math.floor(FAIL_MIN_W / 2)));
        ctx.fillStyle = colorFor(statuses[i]);
        ctx.fillRect(x, rowTop, FAIL_MIN_W, STATUS_STRIP_H);
      }
    });
  }, []);

  useEffect(() => {
    draw();
  }, [rows, fromSec, toSec, draw, plotLeft]);

  useEffect(() => {
    const wrap = wrapRef.current;
    if (!wrap) return;
    const ro = new ResizeObserver(draw);
    ro.observe(wrap);
    return () => ro.disconnect();
  }, [draw]);

  if (rows.length === 0 || fromSec == null || toSec == null) return null;

  const GAP = 6;
  const labelWidth = plotLeft != null ? Math.max(0, plotLeft - GAP) : 28;
  const multi = rows.length > 1;

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
      {/* Left gutter: matches the chart's y-axis width so the canvas starts at
          the same x as the plot area above. For one source, a "status" label;
          for several, a source-coloured swatch per row keys each lane to its
          node using the same colour the bars use. */}
      <div
        style={{
          flexShrink: 0,
          width: labelWidth,
          position: "relative",
          height: rows.length * STATUS_STRIP_H,
        }}
      >
        {multi ? (
          rows.map((r, i) => (
            <span
              key={r.source || "—"}
              title={r.source || "—"}
              style={{
                position: "absolute",
                right: 0,
                top: i * STATUS_STRIP_H + (STATUS_STRIP_H - 6) / 2,
                width: 6,
                height: 6,
                borderRadius: 1,
                background: palette.get(r.source)?.stroke ?? "#8a93a6",
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
              lineHeight: `${rows.length * STATUS_STRIP_H}px`,
              color: "#8a93a6",
            }}
          >
            status
          </span>
        )}
      </div>
      <canvas
        ref={canvasRef}
        style={{
          flex: 1,
          minWidth: 0,
          height: rows.length * STATUS_STRIP_H,
          display: "block",
          ...(plotRight != null && plotRight > 0 ? { marginRight: plotRight } : {}),
        }}
      />
    </div>
  );
}
