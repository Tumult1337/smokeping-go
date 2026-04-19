import { useEffect, useState } from "react";
import { getHops, type HopPoint } from "./api";

interface Props {
  targetId: string;
  refreshTick: number;
  // When set, load hops for the MTR cycle closest to this unix-seconds
  // timestamp instead of the latest one. onResetAt lets the user clear
  // the pin back to "latest".
  atSec?: number;
  onResetAt?: () => void;
}

// Renders an MTR path for a target: one row per hop showing TTL, discovered
// router IP, sample count, loss%, and a min/avg/max latency bar. Defaults to
// the latest cycle; when atSec is provided, shows the nearest historical one.
export function HopsTable({ targetId, refreshTick, atSec, onResetAt }: Props) {
  const [hops, setHops] = useState<HopPoint[] | null>(null);
  const [cycleTime, setCycleTime] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    setErr(null);
    getHops(targetId, atSec)
      .then((r) => {
        if (cancelled) return;
        const rows = r.hops ?? [];
        setHops(rows);
        setCycleTime(rows.length > 0 ? rows[0].Time : null);
      })
      .catch((e) => {
        if (!cancelled) setErr(String(e));
      });
    return () => {
      cancelled = true;
    };
  }, [targetId, refreshTick, atSec]);

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

  const maxRtt = Math.max(1, ...hops.map((h) => h.Max));

  return (
    <>
    <HopsHeader atSec={atSec} cycleTime={cycleTime} onResetAt={onResetAt} />
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
        {hops.map((h) => (
          <tr key={h.Index}>
            <td>{h.Index}</td>
            <td>
              {h.IP ? (
                <span className="hop-ip">{h.IP}</span>
              ) : (
                <span className="hop-none">???</span>
              )}
            </td>
            <td className="num" style={{ color: lossColor(h.LossPct) }}>
              {h.LossPct.toFixed(1)}
            </td>
            <td className="num">{h.Sent}</td>
            <td className="num">{h.Min.toFixed(1)}</td>
            <td className="num">{h.Mean.toFixed(1)}</td>
            <td className="num">{h.Max.toFixed(1)}</td>
            <td>
              <HopBar min={h.Min} mean={h.Mean} max={h.Max} scale={maxRtt} />
            </td>
          </tr>
        ))}
      </tbody>
    </table>
    </div>
    </>
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

function lossColor(pct: number): string {
  if (pct <= 0) return "#cfd3dd";
  if (pct < 5) return "#eab308";
  if (pct < 20) return "#f97316";
  return "#ef4444";
}
