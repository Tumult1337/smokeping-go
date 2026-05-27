import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { getHops, type HopPoint } from "./api";
import { HopsPath } from "./HopsTable";
import { HopsTable } from "./HopsTable";
import { MtrHeatmap } from "./MtrHeatmap";
import { lossColor } from "./palette";

interface Props {
  targetId: string;
  refreshTick: number;
  fromSec: number | null;
  toSec: number | null;
  atSec: number | null;
  onResetAt: () => void;
  onCyclePick: (timeSec: number, source?: string) => void;
  // Set when the user picked a source chip; collapses the whole section
  // down to one source-filtered (path + heatmap) pair instead of the
  // multi-source list of collapsibles.
  sourceParam: string | null;
  hideZeroLossHeatmap: boolean;
}

// Persists between page loads. Keyed by source name; missing = expanded.
// Note: this is the same key MtrHeatmap previously used for its in-stack
// collapse — keeping it means a user who had collapsed sources before
// keeps them collapsed under the new unified section layout.
const COLLAPSED_SOURCES_KEY = "gosmokeping.collapsedHopSources";

// MtrSection owns the MTR view for the active target. Two modes:
//
//   - Single-source (sourceParam set, or the target has only one origin):
//     two stacked cards — Path table + heatmap — same as the layout
//     before this refactor.
//
//   - Multi-source (no chip filter and more than one origin probes the
//     target): one collapsible card per source, each containing that
//     source's path table + heatmap. Path data is sliced from a single
//     unfiltered /hops?at fetch; heatmaps fetch their own source-scoped
//     timeline only when their section is expanded.
export function MtrSection({
  targetId,
  refreshTick,
  fromSec,
  toSec,
  atSec,
  onResetAt,
  onCyclePick,
  sourceParam,
  hideZeroLossHeatmap,
}: Props) {
  // Single-source path stays on the existing components — HopsTable's
  // own fetch + render is simpler than re-implementing it here, and the
  // single-source MtrHeatmap is also unchanged.
  if (sourceParam) {
    return (
      <SingleSourceLayout
        targetId={targetId}
        refreshTick={refreshTick}
        fromSec={fromSec}
        toSec={toSec}
        atSec={atSec}
        onResetAt={onResetAt}
        onCyclePick={onCyclePick}
        source={sourceParam}
        hideZeroLossHeatmap={hideZeroLossHeatmap}
      />
    );
  }
  return (
    <MultiSourceLayout
      targetId={targetId}
      refreshTick={refreshTick}
      fromSec={fromSec}
      toSec={toSec}
      atSec={atSec}
      onResetAt={onResetAt}
      onCyclePick={onCyclePick}
      hideZeroLossHeatmap={hideZeroLossHeatmap}
    />
  );
}

function SingleSourceLayout({
  targetId,
  refreshTick,
  fromSec,
  toSec,
  atSec,
  onResetAt,
  onCyclePick,
  source,
  hideZeroLossHeatmap,
}: {
  targetId: string;
  refreshTick: number;
  fromSec: number | null;
  toSec: number | null;
  atSec: number | null;
  onResetAt: () => void;
  onCyclePick: (timeSec: number, source?: string) => void;
  source: string;
  hideZeroLossHeatmap: boolean;
}) {
  return (
    <>
      <div className="chart-wrap">
        <div className="chart-title">
          Path {atSec != null ? "— historical MTR" : "(latest MTR)"}
        </div>
        <HopsTable
          targetId={targetId}
          refreshTick={refreshTick}
          atSec={atSec ?? undefined}
          onResetAt={onResetAt}
          source={source}
          hideZeroLoss={false}
        />
      </div>
      {fromSec != null && toSec != null && (
        <div className="chart-wrap">
          <div className="chart-title">MTR history — per-hop loss</div>
          <MtrHeatmap
            targetId={targetId}
            refreshTick={refreshTick}
            fromSec={fromSec}
            toSec={toSec}
            onCyclePick={onCyclePick}
            selectedSec={atSec ?? undefined}
            source={source}
            hideZeroLoss={hideZeroLossHeatmap}
          />
        </div>
      )}
    </>
  );
}

function MultiSourceLayout({
  targetId,
  refreshTick,
  fromSec,
  toSec,
  atSec,
  onResetAt,
  onCyclePick,
  hideZeroLossHeatmap,
}: {
  targetId: string;
  refreshTick: number;
  fromSec: number | null;
  toSec: number | null;
  atSec: number | null;
  onResetAt: () => void;
  onCyclePick: (timeSec: number, source?: string) => void;
  hideZeroLossHeatmap: boolean;
}) {
  const [hops, setHops] = useState<HopPoint[] | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const prevKey = useRef<string>("");

  useEffect(() => {
    const k = `${targetId}|${atSec ?? ""}`;
    const changed = prevKey.current !== k;
    prevKey.current = k;
    setErr(null);
    if (changed) setHops(null);
    const controller = new AbortController();
    getHops(targetId, atSec ?? undefined, undefined, controller.signal)
      .then((r) => setHops(r.hops ?? []))
      .catch((e) => {
        if (e?.name !== "AbortError") setErr(String(e));
      });
    return () => controller.abort();
  }, [targetId, refreshTick, atSec]);

  const groups = useMemo(() => groupBySource(hops ?? []), [hops]);

  const [collapsed, setCollapsed] = useState<Set<string>>(() => {
    try {
      const raw = typeof localStorage !== "undefined"
        ? localStorage.getItem(COLLAPSED_SOURCES_KEY)
        : null;
      if (!raw) return new Set();
      const arr = JSON.parse(raw);
      return new Set(Array.isArray(arr) ? arr : []);
    } catch {
      return new Set();
    }
  });
  const toggle = useCallback((src: string) => {
    setCollapsed((prev) => {
      const next = new Set(prev);
      if (next.has(src)) next.delete(src);
      else next.add(src);
      try {
        localStorage.setItem(COLLAPSED_SOURCES_KEY, JSON.stringify([...next]));
      } catch {
        // localStorage unavailable — ignore
      }
      return next;
    });
  }, []);

  if (err) return <div className="error">{err}</div>;
  if (hops === null) return <div className="empty">Loading MTR data…</div>;
  if (hops.length === 0) {
    return (
      <div className="chart-wrap">
        {atSec != null && (
          <div className="hops-header">
            <span>Showing cycle at {new Date(atSec * 1000).toLocaleString()}</span>
            <button className="hops-reset" onClick={onResetAt} title="Show latest">
              ← latest
            </button>
          </div>
        )}
        <div className="empty">
          {atSec != null ? "No MTR cycle near this time" : "No hop data yet"}
        </div>
      </div>
    );
  }

  // If the response collapsed to one source (rare in all-view: cluster
  // members offline, or a target without slaves), fall back to the
  // single-source layout so the user doesn't get one needless chevron.
  if (groups.length === 1) {
    return (
      <SingleSourceLayout
        targetId={targetId}
        refreshTick={refreshTick}
        fromSec={fromSec}
        toSec={toSec}
        atSec={atSec}
        onResetAt={onResetAt}
        onCyclePick={onCyclePick}
        source={groups[0].source}
        hideZeroLossHeatmap={hideZeroLossHeatmap}
      />
    );
  }

  // Each section's path bar is scaled to that source's own worst Max, not
  // a cross-source max — keeps short-RTT paths visible when one other
  // source has a 200ms hop dominating the global scale.
  return (
    <>
      {atSec != null && (
        <div className="hops-header">
          <span>Showing cycle at {new Date(atSec * 1000).toLocaleString()}</span>
          <button className="hops-reset" onClick={onResetAt} title="Show latest">
            ← latest
          </button>
        </div>
      )}
      <div className="mtr-sections">
        {groups.map((g) => {
          const isCollapsed = collapsed.has(g.source);
          const worstLoss = g.hops.reduce(
            (m, h) => (h.LossPct > m ? h.LossPct : m),
            0,
          );
          const scale = Math.max(1, ...g.hops.map((h) => h.Max));
          return (
            <div key={g.source || "(unspecified)"} className="mtr-section">
              <button
                type="button"
                className="mtr-section-heading"
                aria-expanded={!isCollapsed}
                onClick={() => toggle(g.source)}
              >
                <span className="mtr-section-chevron">
                  {isCollapsed ? "▶" : "▼"}
                </span>
                <span className="mtr-section-name">
                  {g.source || "(unspecified)"}
                </span>
                <span className="mtr-section-summary">
                  {countDistinct(g.hops)} hop
                  {countDistinct(g.hops) === 1 ? "" : "s"}
                  <span
                    className="mtr-section-worst"
                    style={{ color: lossColor(worstLoss, "#4a5160") }}
                  >
                    max {worstLoss.toFixed(1)}%
                  </span>
                </span>
              </button>
              {!isCollapsed && (
                <div className="mtr-section-body">
                  <HopsPath
                    source={g.source}
                    time={g.time}
                    rows={g.hops}
                    scale={scale}
                    showSourceHeading={false}
                  />
                  {fromSec != null && toSec != null && (
                    <MtrHeatmap
                      targetId={targetId}
                      refreshTick={refreshTick}
                      fromSec={fromSec}
                      toSec={toSec}
                      // Drop the source arg so a heatmap click in
                      // multi-source mode just pins the timestamp across
                      // every section — clicking source A's heatmap no
                      // longer collapses the whole MtrSection to source A
                      // (which would happen if pickedSource got set).
                      onCyclePick={(t) => onCyclePick(t)}
                      selectedSec={atSec ?? undefined}
                      source={g.source}
                      hideZeroLoss={hideZeroLossHeatmap}
                    />
                  )}
                </div>
              )}
            </div>
          );
        })}
      </div>
    </>
  );
}

interface HopsGroup {
  source: string;
  time: string;
  hops: HopPoint[];
}

function groupBySource(hops: HopPoint[]): HopsGroup[] {
  const order: string[] = [];
  const byKey = new Map<string, HopsGroup>();
  for (const h of hops) {
    const existing = byKey.get(h.Source);
    if (existing) {
      existing.hops.push(h);
    } else {
      order.push(h.Source);
      byKey.set(h.Source, { source: h.Source, time: h.Time, hops: [h] });
    }
  }
  return order.map((s) => byKey.get(s)!);
}

function countDistinct(hops: HopPoint[]): number {
  const seen = new Set<number>();
  for (const h of hops) seen.add(h.Index);
  return seen.size;
}
