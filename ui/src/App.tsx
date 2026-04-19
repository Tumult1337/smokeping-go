import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  listTargets,
  getCycles,
  type Target,
  type CyclesResponse,
  type Resolution,
} from "./api";
import { SmokeChart } from "./SmokeChart";
import { SmokeBarChart } from "./SmokeBarChart";
import { HttpChart } from "./HttpChart";
import { HopsTable } from "./HopsTable";
import { MtrHeatmap } from "./MtrHeatmap";
import { paletteForSorted } from "./palette";

type Range = "-1h" | "-6h" | "-24h" | "-7d" | "-30d" | "-180d" | "-365d";
type ChartStyle = "band" | "bars";
const CHART_STYLE_KEY = "gosmokeping.chartStyle";
const COLLAPSED_GROUPS_KEY = "gosmokeping.collapsedGroups";

// Ranges wide enough that long MTR paths become visual clutter; we drop
// clean hops so the table + heatmap stay readable.
const WIDE_RANGES: Range[] = ["-6h", "-24h", "-7d", "-30d", "-180d", "-365d"];

const RANGES: { label: string; value: Range }[] = [
  { label: "1h", value: "-1h" },
  { label: "6h", value: "-6h" },
  { label: "24h", value: "-24h" },
  { label: "7d", value: "-7d" },
  { label: "30d", value: "-30d" },
  { label: "180d", value: "-180d" },
  { label: "1y", value: "-365d" },
];

// Backend caps HTTP sample queries at 7d (raw-bucket retention).
const HTTP_RANGES: Range[] = ["-1h", "-6h", "-24h", "-7d"];

const AUTO_REFRESH_MS = 30_000;

export default function App() {
  const [targets, setTargets] = useState<Target[]>([]);
  // null = "all sources" — no source param forwarded.
  const [selectedSource, setSelectedSource] = useState<string | null>(null);
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
  const [sidebarOpen, setSidebarOpen] = useState(false);

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
    const key = `${selectedId}|${range}|${resolution}|${selectedSource ?? ""}`;
    const isKeyChange = fetchKeyRef.current !== key;
    fetchKeyRef.current = key;
    setError(null);
    // Only clear the chart on a target/range/source change — a plain refresh
    // keeps the current view until the new data arrives, so it doesn't flash
    // empty.
    if (isKeyChange) {
      setCycles(null);
      setPickedSec(null);
    }
    setRefreshing(true);
    let cancelled = false;
    getCycles(selectedId, range, undefined, resolution, selectedSource ?? undefined)
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
  }, [selectedId, range, resolution, refreshTick, selectedSource]);

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
    // Preserve first-seen group_title so the sidebar label honours the config
    // even if only the first target in a group sets it (they all should, but
    // we don't want an empty string from a later target to clobber it).
    const byGroup = new Map<string, { title: string; targets: Target[] }>();
    for (const t of targets) {
      let entry = byGroup.get(t.group);
      if (!entry) {
        entry = { title: t.group_title || t.group, targets: [] };
        byGroup.set(t.group, entry);
      }
      entry.targets.push(t);
    }
    return Array.from(byGroup.entries());
  }, [targets]);

  const selected = targets.find((t) => t.id === selectedId) ?? null;
  const selectedProbeType = selected?.probe_type;
  const targetSources = selected?.sources ?? [];

  useEffect(() => {
    if (selectedProbeType === "http" && !HTTP_RANGES.includes(range)) {
      setRange("-24h");
    }
  }, [selectedProbeType, range]);

  // If the picked source doesn't probe this target, fall back to "all".
  // Otherwise the chart silently filters to a source that has no data here.
  useEffect(() => {
    if (selectedSource && !targetSources.includes(selectedSource)) {
      setSelectedSource(null);
    }
  }, [selectedSource, targetSources]);
  const points = cycles?.points ?? [];
  const latest = points.length ? points[points.length - 1] : null;
  // Pin the chart x-axis to the server's echoed window so clicking 1y vs
  // 30d visibly changes the span even when only a slice has data. Falls back
  // to undefined (uPlot auto-fit) before the first response arrives.
  const fromSec = cycles?.from ? Math.floor(new Date(cycles.from).getTime() / 1000) : undefined;
  const toSec = cycles?.to ? Math.floor(new Date(cycles.to).getTime() / 1000) : undefined;

  // Wide time ranges collapse long MTR paths to a handful of lossy hops.
  const hideZeroLossHops = WIDE_RANGES.includes(range);
  const sourceParam = selectedSource ?? undefined;

  // Mirror the chart's palette assignment so the chip text reads in the same
  // colour as that source's line in the "all" view. Derived from the actual
  // points (not targetSources) so a source with no data in range stays neutral
  // — matching the chart, which only paints sources it has data for.
  const chartPalette = useMemo(() => {
    if (selectedSource != null) return new Map();
    const present = new Set<string>();
    for (const p of points) present.add(p.Source ?? "");
    if (present.size < 2) return new Map();
    return paletteForSorted([...present].sort());
  }, [points, selectedSource]);

  const pickTarget = (id: string) => {
    setSelectedId(id);
    setSidebarOpen(false);
  };

  return (
    <div className={`app ${sidebarOpen ? "sidebar-open" : ""}`}>
      {sidebarOpen && (
        <div className="sidebar-backdrop" onClick={() => setSidebarOpen(false)} />
      )}
      <aside className="sidebar">
        <h1>gosmokeping</h1>
        {groups.length === 0 && <div className="empty">No targets</div>}
        {groups.map(([group, entry]) => {
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
                {entry.title}
                <span className="group-count">{entry.targets.length}</span>
              </button>
              {!collapsed &&
                entry.targets.map((t) => (
                  <button
                    key={t.id}
                    className={`target-item ${t.id === selectedId ? "active" : ""}`}
                    onClick={() => pickTarget(t.id)}
                  >
                    {t.title || t.name}
                  </button>
                ))}
            </div>
          );
        })}
      </aside>

      <main className="main">
        {!selected && (
          <>
            <button
              type="button"
              className="hamburger"
              aria-label="Open target list"
              onClick={() => setSidebarOpen(true)}
            >
              ☰
            </button>
            <div className="empty">Select a target</div>
          </>
        )}
        {selected && (
          <>
            <div className="toolbar">
              <button
                type="button"
                className="hamburger"
                aria-label="Open target list"
                onClick={() => setSidebarOpen(true)}
              >
                ☰
              </button>
              <strong>{selected.title || selected.name}</strong>
              {selected.title && (
                <span className="toolbar-id">{selected.id}</span>
              )}
              <span style={{ color: "#8a93a6" }}>· {selected.probe}</span>
              <div style={{ flex: 1 }} />
              {RANGES.filter(
                (r) => selected.probe_type !== "http" || HTTP_RANGES.includes(r.value),
              ).map((r) => (
                <button
                  key={r.value}
                  className={range === r.value ? "active" : ""}
                  onClick={() => setRange(r.value)}
                >
                  {r.label}
                </button>
              ))}
              {selected.probe_type !== "http" && (
                <>
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
                </>
              )}
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
            {targetSources.length > 0 && (
              <div className="source-chips">
                <span className="source-label">source:</span>
                {targetSources.length === 1 ? (
                  <span className="chip active">{targetSources[0]}</span>
                ) : (
                  <>
                    <button
                      type="button"
                      className={`chip ${selectedSource == null ? "active" : ""}`}
                      onClick={() => setSelectedSource(null)}
                    >
                      all
                    </button>
                    {targetSources.map((s) => {
                      const c = chartPalette.get(s);
                      return (
                        <button
                          key={s}
                          type="button"
                          className={`chip ${selectedSource === s ? "active" : ""}`}
                          style={c ? { color: c.stroke } : undefined}
                          onClick={() => setSelectedSource(s)}
                        >
                          {s}
                        </button>
                      );
                    })}
                  </>
                )}
              </div>
            )}
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
                  source={sourceParam}
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
                    source={sourceParam}
                    hideZeroLoss={hideZeroLossHops && pickedSec == null}
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
                      source={sourceParam}
                      hideZeroLoss={hideZeroLossHops}
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
