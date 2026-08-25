import { useEffect, useMemo, useRef, useState } from "react";
import uPlot, { type Options, type AlignedData } from "uplot";
import { getHttpSamples, type HttpPoint } from "./api";
import { paletteForSorted, lossTextColor } from "./palette";
import { colorFor, statusLabel, statusClass, isSuccess, type StatusClass } from "./httpStatus";
import { StatusStrip } from "./StatusStrip";

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

// Per-request HTTP chart: one vertical bar per sample, height = RTT, color =
// source. Status (success / 4xx / 5xx / network error) is encoded entirely by
// the StatusStrip below — one dimension per visual element, so the bar no
// longer carries the confusing status cap it used to.
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
  const [loading, setLoading] = useState(false);
  // Left/right gutter of the plot area in CSS px, tracked from u.bbox so the
  // StatusStrip canvas lines up pixel-for-pixel under the chart.
  const [plotOffsets, setPlotOffsets] = useState({ left: 34, right: 0 });
  // Hover readout: nearest sample index + cursor position in plot-area CSS px.
  const [hover, setHover] = useState<{ idx: number; left: number; top: number } | null>(null);

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

  // Window summary, computed from the fetched samples (already source-filtered
  // server-side when a source is selected). uptime mirrors the probe's loss
  // semantics: status in [200,400) is a success.
  const summary = useMemo(() => {
    if (points.length === 0) return null;
    let success = 0;
    const dist: Record<StatusClass, number> = { "2xx": 0, "3xx": 0, "4xx": 0, "5xx": 0, err: 0 };
    const rtts: number[] = [];
    for (const p of points) {
      dist[statusClass(p.Status)]++;
      if (isSuccess(p.Status)) {
        success++;
        rtts.push(p.RTT);
      }
    }
    rtts.sort((a, b) => a - b);
    const pct = (q: number): number | null =>
      rtts.length === 0 ? null : rtts[Math.min(rtts.length - 1, Math.max(0, Math.ceil((q / 100) * rtts.length) - 1))];
    return { uptime: (success / points.length) * 100, total: points.length, dist, p50: pct(50), p95: pct(95) };
  }, [points]);

  const fetchKeyRef = useRef<string>("");
  useEffect(() => {
    // Blank only when the requested series changes; a refresh tick keeps the
    // current bars until the new ones land so the chart doesn't flash empty.
    const key = `${targetId}|${range}|${source ?? ""}|${fromArg ?? ""}|${toArg ?? ""}`;
    if (fetchKeyRef.current !== key) {
      fetchKeyRef.current = key;
      setPoints([]);
    }
    let cancelled = false;
    setError(null);
    setLoading(true);
    getHttpSamples(targetId, fromArg ?? range, toArg, source)
      .then((r) => {
        if (!cancelled) setPoints(r.points ?? []);
      })
      .catch((e) => {
        if (!cancelled) {
          setError(String(e));
          setPoints([]);
        }
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
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
      legend: { show: false },
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
            const pts = pointsRef.current;
            const pal = paletteRef.current;
            // Track the plot gutter so the StatusStrip aligns under the bars.
            const dpr = devicePixelRatio || 1;
            const left = Math.round(u.bbox.left / dpr);
            const right = Math.round(u.width - (u.bbox.left + u.bbox.width) / dpr);
            setPlotOffsets((prev) => (prev.left === left && prev.right === right ? prev : { left, right }));
            if (xs.length === 0) return;
            const sts = u.data[2] as number[];
            // Bar width scales with sample density: wide enough to read in a
            // sparse window, thin enough to stay distinct when packed.
            const barW = Math.max(3, Math.min(10, (u.bbox.width / xs.length) * 0.6));
            // Network errors have no RTT, so they can't be a latency bar. Draw
            // them as a short baseline stub (a marker) — never a tall bar that
            // would masquerade as high latency. The strip carries the failure
            // loudly; this stub just keeps the sample hoverable for its error.
            const stubH = 6 * dpr;
            ctx.save();
            // Clip to plot bbox so bars at the edge don't paint over the
            // y-axis labels when the user drag-zooms into a narrow window.
            ctx.beginPath();
            ctx.rect(u.bbox.left, u.bbox.top, u.bbox.width, u.bbox.height);
            ctx.clip();
            for (let i = 0; i < xs.length; i++) {
              const x = u.valToPos(xs[i], "x", true);
              const y0 = u.valToPos(0, "y", true);
              const src = pts[i]?.Source ?? "";
              // Bar colour = source only. Status is the strip's job.
              ctx.fillStyle = pal.get(src)?.stroke ?? "#5eead4";
              if (sts[i] === 0) {
                ctx.fillRect(x - barW / 2, y0 - stubH, barW, stubH);
                continue;
              }
              const y = u.valToPos(ys[i], "y", true);
              ctx.fillRect(x - barW / 2, y, barW, y0 - y);
            }
            ctx.restore();
          },
        ],
        setCursor: [
          (u) => {
            const idx = u.cursor.idx ?? null;
            const left = u.cursor.left ?? -1;
            const top = u.cursor.top ?? -1;
            if (idx == null || left < 0) {
              setHover((prev) => (prev === null ? prev : null));
              return;
            }
            setHover({ idx, left, top });
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
  useEffect(() => {
    const u = plotRef.current;
    if (!u) return;
    let data: AlignedData;
    if (points.length === 0) {
      data = [[], [], []];
    } else {
      const ts = points.map((p) => Math.floor(new Date(p.Time).getTime() / 1000));
      // Network errors (status == 0) have no RTT — give them 0 so they don't
      // inflate the y-axis with a phantom height. The draw hook paints them as
      // a short baseline stub instead. Non-2xx with a status (4xx/5xx) keep
      // their real time-to-first-byte and render as normal bars.
      const rtts = points.map((p) => (p.Status === 0 ? 0 : p.RTT));
      const statuses = points.map((p) => p.Status);
      data = [ts, rtts, statuses];
    }
    // Always call setScale("x") when the window is known — not just on change.
    // setScale("x") is what triggers uPlot's y auto-range; without it, if the
    // http data arrives after the x-scale was already pinned on empty data, y
    // stays at the empty-data default [0,1] and every bar renders off-scale,
    // filling the whole canvas with a solid source colour.
    u.batch(() => {
      if (fromSec != null && toSec != null) {
        internalScaleRef.current = true;
        u.setScale("x", { min: fromSec, max: toSec });
        internalScaleRef.current = false;
      }
      u.setData(data, false);
    });
  }, [points, fromSec, toSec]);

  if (error) return <div className="error">{error}</div>;

  const hovered = hover && hover.idx < points.length ? points[hover.idx] : null;

  return (
    <div>
      <div className="chart-host" style={{ minHeight: height, position: "relative" }}>
        <div ref={divRef} style={{ width: "100%" }} />
        {points.length === 0 &&
          (loading ? (
            <div className="chart-skeleton" role="status" aria-label="Loading…" />
          ) : (
            <div className="chart-empty">No HTTP samples in range</div>
          ))}
        {hovered && (
          <div
            style={{
              position: "absolute",
              left: plotOffsets.left + hover!.left,
              top: hover!.top,
              transform: "translate(-50%, -100%)",
              marginTop: -8,
              pointerEvents: "none",
              background: "#10141c",
              border: "1px solid #2a3142",
              borderRadius: 4,
              padding: "4px 8px",
              fontSize: 12,
              whiteSpace: "nowrap",
              zIndex: 5,
            }}
          >
            <div style={{ color: "#8a93a6" }}>{new Date(hovered.Time).toLocaleString()}</div>
            <div>
              <span style={{ color: colorFor(hovered.Status) }}>● {statusLabel(hovered.Status)}</span>
              {hovered.Status !== 0 && <span style={{ marginLeft: 8 }}>{hovered.RTT.toFixed(1)} ms</span>}
              {hovered.Source && <span style={{ marginLeft: 8, color: "#8a93a6" }}>{hovered.Source}</span>}
            </div>
            {hovered.Err && <div style={{ color: "#ef4444" }}>{hovered.Err.slice(0, 80)}</div>}
          </div>
        )}
      </div>

      <StatusStrip
        points={points}
        fromSec={fromSec}
        toSec={toSec}
        plotLeft={plotOffsets.left}
        plotRight={plotOffsets.right}
      />

      {summary && (
        <div className="stats" style={{ marginTop: 12, display: "flex", flexWrap: "wrap", gap: 16 }}>
          <span>
            uptime:{" "}
            <strong style={{ color: lossTextColor(100 - summary.uptime, "#5eead4") }}>
              {summary.uptime.toFixed(1)}%
            </strong>{" "}
            <span style={{ color: "#8a93a6" }}>({summary.total} samples)</span>
          </span>
          <span style={{ display: "inline-flex", gap: 8 }}>
            {(["2xx", "3xx", "4xx", "5xx", "err"] as StatusClass[])
              .filter((c) => summary.dist[c] > 0)
              .map((c) => (
                <span key={c} style={{ color: colorFor(classSample(c)) }}>
                  {c === "err" ? "network error" : c} {summary.dist[c]}
                </span>
              ))}
          </span>
          <span style={{ color: "#8a93a6" }}>
            TTFB p50 <strong style={{ color: "#cbd5e1" }}>{fmtMs(summary.p50)}</strong> · p95{" "}
            <strong style={{ color: "#cbd5e1" }}>{fmtMs(summary.p95)}</strong>
          </span>
        </div>
      )}

      {/* Labelled legends: each names the element it explains, so there's no
          guessing which mark is what. */}
      <div className="stats" style={{ marginTop: 8, fontSize: 12, color: "#8a93a6" }}>
        <span className="source-label" style={{ marginRight: 4 }}>bar color = source:</span>
        {[...palette.entries()].map(([name, p]) => (
          <span key={name || "—"} style={{ display: "inline-flex", alignItems: "center", gap: 4, marginRight: 8 }}>
            <span style={{ display: "inline-block", width: 10, height: 10, background: p.stroke, borderRadius: 2 }} />
            {name || "—"}
          </span>
        ))}
      </div>
      <HttpLegend />
    </div>
  );
}

function fmtMs(v: number | null): string {
  return v == null ? "—" : `${v.toFixed(1)}ms`;
}

// Representative status code per class, for coloring the distribution chips.
function classSample(c: StatusClass): number {
  switch (c) {
    case "2xx":
      return 200;
    case "3xx":
      return 301;
    case "4xx":
      return 404;
    case "5xx":
      return 500;
    case "err":
      return 0;
  }
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
      <span className="source-label" style={{ marginRight: 4 }}>strip color = status:</span>
      {items.map((i) => (
        <span key={i.label} style={{ display: "inline-flex", alignItems: "center", gap: 4, marginRight: 8 }}>
          <span style={{ display: "inline-block", width: 10, height: 10, background: i.color, borderRadius: 2 }} />
          {i.label}
        </span>
      ))}
    </div>
  );
}
