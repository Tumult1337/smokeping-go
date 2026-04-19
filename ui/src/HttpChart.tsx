import { useEffect, useMemo, useRef, useState } from "react";
import uPlot, { type Options, type AlignedData } from "uplot";
import { getHttpSamples, type HttpPoint } from "./api";

interface Props {
  targetId: string;
  range: string;
  refreshTick: number;
  height?: number;
  fromSec?: number;
  toSec?: number;
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
}: Props) {
  const divRef = useRef<HTMLDivElement | null>(null);
  const plotRef = useRef<uPlot | null>(null);
  const [points, setPoints] = useState<HttpPoint[]>([]);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    setError(null);
    getHttpSamples(targetId, range)
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
  }, [targetId, range, refreshTick]);

  const hovered = useMemo(() => points, [points]);

  useEffect(() => {
    if (!divRef.current) return;
    if (plotRef.current) {
      plotRef.current.destroy();
      plotRef.current = null;
    }
    if (hovered.length === 0) return;

    const ts = hovered.map((p) => Math.floor(new Date(p.Time).getTime() / 1000));
    // Failed requests (status == 0 AND rtt == 0) still render a visible marker
    // so the outage is obvious — pin them to a small floor height so they
    // don't collapse to zero.
    let rttMax = 1;
    for (const p of hovered) if (p.RTT > rttMax) rttMax = p.RTT;
    const failHeight = Math.max(rttMax * 0.15, 1);
    const rtts = hovered.map((p) => (p.Status === 0 ? failHeight : p.RTT));
    const statuses = hovered.map((p) => p.Status);

    const data: AlignedData = [ts, rtts, statuses];

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
      hooks: {
        draw: [
          (u) => {
            const ctx = u.ctx;
            const xs = u.data[0] as number[];
            const ys = u.data[1] as number[];
            const sts = u.data[2] as number[];
            // Bar width: constant pixels so dense ranges still show each bar.
            const barW = 3;
            ctx.save();
            for (let i = 0; i < xs.length; i++) {
              const x = u.valToPos(xs[i], "x", true);
              const y = u.valToPos(ys[i], "y", true);
              const y0 = u.valToPos(0, "y", true);
              ctx.fillStyle = colorFor(sts[i]);
              ctx.fillRect(x - barW / 2, y, barW, y0 - y);
            }
            ctx.restore();
          },
        ],
      },
    };

    plotRef.current = new uPlot(opts, data, divRef.current);
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
      plotRef.current?.destroy();
      plotRef.current = null;
    };
  }, [hovered, height, fromSec, toSec]);

  if (error) return <div className="error">{error}</div>;
  if (points.length === 0) return <div className="empty">No HTTP samples in range</div>;

  const last = points[points.length - 1];
  return (
    <div>
      <div ref={divRef} style={{ width: "100%" }} />
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
