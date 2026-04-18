import { useEffect, useMemo, useRef, useState } from "react";
import { getHopsTimeline, type HopPoint } from "./api";

interface Props {
  targetId: string;
  refreshTick: number;
  // Shared x-axis window with the main chart (unix seconds).
  fromSec: number;
  toSec: number;
  // Click anywhere on the heatmap → bubble up the unix timestamp so the
  // HopsTable can swap to that cycle. Same callback as the main chart.
  onCyclePick?: (timeSec: number) => void;
  // Highlighted cycle column (unix seconds) — rendered as a vertical marker.
  selectedSec?: number;
  height?: number;
}

// Per-hop packet-loss heatmap over a time window. One row per discovered hop
// (numbered ascending by TTL), one column per MTR cycle that ran in the range.
// Color maps loss_pct: teal=0%, yellow=low, orange=mid, red=total — matches
// the loss palette used elsewhere in the UI.
export function MtrHeatmap({
  targetId,
  refreshTick,
  fromSec,
  toSec,
  onCyclePick,
  selectedSec,
  height = 180,
}: Props) {
  const [hops, setHops] = useState<HopPoint[] | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const canvasRef = useRef<HTMLCanvasElement | null>(null);
  const wrapRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    let cancelled = false;
    setErr(null);
    // The backend enforces a 7d cap for timeline queries — if the user
    // selected a wider range (30d, 1y) we just don't render the heatmap.
    const span = toSec - fromSec;
    if (span > 7 * 24 * 3600) {
      setHops([]);
      return;
    }
    const fromISO = new Date(fromSec * 1000).toISOString();
    const toISO = new Date(toSec * 1000).toISOString();
    getHopsTimeline(targetId, fromISO, toISO)
      .then((r) => {
        if (!cancelled) setHops(r.hops ?? []);
      })
      .catch((e) => {
        if (!cancelled) setErr(String(e));
      });
    return () => {
      cancelled = true;
    };
  }, [targetId, refreshTick, fromSec, toSec]);

  // Group hops by index (row), and collect distinct cycle timestamps (columns).
  const { rows, cycles, maxIndex } = useMemo(() => {
    if (!hops) return { rows: new Map<number, Map<number, HopPoint>>(), cycles: [] as number[], maxIndex: 0 };
    const byHop = new Map<number, Map<number, HopPoint>>();
    const cycleSet = new Set<number>();
    let max = 0;
    for (const h of hops) {
      const t = Math.floor(new Date(h.Time).getTime() / 1000);
      cycleSet.add(t);
      if (h.Index > max) max = h.Index;
      let row = byHop.get(h.Index);
      if (!row) {
        row = new Map();
        byHop.set(h.Index, row);
      }
      row.set(t, h);
    }
    return {
      rows: byHop,
      cycles: Array.from(cycleSet).sort((a, b) => a - b),
      maxIndex: max,
    };
  }, [hops]);

  // Paint the heatmap whenever data/size/selection changes. We repaint on
  // every render rather than diffing — the data is small (hops × cycles).
  useEffect(() => {
    const canvas = canvasRef.current;
    const wrap = wrapRef.current;
    if (!canvas || !wrap) return;
    const cssW = wrap.clientWidth;
    const cssH = height;
    const dpr = window.devicePixelRatio || 1;
    canvas.width = Math.floor(cssW * dpr);
    canvas.height = Math.floor(cssH * dpr);
    canvas.style.width = cssW + "px";
    canvas.style.height = cssH + "px";
    const ctx = canvas.getContext("2d");
    if (!ctx) return;
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, cssW, cssH);
    ctx.fillStyle = "#0f141c";
    ctx.fillRect(0, 0, cssW, cssH);

    if (maxIndex === 0 || cycles.length === 0) return;

    // Left gutter holds the hop index labels.
    const gutter = 28;
    const plotX = gutter;
    const plotW = cssW - gutter - 4;
    const rowH = Math.max(6, (cssH - 4) / maxIndex);

    // Each cycle column is scaled to the shared window so the x-axis lines up
    // with the main chart above. Widths shrink to 1px minimum for dense data.
    const spanSec = Math.max(1, toSec - fromSec);
    const colWForSec = (s: number) => (s / spanSec) * plotW;
    const xForSec = (s: number) => plotX + ((s - fromSec) / spanSec) * plotW;

    // Typical inter-cycle gap — used to pick a column width that feels filled.
    let colW = plotW / Math.max(1, cycles.length);
    if (cycles.length > 1) {
      const gaps: number[] = [];
      for (let i = 1; i < cycles.length; i++) gaps.push(cycles[i] - cycles[i - 1]);
      gaps.sort((a, b) => a - b);
      const median = gaps[Math.floor(gaps.length / 2)];
      colW = Math.max(1, colWForSec(median));
    }

    for (let hop = 1; hop <= maxIndex; hop++) {
      const row = rows.get(hop);
      const y = 2 + (hop - 1) * rowH;
      // Row backdrop so missing hops look empty rather than identical to 0% loss.
      ctx.fillStyle = "#131823";
      ctx.fillRect(plotX, y, plotW, rowH - 1);
      if (row) {
        for (const t of cycles) {
          const p = row.get(t);
          if (!p) continue;
          const x = xForSec(t) - colW / 2;
          ctx.fillStyle = lossColor(p.LossPct);
          ctx.fillRect(x, y, Math.max(1, colW), rowH - 1);
        }
      }
    }

    // Hop index gutter labels — skip when rowH is too tight to read.
    ctx.fillStyle = "#8a93a6";
    ctx.font = "10px system-ui, sans-serif";
    ctx.textBaseline = "middle";
    const labelStep = rowH < 12 ? Math.ceil(12 / rowH) : 1;
    for (let hop = 1; hop <= maxIndex; hop++) {
      if ((hop - 1) % labelStep !== 0 && hop !== maxIndex) continue;
      const y = 2 + (hop - 1) * rowH + rowH / 2;
      ctx.fillText(String(hop), 4, y);
    }

    // Selected-cycle marker — a thin vertical line that matches the HopsTable.
    if (selectedSec != null && selectedSec >= fromSec && selectedSec <= toSec) {
      const x = xForSec(selectedSec);
      ctx.fillStyle = "rgba(94, 234, 212, 0.55)";
      ctx.fillRect(Math.round(x), 2, 2, cssH - 4);
    }
  }, [rows, cycles, maxIndex, height, fromSec, toSec, selectedSec]);

  // Translate a pointer x position to the nearest cycle's unix seconds.
  function pickAtX(clientX: number): number | null {
    const canvas = canvasRef.current;
    if (!canvas || cycles.length === 0) return null;
    const rect = canvas.getBoundingClientRect();
    const px = clientX - rect.left;
    const gutter = 28;
    const plotX = gutter;
    const plotW = rect.width - gutter - 4;
    const frac = (px - plotX) / plotW;
    if (frac < 0 || frac > 1) return null;
    const sec = fromSec + frac * (toSec - fromSec);
    // nearest cycle
    let best = cycles[0];
    let diff = Math.abs(sec - best);
    for (const t of cycles) {
      const d = Math.abs(sec - t);
      if (d < diff) {
        best = t;
        diff = d;
      }
    }
    return best;
  }

  useEffect(() => {
    const wrap = wrapRef.current;
    if (!wrap || !hops) return;
    const ro = new ResizeObserver(() => {
      // Trigger repaint by forcing a state nudge via setHops(same).
      setHops((h) => (h ? [...h] : h));
    });
    ro.observe(wrap);
    return () => ro.disconnect();
  }, [hops]);

  if (err) return <div className="error">{err}</div>;
  if (hops === null) return <div className="empty">Loading MTR history…</div>;
  if (toSec - fromSec > 7 * 24 * 3600) {
    return <div className="empty">MTR history limited to 7d windows</div>;
  }
  if (hops.length === 0) return <div className="empty">No MTR cycles in range</div>;

  return (
    <div
      ref={wrapRef}
      className="mtr-heatmap"
      style={{ width: "100%", height, position: "relative", cursor: onCyclePick ? "pointer" : "default" }}
      onClick={(e) => {
        if (!onCyclePick) return;
        const t = pickAtX(e.clientX);
        if (t != null) onCyclePick(t);
      }}
    >
      <canvas ref={canvasRef} style={{ display: "block" }} />
    </div>
  );
}

function lossColor(pct: number): string {
  if (pct <= 0) return "#5eead4";
  if (pct < 5) return "#eab308";
  if (pct < 20) return "#f97316";
  return "#ef4444";
}
