import { useEffect, useMemo, useRef, useState } from "react";
import { getHopsTimeline, type HopPoint } from "./api";
import { lossColor } from "./palette";
import { cycleAtSec } from "./chartUtils";

// The heatmap's chrome colours all live as CSS custom properties in styles.css
// (single source of truth). A <canvas> 2d context can't read CSS vars, so
// readHeatColors pulls --heat-ok (neutral "ok" cell) and --accent-rgb
// (selected-cycle marker) off :root at draw time. Falls back to literals only
// if the vars are somehow missing.
function readHeatColors() {
  const s = getComputedStyle(document.documentElement);
  const heatOk = s.getPropertyValue("--heat-ok").trim() || "#2c3647";
  const noReply = s.getPropertyValue("--heat-noreply").trim() || "#4a5570";
  const accentRgb = s.getPropertyValue("--accent-rgb").trim() || "79, 147, 245";
  return { heatOk, noReply, markerFill: `rgba(${accentRgb}, 0.7)` };
}

// Legend thresholds. Swatch colours derive from lossColor() (the single source
// for the loss ramp) and --heat-ok for the neutral cell, so the legend never
// hard-codes a colour of its own.
const HEATMAP_LEGEND: ReadonlyArray<readonly [string, number]> = [
  ["ok", 0],
  ["<5%", 3],
  ["<20%", 12],
  ["≥20%", 50],
];

// HeatmapLegend is the color key rendered once beneath the heatmap(s) so the
// ok/loss ramp is readable without hovering.
function HeatmapLegend() {
  return (
    <div className="mtr-heatmap-legend">
      {HEATMAP_LEGEND.map(([label, pct]) => (
        <span key={label}>
          <i style={{ background: lossColor(pct, "var(--heat-ok)") }} />
          {label}
        </span>
      ))}
      <span>
        <i style={{ background: "var(--heat-noreply)" }} />
        no reply
      </span>
    </div>
  );
}

interface Props {
  targetId: string;
  refreshTick: number;
  // Shared x-axis window with the main chart (unix seconds).
  fromSec: number;
  toSec: number;
  // Click anywhere on a heatmap → bubble up the unix timestamp so the
  // HopsTable can swap to that cycle. The `source` arg names which probe
  // origin's heatmap was clicked. With per-source heatmaps that's the
  // owning source directly — no worst-loss-wins guess needed.
  onCyclePick?: (timeSec: number, source?: string) => void;
  // Highlighted cycle column (unix seconds) — rendered as a vertical marker
  // in every per-source heatmap.
  selectedSec?: number;
  // The probe origin this heatmap draws. Required: /hops/timeline serves one
  // origin per request, and "" is the untagged pre-cluster origin, not "all".
  source: string;
}

// Per-hop packet-loss heatmap over a time window for one probe origin. The
// stacking across origins lives in MtrSection, which renders one of these per
// source — matching the endpoint, which serves one origin per request.
export function MtrHeatmap({
  targetId,
  refreshTick,
  fromSec,
  toSec,
  onCyclePick,
  selectedSec,
  source,
}: Props) {
  const [hops, setHops] = useState<HopPoint[] | null>(null);
  const [err, setErr] = useState<string | null>(null);

  const [stepSec, setStepSec] = useState(0);

  useEffect(() => {
    setErr(null);
    // The backend enforces a 7d cap for timeline queries — if the user
    // selected a wider range (30d, 1y) we just don't render anything.
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
      .then((r) => {
        setHops(r.hops ?? []);
        setStepSec(r.step_sec ?? 0);
      })
      .catch((e) => {
        // AbortError is the controller cleaning up — not a user-visible error.
        if (e?.name !== "AbortError") setErr(String(e));
      });
    return () => controller.abort();
  }, [targetId, refreshTick, fromSec, toSec, source]);

  if (err && (hops === null || hops.length === 0)) return <div className="error">{err}</div>;
  if (hops === null) return <div className="empty">Loading MTR history…</div>;
  if (toSec - fromSec > 7 * 24 * 3600) {
    return <div className="empty">MTR history limited to 7d windows</div>;
  }
  if (hops.length === 0) return <div className="empty">No MTR cycles in range</div>;

  return (
    <>
      <PathHeatmap
        source={source}
        hops={hops}
        fromSec={fromSec}
        toSec={toSec}
        stepSec={stepSec}
        selectedSec={selectedSec}
        onPick={onCyclePick}
        stale={err != null}
      />
      <HeatmapLegend />
    </>
  );
}

// PathHeatmap renders one source's hop-loss matrix. Owns its own canvas,
// ref, and ResizeObserver. Height adapts to hop count so each row stays
// at least ~14px regardless of how short or long the path is.
// Width of a single cycle's column when neither the server step nor a row
// gap can size it. Wide enough to see, narrow enough not to imply a span.
const MIN_COL_PX = 3;

function PathHeatmap({
  source,
  hops,
  fromSec,
  toSec,
  stepSec,
  selectedSec,
  onPick,
  stale,
}: {
  source: string;
  hops: HopPoint[];
  fromSec: number;
  toSec: number;
  stepSec: number;
  selectedSec?: number;
  onPick?: (timeSec: number, source?: string) => void;
  stale: boolean;
}) {
  const canvasRef = useRef<HTMLCanvasElement | null>(null);
  const wrapRef = useRef<HTMLDivElement | null>(null);
  const [repaintCount, setRepaintCount] = useState(0);
  // Hovered cell, resolved with the same pickAtX a click uses so the tooltip
  // always describes the cell a click would open.
  const [hover, setHover] = useState<{
    x: number;
    y: number;
    hop: number;
    sec: number;
    p: HopPoint;
  } | null>(null);

  // rows: hop index → (cycleSec → HopPoint).
  // cycles: distinct cycle timestamps in this source.
  // visibleHops: every hop index in the path, ascending. We render the full
  // path rather than only the lossy rows: intermediate routers that rate-limit
  // TTL-expired ICMP show as "loss" here, so a lossy-only view both buries the
  // real (clean) last hop and makes the bottom-most row a mid-path rate-limit
  // artifact the eye reads as the destination.
  const { rows, cycles, visibleHops } = useMemo(() => {
    const byHop = new Map<number, Map<number, HopPoint>>();
    const cycleSet = new Set<number>();
    let maxIdx = 0;
    for (const h of hops) {
      const t = Math.floor(new Date(h.Time).getTime() / 1000);
      cycleSet.add(t);
      if (h.Index > maxIdx) maxIdx = h.Index;
      // MaxLossPct (worst single cycle in the bucket) catches brief spikes
      // that bucket-avg LossPct dilutes to ~0%. Falls back to LossPct for
      // raw rows where the server didn't compute a max.
      const worst = (h as { MaxLossPct?: number }).MaxLossPct ?? h.LossPct;
      let row = byHop.get(h.Index);
      if (!row) {
        row = new Map();
        byHop.set(h.Index, row);
      }
      // Same bucket can legitimately have multiple rows when CH bucketed
      // a path flap (see QueryHopsTimeline). Keep the worst-loss-wins
      // entry — the heatmap is a loss view, not a path-topology view.
      const existing = row.get(t);
      const existingWorst =
        existing && ((existing as { MaxLossPct?: number }).MaxLossPct ?? existing.LossPct);
      if (!existing || worst > (existingWorst ?? 0)) row.set(t, h);
    }
    const visible: number[] = [];
    for (let i = 1; i <= maxIdx; i++) if (byHop.has(i)) visible.push(i);
    return {
      rows: byHop,
      cycles: Array.from(cycleSet).sort((a, b) => a - b),
      visibleHops: visible,
    };
  }, [hops]);

  // Adaptive height: at least 14px per hop row, plus a fixed bottom axis
  // strip. Clamped so a 30-hop path doesn't push the page to 500+px.
  const rowH = 14;
  const axisH = 18;
  const height = Math.min(
    Math.max(80, visibleHops.length * rowH + axisH + 4),
    420
  );

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
    const { heatOk, noReply, markerFill } = readHeatColors();
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, cssW, cssH);
    ctx.fillStyle = "#0f141c";
    ctx.fillRect(0, 0, cssW, cssH);

    if (visibleHops.length === 0 || cycles.length === 0) return;

    const gutter = 28;
    const plotX = gutter;
    const plotW = cssW - gutter - 4;
    const plotH = cssH - axisH - 2;
    const actualRowH = Math.max(6, (plotH - 4) / visibleHops.length);

    const spanSec = Math.max(1, toSec - fromSec);
    const colWForSec = (s: number) => (s / spanSec) * plotW;
    const xForSec = (s: number) => plotX + ((s - fromSec) / spanSec) * plotW;

    // A column is one bucket wide. Prefer the server's resolved step: row
    // spacing can only be measured when there are at least two rows, and a
    // wide window holding a single bucket has none — sizing that by row count
    // painted one bucket across the whole span, reading as hours of history
    // that had not been collected yet. The current server always buckets, but
    // a v2.2.0 server omits step_sec entirely (read as 0), so the median
    // inter-cycle gap is the live fallback that keeps a rolling upgrade
    // rendering. With neither available, draw a thin mark: under-drawing one
    // cycle is honest, overdrawing invents history.
    // The same step fixes the offset: bucket timestamps are bucket starts, so
    // a bucketed column is drawn from t; only the version-skew fallback is
    // centred, since its rows sit on no known grid.
    let colW = Math.max(1, MIN_COL_PX);
    if (stepSec > 0) {
      colW = Math.max(1, colWForSec(stepSec));
    } else if (cycles.length > 1) {
      const gaps: number[] = [];
      for (let i = 1; i < cycles.length; i++) gaps.push(cycles[i] - cycles[i - 1]);
      gaps.sort((a, b) => a - b);
      const median = gaps[Math.floor(gaps.length / 2)];
      colW = Math.max(1, colWForSec(median));
    }

    for (let rank = 0; rank < visibleHops.length; rank++) {
      const hop = visibleHops[rank];
      const row = rows.get(hop);
      const y = 2 + rank * actualRowH;
      ctx.fillStyle = "#131823";
      ctx.fillRect(plotX, y, plotW, actualRowH - 1);
      if (row) {
        for (const t of cycles) {
          const p = row.get(t);
          if (!p) continue;
          const x = stepSec > 0 ? xForSec(t) : xForSec(t) - colW / 2;
          // Color by MaxLossPct so a brief 100% loss event inside a 5-min
          // bucket stays visible — averaging it (LossPct) to ~3% would make
          // it disappear into the clean background.
          const worst = (p as { MaxLossPct?: number }).MaxLossPct ?? p.LossPct;
          // A hop with no resolved address never answered the TTL probe — a
          // silent/rate-limited router, not a real packet drop. Paint it the
          // muted "no reply" neutral instead of the loss ramp so transit noise
          // doesn't read as a genuine outage; only hops that actually replied
          // get the red ramp.
          ctx.fillStyle = p.IP ? lossColor(worst, heatOk) : noReply;
          ctx.fillRect(x, y, Math.max(1, colW), actualRowH - 1);
        }
      }
    }

    // Hop index gutter labels.
    ctx.fillStyle = "#8a93a6";
    ctx.font = '10px "JetBrains Mono Variable", ui-monospace, monospace';
    ctx.textBaseline = "middle";
    const labelStep = actualRowH < 12 ? Math.ceil(12 / actualRowH) : 1;
    for (let rank = 0; rank < visibleHops.length; rank++) {
      if (rank % labelStep !== 0 && rank !== visibleHops.length - 1) continue;
      const y = 2 + rank * actualRowH + actualRowH / 2;
      ctx.fillText(String(visibleHops[rank]), 4, y);
    }

    // Bottom time axis: 4-5 ticks across the plot width with localized
    // short labels. Lets the heatmap stand on its own without the user
    // having to glance up at the chart's x-axis.
    const ticks = pickAxisTicks(fromSec, toSec, 5);
    ctx.fillStyle = "#4a5160";
    ctx.strokeStyle = "#2a3142";
    ctx.lineWidth = 1;
    ctx.beginPath();
    ctx.moveTo(plotX, plotH);
    ctx.lineTo(plotX + plotW, plotH);
    ctx.stroke();
    ctx.textBaseline = "top";
    ctx.textAlign = "center";
    for (const t of ticks) {
      const x = xForSec(t);
      ctx.beginPath();
      ctx.moveTo(x, plotH);
      ctx.lineTo(x, plotH + 4);
      ctx.stroke();
      ctx.fillText(formatTick(t, toSec - fromSec), x, plotH + 6);
    }
    ctx.textAlign = "left";

    // Selected-cycle marker.
    if (selectedSec != null && selectedSec >= fromSec && selectedSec <= toSec) {
      const x = xForSec(selectedSec);
      ctx.fillStyle = markerFill;
      ctx.fillRect(Math.round(x), 2, 2, plotH - 4);
    }
  }, [rows, cycles, visibleHops, height, fromSec, toSec, selectedSec, stepSec, repaintCount]);

  useEffect(() => {
    const wrap = wrapRef.current;
    if (!wrap) return;
    const ro = new ResizeObserver(() => setRepaintCount((n) => n + 1));
    ro.observe(wrap);
    return () => ro.disconnect();
  }, []);

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
    return cycleAtSec(cycles, stepSec, fromSec + frac * (toSec - fromSec));
  }

  // hopAtY mirrors the draw effect's row geometry: rows start at y=2 and are
  // actualRowH tall, above the axis strip.
  function hopAtY(clientY: number): number | null {
    const canvas = canvasRef.current;
    if (!canvas || visibleHops.length === 0) return null;
    const rect = canvas.getBoundingClientRect();
    const y = clientY - rect.top;
    const plotH = rect.height - axisH - 2;
    const actualRowH = Math.max(6, (plotH - 4) / visibleHops.length);
    if (y < 2 || y >= plotH) return null;
    const rank = Math.floor((y - 2) / actualRowH);
    return rank >= 0 && rank < visibleHops.length ? visibleHops[rank] : null;
  }

  function handleMouseMove(e: React.MouseEvent) {
    const wrap = wrapRef.current;
    const sec = pickAtX(e.clientX);
    const hop = hopAtY(e.clientY);
    const p = sec != null && hop != null ? rows.get(hop)?.get(sec) : undefined;
    if (!wrap || sec == null || hop == null || !p) {
      setHover(null);
      return;
    }
    const rect = wrap.getBoundingClientRect();
    setHover({ x: e.clientX - rect.left, y: e.clientY - rect.top, hop, sec, p });
  }

  // worstCycleSec maps a clicked bucket-start (unix sec, as keyed in `rows`) to
  // the timestamp of a cycle that is actually inside that bucket: the
  // worst-loss one across the visible hops, since cells are coloured by
  // MaxLossPct and clicking a red cell should open the cycle that lost rather
  // than the earliest one (which read as "one to the left"). A clean bucket
  // takes the same path, because /hops?at= resolves the nearest cycle within
  // ±15m and the bucket start is that far from every cycle in it once the
  // probe interval floors the step above 30m — the cell is drawn, and clicking
  // it opened a neighbouring bucket's cycle or nothing at all. Only a row that
  // carries no WorstTime at all falls back to the key the column was drawn at.
  function worstCycleSec(bucketSec: number): number {
    let bestLoss = -1;
    let worstISO: string | undefined;
    for (const hop of visibleHops) {
      const p = rows.get(hop)?.get(bucketSec);
      if (!p?.WorstTime) continue;
      const loss = p.MaxLossPct ?? p.LossPct;
      if (loss > bestLoss) {
        bestLoss = loss;
        worstISO = p.WorstTime;
      }
    }
    if (worstISO) {
      const s = new Date(worstISO).getTime() / 1000;
      if (Number.isFinite(s)) return s;
    }
    return bucketSec;
  }

  // After a pick the path table above this heatmap re-renders to the new
  // cycle's hops. This target's route flaps between e.g. 10 and 17 hops
  // cycle-to-cycle (anycast), so that table changes height and the heatmap the
  // user just clicked jumps out from under the cursor. The reflow lands
  // asynchronously (after the /hops fetch), and in the multi-source view
  // several tables resize at once.
  //
  // We track this heatmap's position within the scroll *content*
  // (scroll-invariant), not its viewport top. A reflow above the heatmap
  // shifts its content-space position; the user scrolling does NOT. So
  // compensating only content-space shifts keeps the heatmap visually fixed
  // across the table resize without fighting a user who scrolls during the
  // bounded settle window — tracking viewport top instead made any scroll get
  // yanked back. The window stops once the layout settles.
  function pinScrollAcrossReflow() {
    const el = wrapRef.current;
    const scroller = el?.closest(".main") as HTMLElement | null;
    if (!el || !scroller) return;
    const contentTop = () =>
      el.getBoundingClientRect().top -
      scroller.getBoundingClientRect().top +
      scroller.scrollTop;
    let prev = contentTop();
    const start = performance.now();
    let corrected = false;
    let stableFrames = 0;
    const tick = () => {
      const now = contentTop();
      const d = now - prev;
      if (Math.abs(d) >= 1) {
        // Reflow above us moved the heatmap down/up in the content; scroll by
        // the same amount to hold its viewport position. (A user scroll leaves
        // content-space position unchanged, so d is 0 and we don't touch it.)
        scroller.scrollTop += d;
        prev = now;
        corrected = true;
        stableFrames = 0;
      } else {
        stableFrames++;
      }
      if ((corrected && stableFrames > 6) || performance.now() - start > 1500) return;
      requestAnimationFrame(tick);
    };
    requestAnimationFrame(tick);
  }

  if (visibleHops.length === 0) {
    return <div className="empty">All hops clean in this range</div>;
  }

  return (
    <div
      ref={wrapRef}
      className="mtr-heatmap"
      style={{
        width: "100%",
        height,
        position: "relative",
        cursor: onPick ? "pointer" : "default",
      }}
      onClick={(e) => {
        if (!onPick) return;
        const t = pickAtX(e.clientX);
        if (t == null) return;
        pinScrollAcrossReflow();
        onPick(worstCycleSec(t), source || undefined);
      }}
      onMouseMove={handleMouseMove}
      onMouseLeave={() => setHover(null)}
    >
      <canvas ref={canvasRef} style={{ display: "block" }} />
      {hover && (
        <div
          className="mtr-heatmap-tip"
          style={
            hover.x > 220
              ? { right: `calc(100% - ${hover.x}px + 12px)`, top: Math.max(0, hover.y - 10) }
              : { left: hover.x + 12, top: Math.max(0, hover.y - 10) }
          }
        >
          <div className="mtr-heatmap-tip-head">
            hop {hover.hop} · {new Date(hover.sec * 1000).toLocaleString()}
          </div>
          <div>{hover.p.IP || "no reply"}</div>
          <div>
            loss {(hover.p.MaxLossPct ?? hover.p.LossPct).toFixed(1)}%
            {hover.p.MaxLossPct != null ? " (worst cycle)" : ""}
          </div>
          {/* Absent on a pre-step_sec server and blanked with the address on
              redacted rows — both render as an ordinary cell. */}
          {hover.p.Unreach && (
            <div className="mtr-heatmap-tip-unreach">
              <span className="hop-unreach">{hover.p.Unreach}</span> trace ended
              here: target unreachable
            </div>
          )}
        </div>
      )}
      {stale && (
        <div
          style={{
            position: "absolute",
            top: 4,
            right: 4,
            fontSize: 10,
            color: "#8a93a6",
            background: "#0f141c",
            padding: "1px 5px",
            borderRadius: 3,
            pointerEvents: "none",
          }}
        >
          stale
        </div>
      )}
    </div>
  );
}

// pickAxisTicks returns 4-5 unix-second timestamps spaced across [from, to]
// at human-readable round positions when possible. Uses simple uniform
// division — the heatmap is narrow so picking "nice" minute/hour boundaries
// rarely changes the visual; uniform avoids cluster bias on irregular cycles.
function pickAxisTicks(from: number, to: number, count: number): number[] {
  if (to <= from || count < 2) return [from, to];
  const step = (to - from) / (count - 1);
  const out: number[] = [];
  for (let i = 0; i < count; i++) out.push(Math.round(from + i * step));
  return out;
}

// formatTick renders a unix-second tick. Wide windows (>24h) drop the
// minute precision; narrow windows include it. Local-time consistent with
// the rest of the UI's date formatting.
function formatTick(t: number, spanSec: number): string {
  const d = new Date(t * 1000);
  if (spanSec > 24 * 3600) {
    return d.toLocaleDateString(undefined, { month: "short", day: "numeric" })
      + " " + d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
  }
  return d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
}
