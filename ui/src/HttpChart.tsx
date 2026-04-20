import { useEffect, useMemo, useRef, useState } from "react";
import uPlot, { type Options, type AlignedData } from "uplot";
import { getHttpSamples, type HttpPoint } from "./api";
import { paletteForSorted } from "./palette";

interface Props {
  targetId: string;
  range: string;
  refreshTick: number;
  height?: number;
  fromSec?: number;
  toSec?: number;
  source?: string;
  onZoomChange?: (window: { from: number; to: number } | null) => void;
  // Absolute window override. When set, supersedes `range` for the fetch.
  fromArg?: string;
  toArg?: string;
}

// Color by status class. Network error (status==0) gets its own color so you
// can tell "server said no" apart from "never reached server".
function colorFor(status: number): string {
  if (status === 0) return "#6b7280";
  if (status >= 500) return "#ef4444";
  if (status >= 400) return "#f59e0b";
  if (status >= 300) return "#60a5fa";
  if (status >= 200) return "#5eead4";
  return "#8a93a6";
}

function statusLabel(status: number): string {
  if (status === 0) return "network error";
  return String(status);
}

// Per-request HTTP chart: one vertical bar per sample, height = RTT, color =
// status class. Replaces the smoke band for HTTP targets because with 1–2
// samples per cycle the percentile math is meaningless.
export function HttpChart({
  targetId,
  range,
  refreshTick,
  height = 260,
  fromSec,
  toSec,
  source,
  onZoomChange,
  fromArg,
  toArg,
}: Props) {
  const divRef = useRef<HTMLDivElement | null>(null);
  const plotRef = useRef<uPlot | null>(null);
  const [points, setPoints] = useState<HttpPoint[]>([]);
  const [error, setError] = useState<string | null>(null);

  // Sorted source names → palette map, matching SmokeChart's convention so a
  // given node wears the same colour everywhere on the page. Derived from the
  // data (not the target.sources list) so a source with no samples in range
  // doesn't claim a slot.
  const palette = useMemo(() => {
    const uniq = new Set<string>();
    for (const p of points) uniq.add(p.Source ?? "");
    return paletteForSorted([...uniq].sort());
  }, [points]);
  // The draw hook runs off refs because the uPlot instance is built once per
  // mount and its closures can't see later renders' palette/points.
  const paletteRef = useRef(palette);
  paletteRef.current = palette;
  const pointsRef = useRef(points);
  pointsRef.current = points;
  const onZoomChangeRef = useRef(onZoomChange);
  onZoomChangeRef.current = onZoomChange;
  const internalScaleRef = useRef(false);
  // Compare new scale against the requested window (not data extent) so that
  // drag-zooming into a range that happens to contain all the samples still
  // registers as a zoom instead of collapsing to a reset.
  const requestedWindowRef = useRef<{ from?: number; to?: number }>({});
  requestedWindowRef.current = { from: fromSec, to: toSec };

  useEffect(() => {
    let cancelled = false;
    setError(null);
    getHttpSamples(targetId, fromArg ?? range, toArg, source)
      .then((r) => {
        if (!cancelled) setPoints(r.points ?? []);
      })
      .catch((e) => {
        if (!cancelled) {
          setError(String(e));
          setPoints([]);
        }
      });
    return () => {
      cancelled = true;
    };
  }, [targetId, range, refreshTick, source, fromArg, toArg]);

  // Create uPlot once. setData drives refreshes so the DOM node stays in
  // place — destroy/recreate on every tick collapses the wrapper and the
  // page scrolls underneath the user.
  useEffect(() => {
    if (!divRef.current) return;

    const opts: Options = {
      width: divRef.current.clientWidth,
      height,
      scales: {
        x: { time: true },
        y: { auto: true, range: (_u, _min, max) => [0, Math.max(max, 1)] },
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
        {
          label: "rtt",
          stroke: "transparent",
          points: { show: false },
          paths: () => null,
        },
        { label: "status", stroke: "transparent", points: { show: false } },
      ],
      legend: { live: true },
      // Disable uPlot's default dblclick (auto-resets scales) so our own
      // handler owns the gesture — clears the zoom via onZoomChange(null)
      // without a spurious data-extent round-trip through setScale.
      cursor: { bind: { dblclick: () => null } },
      hooks: {
        draw: [
          (u) => {
            const ctx = u.ctx;
            const xs = u.data[0] as number[];
            const ys = u.data[1] as number[];
            const sts = u.data[2] as number[];
            const pts = pointsRef.current;
            const pal = paletteRef.current;
            if (xs.length === 0) return;
            const barW = 3;
            // Height of the top status-class cap. Small enough that the
            // source colour dominates the body; big enough that a 5xx stands
            // out at a glance.
            const capH = 3;
            ctx.save();
            // Clip to plot bbox so bars at the edge don't paint over the
            // y-axis labels when the user drag-zooms into a narrow window.
            ctx.beginPath();
            ctx.rect(u.bbox.left, u.bbox.top, u.bbox.width, u.bbox.height);
            ctx.clip();
            for (let i = 0; i < xs.length; i++) {
              const x = u.valToPos(xs[i], "x", true);
              const y = u.valToPos(ys[i], "y", true);
              const y0 = u.valToPos(0, "y", true);
              const src = pts[i]?.Source ?? "";
              const body = pal.get(src)?.stroke ?? colorFor(sts[i]);
              ctx.fillStyle = body;
              ctx.fillRect(x - barW / 2, y, barW, y0 - y);
              // Thin cap encodes the status class so outages stay visually
              // loud without losing source identity on the body.
              ctx.fillStyle = colorFor(sts[i]);
              ctx.fillRect(x - barW / 2, y, barW, Math.min(capH, y0 - y));
            }
            ctx.restore();
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
            if (Math.abs(from - reqFrom) <= 1 && Math.abs(to - reqTo) <= 1) return;
            onZoomChangeRef.current?.({ from, to });
          },
        ],
      },
    };

    const empty: AlignedData = [[], [], []];
    plotRef.current = new uPlot(opts, empty, divRef.current);
    const over = plotRef.current.over;
    const onDblClick = () => {
      onZoomChangeRef.current?.(null);
    };
    over.addEventListener("dblclick", onDblClick);
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
      over.removeEventListener("dblclick", onDblClick);
      plotRef.current?.destroy();
      plotRef.current = null;
    };
  }, [height]);

  // Pin the x scale only when the requested window changes. On a plain data
  // refresh (same fromSec/toSec) we skip the pin and pass resetScales=false
  // so any drag-zoom survives the tick.
  const pinRef = useRef<{ from?: number; to?: number }>({});
  useEffect(() => {
    const u = plotRef.current;
    if (!u) return;
    const pinChanged =
      pinRef.current.from !== fromSec || pinRef.current.to !== toSec;
    pinRef.current = { from: fromSec, to: toSec };
    const pin = pinChanged && fromSec != null && toSec != null;
    let data: AlignedData;
    if (points.length === 0) {
      data = [[], [], []];
    } else {
      const ts = points.map((p) => Math.floor(new Date(p.Time).getTime() / 1000));
      // Failed requests (status == 0 AND rtt == 0) still render a visible
      // marker so the outage is obvious — pin them to a small floor height
      // so they don't collapse to zero.
      let rttMax = 1;
      for (const p of points) if (p.RTT > rttMax) rttMax = p.RTT;
      const failHeight = Math.max(rttMax * 0.15, 1);
      const rtts = points.map((p) => (p.Status === 0 ? failHeight : p.RTT));
      const statuses = points.map((p) => p.Status);
      data = [ts, rtts, statuses];
    }
    u.batch(() => {
      if (pin) {
        internalScaleRef.current = true;
        u.setScale("x", { min: fromSec, max: toSec });
        internalScaleRef.current = false;
      }
      u.setData(data, false);
    });
  }, [points, fromSec, toSec]);

  if (error) return <div className="error">{error}</div>;

  const last = points.length > 0 ? points[points.length - 1] : null;
  return (
    <div>
      <div className="chart-host" style={{ minHeight: height }}>
        <div ref={divRef} style={{ width: "100%" }} />
        {points.length === 0 && <div className="chart-empty">No HTTP samples in range</div>}
      </div>
      {last && (
        <div className="stats" style={{ marginTop: 12 }}>
          <span>
            latest status: <strong style={{ color: colorFor(last.Status) }}>{statusLabel(last.Status)}</strong>
          </span>
          <span>
            latest rtt: <strong>{last.Status === 0 ? "—" : `${last.RTT.toFixed(1)}ms`}</strong>
          </span>
          {last.Err && (
            <span title={last.Err}>
              error: <strong style={{ color: "#ef4444" }}>{last.Err.slice(0, 64)}</strong>
            </span>
          )}
        </div>
      )}
      {palette.size > 1 && (
        <div className="stats" style={{ marginTop: 8, fontSize: 12, color: "#8a93a6" }}>
          <span className="source-label" style={{ marginRight: 4 }}>sources:</span>
          {[...palette.entries()].map(([name, p]) => (
            <span key={name || "—"} style={{ display: "inline-flex", alignItems: "center", gap: 4 }}>
              <span style={{ display: "inline-block", width: 10, height: 10, background: p.stroke, borderRadius: 2 }} />
              {name || "—"}
            </span>
          ))}
        </div>
      )}
      <HttpLegend />
    </div>
  );
}

function HttpLegend() {
  const items: { label: string; color: string }[] = [
    { label: "2xx", color: colorFor(200) },
    { label: "3xx", color: colorFor(301) },
    { label: "4xx", color: colorFor(404) },
    { label: "5xx", color: colorFor(500) },
    { label: "network error", color: colorFor(0) },
  ];
  return (
    <div className="stats" style={{ marginTop: 8, fontSize: 12, color: "#8a93a6" }}>
      {items.map((i) => (
        <span key={i.label} style={{ display: "inline-flex", alignItems: "center", gap: 4 }}>
          <span style={{ display: "inline-block", width: 10, height: 10, background: i.color, borderRadius: 2 }} />
          {i.label}
        </span>
      ))}
    </div>
  );
}
