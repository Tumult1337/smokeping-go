import { useEffect, useMemo, useRef, useState } from "react";
import { getOverview, type OverviewRow, type OverviewWindow } from "./api";
import { lossTextColor } from "./palette";

// SortKey covers every clickable column header. The "target" sort orders
// rows alphabetically by id — handy when the user wants a stable view
// regardless of which numbers are currently spiking.
export type SortKey =
  | "target"
  | "loss_avg"
  | "loss_max"
  | "rtt_median"
  | "rtt_p95"
  | "rtt_max"
  | "worst_source"
  | "last_seen";

export type SortDir = "asc" | "desc";

interface Props {
  window: OverviewWindow;
  onWindowChange: (w: OverviewWindow) => void;
  sort: SortKey;
  dir: SortDir;
  onSortChange: (s: SortKey, d: SortDir) => void;
  autoRefresh: boolean;
  onAutoRefreshChange: (v: boolean) => void;
  refreshTick: number;
  onRefresh: () => void;
  onOpenSidebar: () => void;
  onPickTarget: (id: string, source?: string) => void;
  // When set, the overview is scoped to a single probe source (the "By slave"
  // view). Hides the Worst-src column and pre-selects this source on row click.
  source?: string;
}

// One overview re-render per interval, independent of the data refresh: the
// label must keep ageing when auto-refresh is off.
const LAST_SEEN_TICK_MS = 15_000;

const WINDOW_BUTTONS: { label: string; value: OverviewWindow }[] = [
  { label: "1h", value: "-1h" },
  { label: "6h", value: "-6h" },
  { label: "24h", value: "-24h" },
];

export function OverviewView(props: Props) {
  const {
    window: win,
    onWindowChange,
    sort,
    dir,
    onSortChange,
    autoRefresh,
    onAutoRefreshChange,
    refreshTick,
    onRefresh,
    onOpenSidebar,
    onPickTarget,
    source,
  } = props;

  const [rows, setRows] = useState<OverviewRow[] | null>(null);
  const [nowMs, setNowMs] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => setNowMs(Date.now()), LAST_SEEN_TICK_MS);
    return () => clearInterval(id);
  }, []);
  const [error, setError] = useState<string | null>(null);
  const [refreshing, setRefreshing] = useState(false);
  // prevKeyRef tracks the last (window, source) we fetched. A change to either
  // wipes to skeleton; a same-key refresh tick keeps existing rows visible.
  const prevKeyRef = useRef(`${win}|${source ?? ""}`);

  useEffect(() => {
    let cancelled = false;
    setError(null);
    setRefreshing(true);
    const key = `${win}|${source ?? ""}`;
    if (prevKeyRef.current !== key) {
      prevKeyRef.current = key;
      setRows(null);
    }
    getOverview(win, source)
      .then((r) => {
        if (cancelled) return;
        setRows(r.rows);
      })
      .catch((e) => {
        if (cancelled) return;
        setError(String(e));
      })
      .finally(() => {
        if (cancelled) return;
        setRefreshing(false);
      });
    return () => {
      cancelled = true;
    };
  }, [win, refreshTick, source]);

  // Sort behaviour: silent rows always sit at the top regardless of the
  // user-chosen column, so a click on "sort by median" still surfaces dead
  // targets. The chosen sort applies to the non-silent block only.
  const sortedRows = useMemo(() => {
    if (!rows) return null;
    const silent = rows.filter((r) => r.silent);
    const live = rows.filter((r) => !r.silent);
    const cmp = comparatorFor(sort, dir);
    live.sort(cmp);
    // Within the silent block, keep alphabetical order for stable scanning.
    silent.sort((a, b) => a.id.localeCompare(b.id));
    return [...silent, ...live];
  }, [rows, sort, dir]);

  const handleHeaderClick = (key: SortKey) => {
    if (sort === key) {
      onSortChange(key, dir === "asc" ? "desc" : "asc");
    } else {
      // Numeric columns default to desc (worst first); alpha/source default asc.
      const defaultDir: SortDir =
        key === "target" || key === "worst_source" ? "asc" : "desc";
      onSortChange(key, defaultDir);
    }
  };

  return (
    <>
      <div className="toolbar">
        <button
          type="button"
          className="hamburger"
          aria-label="Open target list"
          onClick={onOpenSidebar}
        >
          ☰
        </button>
        <strong>{source ? source : "Overview"}</strong>
        <span style={{ color: "var(--text-muted)" }}>
          · {source ? "slave health" : "fleet health"}
        </span>
        <div style={{ flex: 1 }} />
        <div className="segmented" role="group" aria-label="Time window">
          {WINDOW_BUTTONS.map((b) => (
            <button
              key={b.value}
              className={win === b.value ? "active" : ""}
              aria-pressed={win === b.value}
              onClick={() => onWindowChange(b.value)}
            >
              {b.label}
            </button>
          ))}
        </div>
        <button
          onClick={onRefresh}
          disabled={refreshing}
          title="Refresh now"
          aria-label="Refresh"
        >
          {refreshing ? "…" : "↻"}
        </button>
        <label
          style={{
            display: "flex",
            alignItems: "center",
            gap: 4,
            fontSize: 13,
            color: "#8a93a6",
            cursor: "pointer",
          }}
        >
          <input
            type="checkbox"
            checked={autoRefresh}
            onChange={(e) => onAutoRefreshChange(e.target.checked)}
          />
          auto
        </label>
      </div>
      {error && <div className="error">{error}</div>}
      <div className="overview-wrap">
        <table className="overview-table">
          <thead>
            <tr>
              <Th k="target" sort={sort} dir={dir} onClick={handleHeaderClick}>
                Target
              </Th>
              <Th k="loss_avg" sort={sort} dir={dir} onClick={handleHeaderClick} numeric>
                Loss avg
              </Th>
              <Th k="loss_max" sort={sort} dir={dir} onClick={handleHeaderClick} numeric>
                Loss max
              </Th>
              <Th k="rtt_median" sort={sort} dir={dir} onClick={handleHeaderClick} numeric>
                Median
              </Th>
              <Th k="rtt_p95" sort={sort} dir={dir} onClick={handleHeaderClick} numeric>
                p95
              </Th>
              <Th k="rtt_max" sort={sort} dir={dir} onClick={handleHeaderClick} numeric>
                Max
              </Th>
              {!source && (
                <Th
                  k="worst_source"
                  sort={sort}
                  dir={dir}
                  onClick={handleHeaderClick}
                >
                  Worst src
                </Th>
              )}
              <Th k="last_seen" sort={sort} dir={dir} onClick={handleHeaderClick}>
                Last seen
              </Th>
              <th className="overview-spark-col">Spark</th>
            </tr>
          </thead>
          <tbody>
            {sortedRows === null && skeletonRows()}
            {sortedRows !== null && sortedRows.length === 0 && (
              <tr>
                <td colSpan={source ? 8 : 9} className="overview-empty">
                  No targets configured
                </td>
              </tr>
            )}
            {sortedRows !== null &&
              sortedRows.map((row) => (
                <Row
                  key={row.id}
                  row={row}
                  nowMs={nowMs}
                  hideWorstSrc={!!source}
                  onPick={() => onPickTarget(row.id, source)}
                />
              ))}
          </tbody>
        </table>
      </div>
    </>
  );
}

function comparatorFor(key: SortKey, dir: SortDir): (a: OverviewRow, b: OverviewRow) => number {
  const mul = dir === "asc" ? 1 : -1;
  return (a, b) => {
    const va = valueFor(a, key);
    const vb = valueFor(b, key);
    if (va == null && vb == null) return 0;
    // nulls always sort last regardless of direction — they're not informative.
    if (va == null) return 1;
    if (vb == null) return -1;
    if (typeof va === "number" && typeof vb === "number") {
      return mul * (va - vb);
    }
    return mul * String(va).localeCompare(String(vb));
  };
}

function valueFor(r: OverviewRow, key: SortKey): number | string | null {
  switch (key) {
    case "target":
      return r.title || r.id;
    case "loss_avg":
      return r.loss_avg;
    case "loss_max":
      return r.loss_max;
    case "rtt_median":
      return r.rtt_median;
    case "rtt_p95":
      return r.rtt_p95;
    case "rtt_max":
      return r.rtt_max;
    case "worst_source":
      return r.worst_source || null;
    case "last_seen":
      return r.last_seen ? new Date(r.last_seen).getTime() : null;
  }
}

function Th(props: {
  k: SortKey;
  sort: SortKey;
  dir: SortDir;
  onClick: (k: SortKey) => void;
  numeric?: boolean;
  children: React.ReactNode;
}) {
  const active = props.sort === props.k;
  const arrow = active ? (props.dir === "asc" ? " ▲" : " ▼") : "";
  return (
    <th
      className={`overview-th${props.numeric ? " num" : ""}${active ? " active" : ""}`}
      aria-sort={active ? (props.dir === "asc" ? "ascending" : "descending") : "none"}
    >
      <button type="button" className="overview-th-btn" onClick={() => props.onClick(props.k)}>
        {props.children}
        <span className="overview-sort-arrow">{arrow}</span>
      </button>
    </th>
  );
}

function Row({
  row,
  nowMs,
  onPick,
  hideWorstSrc,
}: {
  row: OverviewRow;
  nowMs: number;
  onPick: () => void;
  hideWorstSrc?: boolean;
}) {
  const label = row.title || row.id;
  return (
    <tr
      className={`overview-row${row.silent ? " silent" : ""}`}
      onClick={onPick}
    >
      <td className="overview-target">
        <button
          type="button"
          className="overview-target-btn"
          onClick={(e) => {
            e.stopPropagation();
            onPick();
          }}
        >
          <span className="overview-target-name">{label}</span>
          <span className="overview-target-id">{row.id}</span>
        </button>
      </td>
      <td className="num">
        {row.loss_avg == null ? (
          <span className="overview-na">—</span>
        ) : (
          <span style={{ color: lossTextColor(row.loss_avg, "#cfd3dd") }}>
            {row.loss_avg.toFixed(1)}%
          </span>
        )}
      </td>
      <td className="num">
        {row.loss_max == null ? (
          <span className="overview-na">—</span>
        ) : (
          <span style={{ color: lossTextColor(row.loss_max, "#cfd3dd") }}>
            {row.loss_max.toFixed(1)}%
          </span>
        )}
      </td>
      <td className="num">{fmtMs(row.rtt_median)}</td>
      <td className="num">{fmtMs(row.rtt_p95)}</td>
      <td className="num">{fmtMs(row.rtt_max)}</td>
      {!hideWorstSrc && (
        <td className="overview-src">
          {row.worst_source || <span className="overview-na">—</span>}
        </td>
      )}
      <td className="overview-last-seen">
        {row.last_seen == null ? (
          <span className="overview-na">never</span>
        ) : (
          relativeTime(row.last_seen, nowMs)
        )}
      </td>
      <td className="overview-spark-col">
        <Sparkline values={row.sparkline} silent={row.silent} />
      </td>
    </tr>
  );
}

function fmtMs(v: number | null): React.ReactNode {
  if (v == null) return <span className="overview-na">—</span>;
  return <>{v.toFixed(1)}ms</>;
}

// Wall clock, not monotonic: this is the age of a server-supplied timestamp,
// and there is no monotonic reading of a remote clock.
function relativeTime(iso: string, nowMs: number): string {
  const t = new Date(iso).getTime();
  const dSec = Math.max(0, Math.floor((nowMs - t) / 1000));
  if (dSec < 60) return `${dSec}s ago`;
  if (dSec < 3600) return `${Math.floor(dSec / 60)}m ago`;
  if (dSec < 86400) return `${Math.floor(dSec / 3600)}h ago`;
  return `${Math.floor(dSec / 86400)}d ago`;
}

function skeletonRows(): React.ReactNode[] {
  return Array.from({ length: 8 }).map((_, i) => (
    <tr key={`skel-${i}`} className="overview-row overview-skel">
      <td colSpan={9}>
        <div className="overview-skel-bar" />
      </td>
    </tr>
  ));
}

// Sparkline is a 60x16 inline SVG. Values are normalized to the row's own
// max so each row tells its own latency story without being squashed by a
// neighbouring spike. Nulls become gaps in the polyline.
function Sparkline({ values, silent }: { values: Array<number | null>; silent: boolean }) {
  if (silent || values.length === 0) {
    return <span className="overview-na">—</span>;
  }
  const W = 60;
  const H = 16;
  let lo = Infinity;
  let hi = -Infinity;
  for (const v of values) {
    if (v == null) continue;
    if (v < lo) lo = v;
    if (v > hi) hi = v;
  }
  if (!isFinite(lo) || !isFinite(hi)) {
    return <span className="overview-na">—</span>;
  }
  if (hi === lo) hi = lo + 1;
  const stepX = values.length > 1 ? W / (values.length - 1) : W;
  // Build segments — break the polyline whenever we hit a null so gaps stay
  // visible instead of being interpolated through.
  const segments: string[] = [];
  let current = "";
  values.forEach((v, i) => {
    if (v == null) {
      if (current) segments.push(current);
      current = "";
      return;
    }
    const x = i * stepX;
    const y = H - ((v - lo) / (hi - lo)) * H;
    current += `${current ? "L" : "M"}${x.toFixed(1)},${y.toFixed(1)}`;
  });
  if (current) segments.push(current);
  return (
    <svg
      className="overview-spark"
      width={W}
      height={H}
      viewBox={`0 0 ${W} ${H}`}
      preserveAspectRatio="none"
    >
      {segments.map((d, i) => (
        <path key={i} d={d} fill="none" stroke="#5eead4" strokeWidth={1.25} />
      ))}
    </svg>
  );
}
