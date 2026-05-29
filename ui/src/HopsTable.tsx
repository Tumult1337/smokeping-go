import { useEffect, useRef, useState } from "react";
import { getHops, type HopPoint } from "./api";
import { lossColor } from "./palette";

interface Props {
  targetId: string;
  refreshTick: number;
  // When set, load hops for the MTR cycle closest to this unix-seconds
  // timestamp instead of the latest one. onResetAt lets the user clear
  // the pin back to "latest".
  atSec?: number;
  onResetAt?: () => void;
  source?: string;
  // Hide hops whose loss is 0% across the window. Used at wide ranges
  // (≥ 6h) to collapse the clean-hop noise in long paths.
  hideZeroLoss?: boolean;
}

// Renders an MTR path for a target: one row per hop showing TTL, discovered
// router IP, sample count, loss%, and a min/avg/max latency bar. Defaults to
// the latest cycle; when atSec is provided, shows the nearest historical one.
export function HopsTable({ targetId, refreshTick, atSec, onResetAt, source, hideZeroLoss }: Props) {
  const [hops, setHops] = useState<HopPoint[] | null>(null);
  const [cycleTime, setCycleTime] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const prevKeyRef = useRef<string>("");

  useEffect(() => {
    // Blank only when the target or source changes (genuinely different path).
    // A cycle re-pick (atSec change) keeps the previous rows visible while the
    // new cycle loads — blanking collapses the section height and the scroll
    // container jumps to the top. Stale-while-revalidate keeps layout stable.
    const k = `${targetId}|${source ?? ""}`;
    const keyChanged = prevKeyRef.current !== k;
    prevKeyRef.current = k;
    setErr(null);
    if (keyChanged) setHops(null);
    const controller = new AbortController();
    getHops(targetId, atSec, source, controller.signal)
      .then((r) => {
        const rows = r.hops ?? [];
        setHops(rows);
        setCycleTime(rows.length > 0 ? rows[0].Time : null);
      })
      .catch((e) => {
        if (e?.name !== "AbortError") setErr(String(e));
      });
    return () => controller.abort();
  }, [targetId, refreshTick, atSec, source]);

  if (err) return <div className="error">{err}</div>;
  if (hops === null) return <div className="empty">Loading hops…</div>;
  if (hops.length === 0) {
    return (
      <>
        {atSec != null && (
          <HopsHeader atSec={atSec} cycleTime={null} onResetAt={onResetAt} />
        )}
        <div className="empty">
          {atSec != null ? "No MTR cycle near this time" : "No hop data yet"}
        </div>
      </>
    );
  }

  const visible = hideZeroLoss ? hops.filter((h) => h.LossPct > 0) : hops;
  if (visible.length === 0) {
    return (
      <>
        <HopsHeader atSec={atSec} cycleTime={cycleTime} onResetAt={onResetAt} />
        <div className="empty">All hops clean in this range</div>
      </>
    );
  }

  // Group by Source so the all-view (one cycle per source returned by
  // /hops?at=…) renders as one labeled subtable per source instead of a
  // flat concatenation of N paths with repeating ttl=1..N columns. With
  // a single source (filtered view) groups.length === 1 and the layout
  // collapses back to the original single-table form.
  const groups = groupBySource(visible);
  const sharedScale = Math.max(1, ...visible.map((h) => h.Max));

  return (
    <>
      <HopsHeader
        atSec={atSec}
        cycleTime={groups.length === 1 ? cycleTime : null}
        onResetAt={onResetAt}
      />
      {groups.map((g) => (
        <HopsPath
          key={g.source || "(unspecified)"}
          source={g.source}
          time={g.time}
          rows={g.rows}
          scale={sharedScale}
          showSourceHeading={groups.length > 1}
        />
      ))}
    </>
  );
}

interface HopsGroup {
  source: string;
  time: string;
  rows: HopPoint[];
}

function groupBySource(hops: HopPoint[]): HopsGroup[] {
  const order: string[] = [];
  const byKey = new Map<string, HopsGroup>();
  for (const h of hops) {
    const existing = byKey.get(h.Source);
    if (existing) {
      existing.rows.push(h);
    } else {
      order.push(h.Source);
      byKey.set(h.Source, { source: h.Source, time: h.Time, rows: [h] });
    }
  }
  return order.map((s) => byKey.get(s)!);
}

export function HopsPath({
  source,
  time,
  rows,
  scale,
  showSourceHeading,
}: {
  source: string;
  time: string;
  rows: HopPoint[];
  scale: number;
  showSourceHeading: boolean;
}) {
  return (
    <div className="hops-path">
      {showSourceHeading && (
        <div className="hops-path-heading">
          <span className="hops-path-source">{source || "(unspecified)"}</span>
          <span className="hops-path-time">{new Date(time).toLocaleString()}</span>
        </div>
      )}
      <div className="hops-table-wrap">
        <table className="hops-table">
          <thead>
            <tr>
              <th>#</th>
              <th>host</th>
              <th className="num">loss%</th>
              <th className="num">sent</th>
              <th className="num">min</th>
              <th className="num">avg</th>
              <th className="num">max</th>
              <th>latency</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((h) => (
              <tr key={h.Index}>
                <td>{h.Index}</td>
                <td>
                  {h.IP ? (
                    <>
                      <span className="hop-ip">{h.IP}</span>
                      <a
                        className="hop-whois"
                        href={`https://bgp.tools/search?q=${encodeURIComponent(h.IP)}`}
                        target="_blank"
                        rel="noreferrer noopener"
                        title={`Look up ${h.IP} on bgp.tools`}
                      >
                        ↗
                      </a>
                    </>
                  ) : (
                    <span className="hop-none">???</span>
                  )}
                </td>
                <td className="num" style={{ color: lossColor(h.LossPct, "#cfd3dd") }}>
                  {h.LossPct.toFixed(1)}
                </td>
                <td className="num">{h.Sent}</td>
                <td className="num">{h.Min.toFixed(1)}</td>
                <td className="num">{h.Mean.toFixed(1)}</td>
                <td className="num">{h.Max.toFixed(1)}</td>
                <td>
                  <HopBar min={h.Min} mean={h.Mean} max={h.Max} scale={scale} />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

function HopsHeader({
  atSec,
  cycleTime,
  onResetAt,
}: {
  atSec?: number;
  cycleTime: string | null;
  onResetAt?: () => void;
}) {
  if (atSec == null) return null;
  const label = cycleTime
    ? new Date(cycleTime).toLocaleString()
    : new Date(atSec * 1000).toLocaleString();
  return (
    <div className="hops-header">
      <span>Showing cycle at {label}</span>
      {onResetAt && (
        <button className="hops-reset" onClick={onResetAt} title="Show latest">
          ← latest
        </button>
      )}
    </div>
  );
}

function HopBar({
  min,
  mean,
  max,
  scale,
}: {
  min: number;
  mean: number;
  max: number;
  scale: number;
}) {
  const pct = (v: number) => `${(100 * v) / scale}%`;
  return (
    <div
      style={{
        position: "relative",
        height: 10,
        width: 160,
        background: "#1a1f2b",
        borderRadius: 2,
      }}
    >
      <div
        style={{
          position: "absolute",
          left: pct(min),
          width: pct(Math.max(0, max - min)),
          top: 0,
          bottom: 0,
          background: "rgba(94,234,212,0.4)",
          borderRadius: 2,
        }}
      />
      <div
        style={{
          position: "absolute",
          left: pct(mean),
          width: 2,
          top: -1,
          bottom: -1,
          background: "#5eead4",
        }}
      />
    </div>
  );
}

