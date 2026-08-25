import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import Fuse from "fuse.js";
import {
  listTargets,
  listSources,
  getCycles,
  type Target,
  type CyclesResponse,
} from "./api";
import { SmokeChart } from "./SmokeChart";
import { SmokeBarChart } from "./SmokeBarChart";
import { HttpChart } from "./HttpChart";
import { MtrSection } from "./MtrSection";
import { paletteForSorted, lossColor } from "./palette";
import { effectiveMin, windowLoss } from "./chartUtils";
import { OverviewView, type SortKey, type SortDir } from "./OverviewView";
import type { OverviewWindow } from "./api";

type Range = "-1h" | "-6h" | "-24h" | "-7d" | "-30d" | "-180d" | "-365d";
type ChartStyle = "band" | "bars";
type YScale = "lin" | "log";
const CHART_STYLE_KEY = "gosmokeping.chartStyle";
const Y_SCALE_KEY = "gosmokeping.yScale";
const COLLAPSED_GROUPS_KEY = "gosmokeping.collapsedGroups";
// Reserved collapse key for the By-slave section. It shares the persisted
// collapsedGroups set with real target groups; the leading underscores keep it
// clear of any group name a config could define.
const BY_SLAVE_KEY = "__byslave";
const OVERVIEW_WINDOW_KEY = "gosmokeping.overviewWindow";
const OVERVIEW_SORT_KEY = "gosmokeping.overviewSort";

const VALID_RANGES: Range[] = ["-1h", "-6h", "-24h", "-7d", "-30d", "-180d", "-365d"];
const VALID_OVERVIEW_WINDOWS: OverviewWindow[] = ["-1h", "-6h", "-24h"];
const VALID_SORT_KEYS: SortKey[] = [
  "target",
  "loss_avg",
  "loss_max",
  "rtt_median",
  "rtt_p95",
  "rtt_max",
  "worst_source",
  "last_seen",
];

// readUrlState plucks the shareable-link params the app writes on every
// state change. Kept loose: unknown values fall through to defaults so a
// malformed URL never wedges the app.
type UrlState = {
  target: string | null;
  range: Range | null;
  mode: ChartStyle | null;
  scale: YScale | null;
  source: string | null;
  pickedSec: number | null;
  zoom: { from: number; to: number } | null;
  view: "overview" | null;
  overviewWindow: OverviewWindow | null;
  overviewSort: SortKey | null;
  overviewDir: SortDir | null;
  slaveView: string | null;
};
function readUrlState(): UrlState {
  if (typeof window === "undefined") {
    return {
      target: null, range: null, mode: null, scale: null, source: null,
      pickedSec: null, zoom: null, view: null, overviewWindow: null,
      overviewSort: null, overviewDir: null, slaveView: null,
    };
  }
  const p = new URLSearchParams(window.location.search);
  const range = p.get("range") as Range | null;
  const mode = p.get("mode");
  const scale = p.get("scale");
  const tRaw = p.get("t");
  const t = tRaw ? Number(tRaw) : NaN;
  const z0Raw = p.get("z0");
  const z1Raw = p.get("z1");
  const z0 = z0Raw ? Number(z0Raw) : NaN;
  const z1 = z1Raw ? Number(z1Raw) : NaN;
  const zoom =
    Number.isFinite(z0) && Number.isFinite(z1) && z1 > z0
      ? { from: z0, to: z1 }
      : null;
  const view = p.get("view") === "overview" ? "overview" : null;
  const ow = p.get("window") as OverviewWindow | null;
  const os = p.get("sort") as SortKey | null;
  const od = p.get("dir");
  return {
    target: p.get("target"),
    range: range && VALID_RANGES.includes(range) ? range : null,
    mode: mode === "bars" || mode === "band" ? mode : null,
    scale: scale === "log" || scale === "lin" ? scale : null,
    source: p.get("source"),
    pickedSec: Number.isFinite(t) ? t : null,
    zoom,
    view,
    overviewWindow: ow && VALID_OVERVIEW_WINDOWS.includes(ow) ? ow : null,
    overviewSort: os && VALID_SORT_KEYS.includes(os) ? os : null,
    overviewDir: od === "asc" || od === "desc" ? od : null,
    slaveView: p.get("view") === "slave" ? p.get("slave") : null,
  };
}

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
  // URL state is read once at mount; later writes go through the sync effect
  // so the address bar tracks the UI without forcing React to re-parse on
  // every render.
  const initialUrl = useMemo(() => readUrlState(), []);
  const [targets, setTargets] = useState<Target[]>([]);
  const [sources, setSources] = useState<string[]>([]);
  const [slaveView, setSlaveView] = useState<string | null>(initialUrl.slaveView);
  // null = "all sources" — no source param forwarded.
  const [selectedSource, setSelectedSource] = useState<string | null>(initialUrl.source);
  const [selectedId, setSelectedId] = useState<string | null>(initialUrl.target);
  const [range, setRange] = useState<Range>(initialUrl.range ?? "-24h");
  const [cycles, setCycles] = useState<CyclesResponse | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [autoRefresh, setAutoRefresh] = useState(true);
  const [refreshTick, setRefreshTick] = useState(0);
  const [refreshing, setRefreshing] = useState(false);
  const [chartStyle, setChartStyle] = useState<ChartStyle>(() => {
    if (initialUrl.mode) return initialUrl.mode;
    const saved = typeof localStorage !== "undefined" ? localStorage.getItem(CHART_STYLE_KEY) : null;
    return saved === "bars" ? "bars" : "band";
  });
  const [yScale, setYScale] = useState<YScale>(() => {
    if (initialUrl.scale) return initialUrl.scale;
    const saved = typeof localStorage !== "undefined" ? localStorage.getItem(Y_SCALE_KEY) : null;
    return saved === "log" ? "log" : "lin";
  });
  // Overview state. View mode: when true (or no target picked), the main pane
  // shows the fleet overview instead of a single target. Window/sort persist
  // in URL + localStorage so a reload restores the view exactly.
  const [overviewView, setOverviewView] = useState<boolean>(initialUrl.view === "overview");
  const [overviewWindow, setOverviewWindow] = useState<OverviewWindow>(() => {
    if (initialUrl.overviewWindow) return initialUrl.overviewWindow;
    const saved = typeof localStorage !== "undefined" ? localStorage.getItem(OVERVIEW_WINDOW_KEY) : null;
    return saved && (VALID_OVERVIEW_WINDOWS as string[]).includes(saved)
      ? (saved as OverviewWindow)
      : "-1h";
  });
  const [overviewSort, setOverviewSort] = useState<SortKey>(() => {
    if (initialUrl.overviewSort) return initialUrl.overviewSort;
    const saved = typeof localStorage !== "undefined" ? localStorage.getItem(OVERVIEW_SORT_KEY) : null;
    if (!saved) return "loss_avg";
    const [k] = saved.split("|");
    return (VALID_SORT_KEYS as string[]).includes(k) ? (k as SortKey) : "loss_avg";
  });
  const [overviewDir, setOverviewDir] = useState<SortDir>(() => {
    if (initialUrl.overviewDir) return initialUrl.overviewDir;
    const saved = typeof localStorage !== "undefined" ? localStorage.getItem(OVERVIEW_SORT_KEY) : null;
    if (!saved) return "desc";
    const parts = saved.split("|");
    return parts[1] === "asc" ? "asc" : "desc";
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
  // "push" means the next URL sync creates a history entry (user navigation);
  // null means replace (derived correction / refinement). Set by user-action
  // handlers, read+cleared by the URL-sync effect (see Task 5).
  const navIntentRef = useRef<"push" | null>(null);
  // Historical MTR pin: when set, HopsTable and the heatmap marker
  // show the cycle at that unix-seconds timestamp. Cleared when the target
  // or range changes, or when the user clicks "← latest". Initial value
  // comes from ?t=<unix> so a shared link lands on the chosen cycle.
  const [pickedSec, setPickedSec] = useState<number | null>(initialUrl.pickedSec);
  // Source override forwarded by an MtrHeatmap click: the probe origin whose
  // data dominated the clicked column (worst loss). The HopsTable uses this
  // in preference to the global source filter so a click on a slave's lossy
  // bucket actually shows the slave's path, not the master's clean cycle for
  // the same timestamp. Cleared on "← latest", target change, etc.
  const [pickedSource, setPickedSource] = useState<string | null>(null);
  const handleCyclePick = useCallback((timeSec: number, source?: string) => {
    setPickedSec(timeSec);
    // Chart clicks (SmokeChart / SmokeBarChart) don't pass a source, so
    // pickedSource resets to null and the HopsTable reverts to the global
    // filter. Heatmap clicks pass the column's worst-loss source. Explicit
    // "" (pre-cluster row, no source tag) also clears the override.
    setPickedSource(source && source !== "" ? source : null);
  }, []);
  const [zoom, setZoom] = useState<{ from: number; to: number } | null>(initialUrl.zoom);
  const [sidebarOpen, setSidebarOpen] = useState(false);
  // Tracks which source is currently soloed in the chart (set by onSoloChange
  // callback). Used to filter the window stats block below the chart.
  const [chartSoloSource, setChartSoloSource] = useState<string | null>(null);
  const [searchQuery, setSearchQuery] = useState("");
  const searchInputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    try {
      localStorage.setItem(CHART_STYLE_KEY, chartStyle);
    } catch {
      // localStorage unavailable — ignore
    }
  }, [chartStyle]);

  useEffect(() => {
    try {
      localStorage.setItem(Y_SCALE_KEY, yScale);
    } catch {
      // localStorage unavailable — ignore
    }
  }, [yScale]);

  useEffect(() => {
    try {
      localStorage.setItem(COLLAPSED_GROUPS_KEY, JSON.stringify([...collapsedGroups]));
    } catch {
      // localStorage unavailable — ignore
    }
  }, [collapsedGroups]);

  useEffect(() => {
    try {
      localStorage.setItem(OVERVIEW_WINDOW_KEY, overviewWindow);
    } catch {
      // localStorage unavailable — ignore
    }
  }, [overviewWindow]);

  useEffect(() => {
    try {
      localStorage.setItem(OVERVIEW_SORT_KEY, `${overviewSort}|${overviewDir}`);
    } catch {
      // localStorage unavailable — ignore
    }
  }, [overviewSort, overviewDir]);

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
        // Honor URL target if it exists; otherwise leave selectedId null so
        // the main pane lands on the overview. Stale bookmarks (target=
        // pointing at a removed config entry) also fall through to overview
        // rather than silently switching to an unrelated target.
        setSelectedId((cur) => {
          if (cur && t.some((x) => x.id === cur)) return cur;
          return null;
        });
      })
      .catch((e) => setError(String(e)));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    listSources()
      .then((r) => setSources(r.sources))
      .catch(() => setSources([]));
  }, []);

  const fromArg = zoom ? String(zoom.from) : range;
  const toArg = zoom ? String(zoom.to) : undefined;

  useEffect(() => {
    if (!selectedId) return;
    const zoomKey = zoom ? `${zoom.from}-${zoom.to}` : "";
    const key = `${selectedId}|${range}|${selectedSource ?? ""}|${zoomKey}`;
    const prevKey = fetchKeyRef.current;
    const isKeyChange = prevKey !== key;
    fetchKeyRef.current = key;
    setError(null);
    // Only clear the chart on a target/range/source change — a plain refresh
    // keeps the current view until the new data arrives, so it doesn't flash
    // empty. Skip the pickedSec wipe on the very first fetch so a URL like
    // ?target=…&t=… lands on the chosen cycle instead of reverting to latest.
    if (isKeyChange) {
      setCycles(null);
      if (prevKey !== "") {
        setPickedSec(null);
        setPickedSource(null);
      }
    }
    setRefreshing(true);
    let cancelled = false;
    getCycles(selectedId, fromArg, toArg, selectedSource ?? undefined)
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
  }, [selectedId, range, refreshTick, selectedSource, zoom]);

  useEffect(() => {
    if (!autoRefresh) return;
    const id = setInterval(() => {
      setRefreshTick((n) => n + 1);
    }, AUTO_REFRESH_MS);
    return () => clearInterval(id);
  }, [autoRefresh]);

  // Mirror UI state into the URL so the current view is shareable via copy-
  // paste. replaceState (not pushState) keeps the back button sane — we're
  // not creating history entries for every range toggle.
  useEffect(() => {
    if (typeof window === "undefined") return;
    const p = new URLSearchParams();
    if (selectedId) p.set("target", selectedId);
    if (range !== "-24h") p.set("range", range);
    if (chartStyle !== "band") p.set("mode", chartStyle);
    if (yScale !== "lin") p.set("scale", yScale);
    if (selectedSource) p.set("source", selectedSource);
    if (pickedSec != null) p.set("t", String(pickedSec));
    if (zoom) {
      p.set("z0", String(zoom.from));
      p.set("z1", String(zoom.to));
    }
    // Overview view: only encoded when active *and* there's a selected target
    // — without target= the URL implicitly means overview, so the extra param
    // is redundant.
    if (overviewView && selectedId) p.set("view", "overview");
    if (slaveView && sources.includes(slaveView)) {
      p.set("view", "slave");
      p.set("slave", slaveView);
    }
    if (overviewView || !selectedId) {
      if (overviewWindow !== "-1h") p.set("window", overviewWindow);
      if (overviewSort !== "loss_avg") p.set("sort", overviewSort);
      if (overviewDir !== "desc") p.set("dir", overviewDir);
    }
    const qs = p.toString();
    const url = `${window.location.pathname}${qs ? `?${qs}` : ""}${window.location.hash}`;
    const current = `${window.location.pathname}${window.location.search}${window.location.hash}`;
    if (url !== current) {
      if (navIntentRef.current === "push") {
        window.history.pushState(null, "", url);
      } else {
        window.history.replaceState(null, "", url);
      }
    }
    navIntentRef.current = null;
  }, [
    selectedId, range, chartStyle, yScale, selectedSource, pickedSec, zoom,
    overviewView, overviewWindow, overviewSort, overviewDir, slaveView, sources,
  ]);

  // Browser Back/Forward: rehydrate all URL-encoded state. Because we set state
  // *from* the URL, the sync effect recomputes the same URL and its equality
  // guard skips any write — no echo, no re-push. pickedSource is not URL-
  // encoded, so it resets to null (matching its target/range-change reset).
  useEffect(() => {
    const onPop = () => {
      const u = readUrlState();
      setSelectedId(u.target);
      setRange(u.range ?? "-24h");
      setSelectedSource(u.source);
      setOverviewView(u.view === "overview");
      setSlaveView(u.slaveView);
      setOverviewWindow(u.overviewWindow ?? "-1h");
      setOverviewSort(u.overviewSort ?? "loss_avg");
      setOverviewDir(u.overviewDir ?? "desc");
      setZoom(u.zoom);
      setPickedSec(u.pickedSec);
      setPickedSource(null);
      setChartStyle(u.mode ?? "band");
      setYScale(u.scale ?? "lin");
    };
    window.addEventListener("popstate", onPop);
    return () => window.removeEventListener("popstate", onPop);
  }, []);

  const refresh = useCallback(() => {
    setRefreshTick((n) => n + 1);
  }, []);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const tag = (e.target as HTMLElement).tagName;
      if (tag === "INPUT" || tag === "TEXTAREA") return;
      if (e.key === "/" || (e.key === "k" && (e.ctrlKey || e.metaKey))) {
        e.preventDefault();
        searchInputRef.current?.focus();
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
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

  const fuse = useMemo(
    () =>
      new Fuse(targets, {
        keys: ["title", "name", "host", "id"],
        threshold: 0.4,
        includeScore: true,
      }),
    [targets],
  );

  const searchResults = useMemo(() => {
    const q = searchQuery.trim();
    if (!q) return null;
    return fuse.search(q).map((r) => r.item);
  }, [fuse, searchQuery]);

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
  // Guard on targets being loaded: on initial mount targetSources is [] while
  // the fetch is in flight, which would incorrectly clear a ?source= URL param.
  useEffect(() => {
    if (targets.length === 0) return;
    if (selectedSource && !targetSources.includes(selectedSource)) {
      setSelectedSource(null);
    }
  }, [selectedSource, targetSources, targets.length]);
  const points = cycles?.points ?? [];
  // Aggregate stats across the window. When a source is soloed in the chart,
  // filter to that source so the numbers match what's visible.
  const windowStats = useMemo(() => {
    const filtered = chartSoloSource != null
      ? points.filter((p) => (p.Source ?? "") === chartSoloSource)
      : points;
    if (filtered.length === 0) return null;
    const valid = filtered.filter((p) => p.LossPct < 100);
    const loss = windowLoss(filtered);
    const maxLoss = filtered.reduce((m, p) => (p.LossPct > m ? p.LossPct : m), 0);
    if (valid.length === 0) return { median: null, p95: null, min: null, max: null, loss, maxLoss };
    const median = valid.reduce((s, p) => s + p.Median, 0) / valid.length;
    const p95 = valid.reduce((s, p) => s + p.P95, 0) / valid.length;
    const min = valid.reduce((m, p) => {
      const floor = effectiveMin(p);
      return floor > 0 && floor < m ? floor : m;
    }, Infinity);
    const max = valid.reduce((m, p) => (p.Max > m ? p.Max : m), -Infinity);
    return { median, p95, min: isFinite(min) ? min : null, max: isFinite(max) ? max : null, loss, maxLoss };
  }, [points, chartSoloSource]);
  // Pin the chart x-axis to the server's echoed window so clicking 1y vs
  // 30d visibly changes the span even when only a slice has data. Falls back
  // to undefined (uPlot auto-fit) before the first response arrives.
  const fromSec = cycles?.from ? Math.floor(new Date(cycles.from).getTime() / 1000) : undefined;
  const toSec = cycles?.to ? Math.floor(new Date(cycles.to).getTime() / 1000) : undefined;

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

  const pickTarget = (id: string, source?: string) => {
    if (id !== selectedId || overviewView || slaveView !== null) navIntentRef.current = "push";
    setSelectedId(id);
    setSlaveView(null);
    setOverviewView(false);
    // Sticky source filter: an explicit source (slave-overview row click) wins,
    // otherwise the current filter carries over to the new target. The
    // targetSources effect below clears it if the new target isn't probed by
    // that source, so this can't strand the chart on an empty filter.
    setSelectedSource(source ?? selectedSource);
    setZoom(null);
    setPickedSec(null);
    setPickedSource(null);
    // Clear any soloed source from the previous target. The chart only resets
    // this when its source set changes, so switching between two targets with
    // an identical source set would otherwise leak a stale solo filter into
    // windowStats and report stats for the wrong source subset.
    setChartSoloSource(null);
    setSidebarOpen(false);
  };

  const openOverview = () => {
    if (selectedId !== null || slaveView !== null || overviewView) navIntentRef.current = "push";
    setSelectedId(null);
    setSlaveView(null);
    setOverviewView(false);
    setSelectedSource(null);
    setZoom(null);
    setPickedSec(null);
    setPickedSource(null);
    setChartSoloSource(null);
    setSidebarOpen(false);
  };

  const pickSlave = (name: string) => {
    if (name !== slaveView || selectedId !== null || overviewView) navIntentRef.current = "push";
    setSlaveView(name);
    setSelectedId(null);
    setOverviewView(false);
    setSelectedSource(null);
    setZoom(null);
    setPickedSec(null);
    setPickedSource(null);
    setChartSoloSource(null);
    setSidebarOpen(false);
  };

  // Slave overview takes precedence when a slave is selected. Otherwise the
  // overview shows when no target is picked (or explicit overview view).
  const showSlaveOverview = slaveView != null && sources.includes(slaveView);
  const showOverview = !showSlaveOverview && (!selectedId || overviewView);

  return (
    <div className={`app ${sidebarOpen ? "sidebar-open" : ""}`}>
      {sidebarOpen && (
        <div className="sidebar-backdrop" onClick={() => setSidebarOpen(false)} />
      )}
      <aside className="sidebar">
        <h1>gosmokeping</h1>
        <div className="search-wrap">
          <input
            ref={searchInputRef}
            className="search-input"
            type="search"
            placeholder="Search targets… ( / )"
            value={searchQuery}
            onChange={(e) => setSearchQuery(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Escape") {
                setSearchQuery("");
                searchInputRef.current?.blur();
              }
            }}
            aria-label="Search targets"
          />
        </div>
        <button
          type="button"
          className={`target-item overview-tab ${showOverview ? "active" : ""}`}
          onClick={openOverview}
        >
          Overview
        </button>
        {sources.length > 1 && (
          <div className="slave-section">
            <button
              type="button"
              className="group-title"
              aria-expanded={!collapsedGroups.has(BY_SLAVE_KEY)}
              onClick={() => toggleGroup(BY_SLAVE_KEY)}
            >
              <span className="group-caret">
                {collapsedGroups.has(BY_SLAVE_KEY) ? "▸" : "▾"}
              </span>
              By slave
              <span className="group-count">{sources.length}</span>
            </button>
            {!collapsedGroups.has(BY_SLAVE_KEY) &&
              sources.map((src) => (
                <button
                  key={src}
                  type="button"
                  className={`target-item slave-item ${
                    slaveView === src ? "active" : ""
                  }`}
                  onClick={() => pickSlave(src)}
                >
                  {src}
                </button>
              ))}
          </div>
        )}
        {searchResults !== null ? (
          searchResults.length === 0 ? (
            <div className="empty" style={{ padding: "16px 0" }}>No matches</div>
          ) : (
            searchResults.map((t) => (
              <button
                key={t.id}
                className={`target-item ${t.id === selectedId ? "active" : ""}`}
                onClick={() => pickTarget(t.id)}
              >
                {t.title || t.name}
              </button>
            ))
          )
        ) : (
          <>
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
          </>
        )}
      </aside>

      <main className="main">
        {(showOverview || showSlaveOverview) && (
          <OverviewView
            window={overviewWindow}
            onWindowChange={(w) => {
              if (w !== overviewWindow) navIntentRef.current = "push";
              setOverviewWindow(w);
            }}
            source={showSlaveOverview ? slaveView ?? undefined : undefined}
            sort={overviewSort}
            dir={overviewDir}
            onSortChange={(s, d) => {
              setOverviewSort(s);
              setOverviewDir(d);
            }}
            autoRefresh={autoRefresh}
            onAutoRefreshChange={setAutoRefresh}
            refreshTick={refreshTick}
            onRefresh={refresh}
            onOpenSidebar={() => setSidebarOpen(true)}
            onPickTarget={pickTarget}
          />
        )}
        {!showOverview && !showSlaveOverview && selected && (
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
              <span style={{ color: "var(--text-muted)" }}>· {selected.probe}</span>
              <div style={{ flex: 1 }} />
              <div className="segmented" role="group" aria-label="Time range">
                {RANGES.filter(
                  (r) => selected.probe_type !== "http" || HTTP_RANGES.includes(r.value),
                ).map((r) => (
                  <button
                    key={r.value}
                    className={range === r.value ? "active" : ""}
                    aria-pressed={range === r.value}
                    onClick={() => {
                      if (r.value !== range) navIntentRef.current = "push";
                      setRange(r.value);
                      setZoom(null);
                    }}
                  >
                    {r.label}
                  </button>
                ))}
              </div>
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
                  <div className="toolbar-sep" />
                  <button
                    className={yScale === "log" ? "active" : ""}
                    onClick={() => setYScale(yScale === "log" ? "lin" : "log")}
                    title="Toggle log-scale y-axis (useful when outliers compress the band)"
                  >
                    log
                  </button>
                </>
              )}
              {zoom && (
                <button
                  onClick={() => setZoom(null)}
                  title="Reset zoom to selected range"
                >
                  reset zoom
                </button>
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
                  color: "var(--text-muted)",
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
                      onClick={() => {
                        if (selectedSource !== null) navIntentRef.current = "push";
                        setSelectedSource(null);
                      }}
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
                          onClick={() => {
                            if (selectedSource !== s) navIntentRef.current = "push";
                            setSelectedSource(s);
                          }}
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
                  onZoomChange={setZoom}
                  fromArg={fromArg}
                  toArg={toArg}
                />
              </div>
            ) : (
              <div className="chart-wrap">
                <div className="chart-title">Latency</div>
                {chartStyle === "band" ? (
                  <SmokeChart
                    points={points}
                    fromSec={fromSec}
                    toSec={toSec}
                    yScale={yScale}
                    onCyclePick={handleCyclePick}
                    onZoomChange={setZoom}
                    onSoloChange={setChartSoloSource}
                    loading={cycles === null}
                  />
                ) : (
                  <SmokeBarChart
                    points={points}
                    fromSec={fromSec}
                    toSec={toSec}
                    yScale={yScale}
                    onCyclePick={handleCyclePick}
                    onZoomChange={setZoom}
                    onSoloChange={setChartSoloSource}
                    loading={cycles === null}
                  />
                )}
                {windowStats && (
                  <div className="stats" style={{ marginTop: 12 }}>
                    <span>
                      avg cycle median:{" "}
                      <strong>
                        {windowStats.median == null ? "—" : `${windowStats.median.toFixed(1)}ms`}
                      </strong>
                    </span>
                    <span>
                      avg cycle p95:{" "}
                      <strong>
                        {windowStats.p95 == null ? "—" : `${windowStats.p95.toFixed(1)}ms`}
                      </strong>
                    </span>
                    <span>
                      min/max:{" "}
                      <strong>
                        {windowStats.min == null || windowStats.max == null
                          ? "— / —"
                          : `${windowStats.min.toFixed(1)} / ${windowStats.max.toFixed(1)}ms`}
                      </strong>
                    </span>
                    <span>
                      loss:{" "}
                      {windowStats.loss == null ? (
                        <strong>—</strong>
                      ) : (
                        <strong style={{ color: lossColor(windowStats.loss, "#8a93a6") }}>
                          {windowStats.loss.toFixed(1)}%
                        </strong>
                      )}{" "}
                      <span style={{ color: "var(--text-muted)" }}>
                        (max{" "}
                        <strong style={{ color: lossColor(windowStats.maxLoss, "#8a93a6") }}>
                          {windowStats.maxLoss.toFixed(1)}%
                        </strong>
                        )
                      </span>
                    </span>
                  </div>
                )}
              </div>
            )}
            {(selected.probe_type === "mtr" || selected.probe_type === "icmp") && (
              <MtrSection
                targetId={selected.id}
                refreshTick={refreshTick}
                fromSec={fromSec ?? null}
                toSec={toSec ?? null}
                atSec={pickedSec}
                onResetAt={() => {
                  setPickedSec(null);
                  setPickedSource(null);
                }}
                onCyclePick={handleCyclePick}
                sourceParam={pickedSource ?? sourceParam ?? null}
              />
            )}
          </>
        )}
      </main>
    </div>
  );
}
