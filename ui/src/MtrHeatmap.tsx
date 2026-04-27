import { useEffect, useMemo, useRef, useState } from "react";
import { getHopsTimeline, type HopPoint } from "./api";

interface Props {
  targetId: string;
  refreshTick: number;
  // Shared x-axis window with the main chart (unix seconds).
  fromSec: number;
  toSec: number;
  // Click anywhere on the heatmap → bubble up the unix timestamp so the
  // HopsTable can swap to that cycle. The optional `source` arg names the
  // probe origin whose data dominated the clicked column (the source with
  // the worst loss at that timestamp). When set, the HopsTable filters to
  // that source so the path detail actually reflects what the user clicked
  // on — without it a click on a slave's lossy bucket silently returns the
  // master's clean cycle. Same callback as the main chart, plus the source.
  onCyclePick?: (timeSec: number, source?: string) => void;
  // Highlighted cycle column (unix seconds) — rendered as a vertical marker.
  selectedSec?: number;
  height?: number;
  source?: string;
  // Drop hops whose loss is 0% across the whole window. At wide ranges a
  // long path reduces to 1-2 problem hops, so hiding the clean rows keeps
  // the heatmap readable.
  hideZeroLoss?: boolean;
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
  source,
  hideZeroLoss,
}: Props) {
  const [hops, setHops] = useState<HopPoint[] | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const canvasRef = useRef<HTMLCanvasElement | null>(null);
  const wrapRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    setErr(null);
    // The backend enforces a 7d cap for timeline queries — if the user
    // selected a wider range (30d, 1y) we just don't render the heatmap.
    const span = toSec - fromSec;
    if (span > 7 * 24 * 3600) {
      setHops([]);
      return;
    }
    // AbortController cancels the in-flight fetch when the effect re-runs
    // (target/range/source change, refresh tick). The 7d view returns
    // multi-MB JSON; without abort, a rapid 24h→7d→24h click sequence keeps
    // all three responses in flight and pays for parsing each one.
    const controller = new AbortController();
    const fromISO = new Date(fromSec * 1000).toISOString();
    const toISO = new Date(toSec * 1000).toISOString();
    getHopsTimeline(targetId, fromISO, toISO, source, controller.signal)
      .then((r) => setHops(r.hops ?? []))
      .catch((e) => {
        // AbortError is the controller cleaning up — not a user-visible error.
        if (e?.name !== "AbortError") setErr(String(e));
      });
    return () => controller.abort();
  }, [targetId, refreshTick, fromSec, toSec, source]);

  // Group hops by index (row), and collect distinct cycle timestamps (columns).
  // When hideZeroLoss is on, `visibleHops` skips rows that never lost a packet
  // in the window; rank in this array becomes the y-position so gaps collapse.
  //
  // Multi-source disambiguation: when more than one origin probes the same
  // target (cluster master + slave(s)), the bucketed timeline returns one row
  // per (hop_index, time, source). We keep the worst-loss row per cell so the
  // heatmap surfaces real loss instead of being non-deterministic, and we
  // remember which source "won" each column so a click can drill into that
  // source's actual path — see worstSourceByCycle below.
  const { rows, cycles, visibleHops, worstSourceByCycle } = useMemo(() => {
    if (!hops) {
      return {
        rows: new Map<number, Map<number, HopPoint>>(),
        cycles: [] as number[],
        visibleHops: [] as number[],
        worstSourceByCycle: new Map<number, string>(),
      };
    }
    const byHop = new Map<number, Map<number, HopPoint>>();
    const cycleSet = new Set<number>();
    const lossyHops = new Set<number>();
    // Per-column tracker for "which source had the worst loss at time t,
    // across any hop". Used to forward source on click.
    const colWorst = new Map<number, { loss: number; source: string }>();
    let max = 0;
    for (const h of hops) {
      const t = Math.floor(new Date(h.Time).getTime() / 1000);
      cycleSet.add(t);
      if (h.Index > max) max = h.Index;
      if (h.LossPct > 0) lossyHops.add(h.Index);
      let row = byHop.get(h.Index);
      if (!row) {
        row = new Map();
        byHop.set(h.Index, row);
      }
      const existing = row.get(t);
      // Worst-loss-wins, with a stable tiebreak on source so identical-loss
      // cells don't flicker between renders when input order changes.
      if (
        !existing ||
        h.LossPct > existing.LossPct ||
        (h.LossPct === existing.LossPct && (h.Source ?? "") < (existing.Source ?? ""))
      ) {
        row.set(t, h);
      }
      const colCur = colWorst.get(t);
      if (!colCur || h.LossPct > colCur.loss) {
        colWorst.set(t, { loss: h.LossPct, source: h.Source ?? "" });
      }
    }
    const all: number[] = [];
    for (let i = 1; i <= max; i++) if (byHop.has(i)) all.push(i);
    const visible = hideZeroLoss ? all.filter((i) => lossyHops.has(i)) : all;
    const sourceMap = new Map<number, string>();
    for (const [t, v] of colWorst) sourceMap.set(t, v.source);
    return {
      rows: byHop,
      cycles: Array.from(cycleSet).sort((a, b) => a - b),
      visibleHops: visible,
      worstSourceByCycle: sourceMap,
    };
  }, [hops, hideZeroLoss]);

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

    if (visibleHops.length === 0 || cycles.length === 0) return;

    // Left gutter holds the hop index labels.
    const gutter = 28;
    const plotX = gutter;
    const plotW = cssW - gutter - 4;
    const rowH = Math.max(6, (cssH - 4) / visibleHops.length);

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

    for (let rank = 0; rank < visibleHops.length; rank++) {
      const hop = visibleHops[rank];
      const row = rows.get(hop);
      const y = 2 + rank * rowH;
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
    for (let rank = 0; rank < visibleHops.length; rank++) {
      if (rank % labelStep !== 0 && rank !== visibleHops.length - 1) continue;
      const y = 2 + rank * rowH + rowH / 2;
      ctx.fillText(String(visibleHops[rank]), 4, y);
    }

    // Selected-cycle marker — a thin vertical line that matches the HopsTable.
    if (selectedSec != null && selectedSec >= fromSec && selectedSec <= toSec) {
      const x = xForSec(selectedSec);
      ctx.fillStyle = "rgba(94, 234, 212, 0.55)";
      ctx.fillRect(Math.round(x), 2, 2, cssH - 4);
    }
  }, [rows, cycles, visibleHops, height, fromSec, toSec, selectedSec]);

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
        if (t != null) onCyclePick(t, worstSourceByCycle.get(t) || undefined);
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
