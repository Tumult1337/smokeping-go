import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { listTargets, getCycles, type Target, type CyclesResponse, type Resolution } from "./api";
import { SmokeChart } from "./SmokeChart";
import { SmokeBarChart } from "./SmokeBarChart";
import { HttpChart } from "./HttpChart";
import { HopsTable } from "./HopsTable";
import { MtrHeatmap } from "./MtrHeatmap";

type Range = "-1h" | "-6h" | "-24h" | "-7d" | "-30d" | "-180d" | "-365d";
type ChartStyle = "band" | "bars";
const CHART_STYLE_KEY = "gosmokeping.chartStyle";
const COLLAPSED_GROUPS_KEY = "gosmokeping.collapsedGroups";

const RANGES: { label: string; value: Range }[] = [
  { label: "1h", value: "-1h" },
  { label: "6h", value: "-6h" },
  { label: "24h", value: "-24h" },
  { label: "7d", value: "-7d" },
  { label: "30d", value: "-30d" },
  { label: "180d", value: "-180d" },
  { label: "1y", value: "-365d" },
];

const AUTO_REFRESH_MS = 30_000;

export default function App() {
  const [targets, setTargets] = useState<Target[]>([]);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [range, setRange] = useState<Range>("-24h");
  const resolution: Resolution = "auto";
  const [cycles, setCycles] = useState<CyclesResponse | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [autoRefresh, setAutoRefresh] = useState(true);
  const [refreshTick, setRefreshTick] = useState(0);
  const [refreshing, setRefreshing] = useState(false);
  const [chartStyle, setChartStyle] = useState<ChartStyle>(() => {
    const saved = typeof localStorage !== "undefined" ? localStorage.getItem(CHART_STYLE_KEY) : null;
    return saved === "bars" ? "bars" : "band";
  });
  const [collapsedGroups, setCollapsedGroups] = useState<Set<string>>(() => {
    try {
      const raw = typeof localStorage !== "undefined" ? localStorage.getItem(COLLAPSED_GROUPS_KEY) : null;
      if (!raw) return new Set();
      const arr = JSON.parse(raw);
      return new Set(Array.isArray(arr) ? arr : []);
    } catch {
      return new Set();
    }
  });
  const fetchKeyRef = useRef<string>("");
  // Historical MTR pin: when set, HopsTable and the heatmap marker
  // show the cycle at that unix-seconds timestamp. Cleared when the target
  // or range changes, or when the user clicks "← latest".
  const [pickedSec, setPickedSec] = useState<number | null>(null);

  useEffect(() => {
    try {
      localStorage.setItem(CHART_STYLE_KEY, chartStyle);
    } catch {
      // localStorage unavailable — ignore
    }
  }, [chartStyle]);

  useEffect(() => {
    try {
      localStorage.setItem(COLLAPSED_GROUPS_KEY, JSON.stringify([...collapsedGroups]));
    } catch {
      // localStorage unavailable — ignore
    }
  }, [collapsedGroups]);

  const toggleGroup = useCallback((group: string) => {
    setCollapsedGroups((prev) => {
      const next = new Set(prev);
      if (next.has(group)) next.delete(group);
      else next.add(group);
      return next;
    });
  }, []);

  useEffect(() => {
    listTargets()
      .then((t) => {
        setTargets(t);
        if (t.length && !selectedId) setSelectedId(t[0].id);
      })
      .catch((e) => setError(String(e)));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    if (!selectedId) return;
    const key = `${selectedId}|${range}|${resolution}`;
    const isKeyChange = fetchKeyRef.current !== key;
    fetchKeyRef.current = key;
    setError(null);
    // Only clear the chart on a target/range change — a plain refresh keeps
    // the current view until the new data arrives, so it doesn't flash empty.
    if (isKeyChange) {
      setCycles(null);
      setPickedSec(null);
    }
    setRefreshing(true);
    let cancelled = false;
    getCycles(selectedId, range, undefined, resolution)
      .then((c) => {
        if (!cancelled) setCycles(c);
      })
      .catch((e) => {
        if (!cancelled) {
          setError(String(e));
          setCycles(null);
        }
      })
      .finally(() => {
        if (!cancelled) setRefreshing(false);
      });
    return () => {
      cancelled = true;
    };
  }, [selectedId, range, resolution, refreshTick]);

  useEffect(() => {
    if (!autoRefresh) return;
    const id = setInterval(() => {
      setRefreshTick((n) => n + 1);
    }, AUTO_REFRESH_MS);
    return () => clearInterval(id);
  }, [autoRefresh]);

  const refresh = useCallback(() => {
    setRefreshTick((n) => n + 1);
  }, []);

  const groups = useMemo(() => {
    const byGroup = new Map<string, Target[]>();
    for (const t of targets) {
      if (!byGroup.has(t.group)) byGroup.set(t.group, []);
      byGroup.get(t.group)!.push(t);
    }
    return Array.from(byGroup.entries());
  }, [targets]);

  const selected = targets.find((t) => t.id === selectedId) ?? null;
  const points = cycles?.points ?? [];
  const latest = points.length ? points[points.length - 1] : null;
  // Pin the chart x-axis to the server's echoed window so clicking 1y vs
  // 30d visibly changes the span even when only a slice has data. Falls back
  // to undefined (uPlot auto-fit) before the first response arrives.
  const fromSec = cycles?.from ? Math.floor(new Date(cycles.from).getTime() / 1000) : undefined;
  const toSec = cycles?.to ? Math.floor(new Date(cycles.to).getTime() / 1000) : undefined;

  return (
    <div className="app">
      <aside className="sidebar">
        <h1>gosmokeping</h1>
        {groups.length === 0 && <div className="empty">No targets</div>}
        {groups.map(([group, ts]) => {
          const collapsed = collapsedGroups.has(group);
          return (
            <div key={group}>
              <button
                type="button"
                className="group-title"
                aria-expanded={!collapsed}
                onClick={() => toggleGroup(group)}
              >
                <span className="group-caret">{collapsed ? "▸" : "▾"}</span>
                {group}
                <span className="group-count">{ts.length}</span>
              </button>
              {!collapsed &&
                ts.map((t) => (
                  <button
                    key={t.id}
                    className={`target-item ${t.id === selectedId ? "active" : ""}`}
                    onClick={() => setSelectedId(t.id)}
                  >
                    {t.name}
                  </button>
                ))}
            </div>
          );
        })}
      </aside>

      <main className="main">
        {!selected && <div className="empty">Select a target</div>}
        {selected && (
          <>
            <div className="toolbar">
              <strong>{selected.id}</strong>
              <span style={{ color: "#8a93a6" }}>· {selected.probe}</span>
              <div style={{ flex: 1 }} />
              {RANGES.map((r) => (
                <button
                  key={r.value}
                  className={range === r.value ? "active" : ""}
                  onClick={() => setRange(r.value)}
                >
                  {r.label}
                </button>
              ))}
              <div className="toolbar-sep" />
              <button
                className={chartStyle === "band" ? "active" : ""}
                onClick={() => setChartStyle("band")}
                title="Smoothed smoke band"
              >
                band
              </button>
              <button
                className={chartStyle === "bars" ? "active" : ""}
                onClick={() => setChartStyle("bars")}
                title="Classic SmokePing per-cycle bars"
              >
                bars
              </button>
              <button
                onClick={refresh}
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
                title={`Auto-refresh every ${AUTO_REFRESH_MS / 1000}s`}
              >
                <input
                  type="checkbox"
                  checked={autoRefresh}
                  onChange={(e) => setAutoRefresh(e.target.checked)}
                />
                auto
              </label>
            </div>
            {error && <div className="error">{error}</div>}
            {selected.probe_type === "http" ? (
              <div className="chart-wrap">
                <div className="chart-title">HTTP status + response time</div>
                <HttpChart
                  targetId={selected.id}
                  range={range}
                  refreshTick={refreshTick}
                  fromSec={fromSec}
                  toSec={toSec}
                />
              </div>
            ) : (
              <div className="chart-wrap">
                <div className="chart-title">
                  Latency — {cycles?.resolution ?? "…"} resolution
                </div>
                {chartStyle === "band" ? (
                  <SmokeChart
                    points={points}
                    fromSec={fromSec}
                    toSec={toSec}
                    onCyclePick={setPickedSec}
                  />
                ) : (
                  <SmokeBarChart
                    points={points}
                    fromSec={fromSec}
                    toSec={toSec}
                    onCyclePick={setPickedSec}
                  />
                )}
                {latest && (
                  <div className="stats" style={{ marginTop: 12 }}>
                    <span>
                      median: <strong>{latest.Median.toFixed(1)}ms</strong>
                    </span>
                    <span>
                      p95: <strong>{latest.P95.toFixed(1)}ms</strong>
                    </span>
                    <span>
                      min/max:{" "}
                      <strong>
                        {latest.Min.toFixed(1)} / {latest.Max.toFixed(1)}ms
                      </strong>
                    </span>
                    <span>
                      loss: <strong>{latest.LossPct.toFixed(1)}%</strong>
                    </span>
                  </div>
                )}
              </div>
            )}
            {(selected.probe_type === "mtr" || selected.probe_type === "icmp") && (
              <>
                <div className="chart-wrap">
                  <div className="chart-title">
                    Path {pickedSec != null ? "— historical MTR" : "(latest MTR)"}
                  </div>
                  <HopsTable
                    targetId={selected.id}
                    refreshTick={refreshTick}
                    atSec={pickedSec ?? undefined}
                    onResetAt={() => setPickedSec(null)}
                  />
                </div>
                {fromSec != null && toSec != null && (
                  <div className="chart-wrap">
                    <div className="chart-title">MTR history — per-hop loss</div>
                    <MtrHeatmap
                      targetId={selected.id}
                      refreshTick={refreshTick}
                      fromSec={fromSec}
                      toSec={toSec}
                      onCyclePick={setPickedSec}
                      selectedSec={pickedSec ?? undefined}
                    />
                  </div>
                )}
              </>
            )}
          </>
        )}
      </main>
    </div>
  );
}
