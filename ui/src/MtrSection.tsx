import { useEffect, useMemo, useRef, useState } from "react";
import { getHops, type CycleLoss, type HopPoint } from "./api";
import { HopsPath } from "./HopsTable";
import { HopsTable } from "./HopsTable";
import { MtrHeatmap } from "./MtrHeatmap";
import { countDistinct, groupBySource, useCollapsedSources } from "./mtrUtils";
import { lossTextColor } from "./palette";

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
}

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
}: {
  targetId: string;
  refreshTick: number;
  fromSec: number | null;
  toSec: number | null;
  atSec: number | null;
  onResetAt: () => void;
  onCyclePick: (timeSec: number, source?: string) => void;
  source: string;
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
}: {
  targetId: string;
  refreshTick: number;
  fromSec: number | null;
  toSec: number | null;
  atSec: number | null;
  onResetAt: () => void;
  onCyclePick: (timeSec: number, source?: string) => void;
}) {
  const [hops, setHops] = useState<HopPoint[] | null>(null);
  const [cycleLoss, setCycleLoss] = useState<CycleLoss[] | null>(null);
  const [cycleTime, setCycleTime] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const prevKey = useRef<string>("");

  useEffect(() => {
    // Blank to the loading state only when the TARGET changes — a new target
    // has a wholly different path, so the old one is misleading. On a mere
    // cycle re-pick (atSec change) keep the current hops on screen while the
    // new cycle loads: blanking the whole section collapses its height, and the
    // scroll container then clamps to the much shorter page and never scrolls
    // back — the "click jumps me to the top" bug.
    const changed = prevKey.current !== targetId;
    prevKey.current = targetId;
    setErr(null);
    if (changed) setHops(null);
    const controller = new AbortController();
    getHops(targetId, atSec ?? undefined, undefined, controller.signal)
      .then((r) => {
        const rows = r.hops ?? [];
        setHops(rows);
        setCycleLoss(r.target_loss ?? null);
        // Label the header from the rows on screen: atSec is the requested
        // time and the server returns the nearest cycle, not that instant.
        setCycleTime(rows.length > 0 ? rows[0].Time : null);
      })
      .catch((e) => {
        if (e?.name !== "AbortError") setErr(String(e));
      });
    return () => controller.abort();
  }, [targetId, refreshTick, atSec]);

  const groups = useMemo(() => groupBySource(hops ?? []), [hops]);

  const { collapsed, toggle } = useCollapsedSources();
  const headerLabel =
    cycleTime != null
      ? new Date(cycleTime).toLocaleString()
      : atSec != null
      ? new Date(atSec * 1000).toLocaleString()
      : "";

  if (err) return <div className="error">{err}</div>;
  if (hops === null) return <div className="empty">Loading MTR data…</div>;
  if (hops.length === 0) {
    return (
      <div className="chart-wrap">
        {atSec != null && (
          <div className="hops-header">
            <span>Showing cycle at {headerLabel}</span>
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
        // Source arg dropped, exactly as the multi-source branch below does:
        // this fallback is reached because the response happened to collapse to
        // one source, not because the user picked one. Forwarding it set
        // pickedSource, which flips MtrSection's own branch from
        // MultiSourceLayout to SingleSourceLayout — a different child type at
        // the same position, so React discards the subtree and both the path
        // table and the heatmap remount and refetch, defeating the
        // stale-while-revalidate guards each of them implements.
        onCyclePick={(t) => onCyclePick(t)}
        source={groups[0].source}
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
          <span>Showing cycle at {headerLabel}</span>
          <button className="hops-reset" onClick={onResetAt} title="Show latest">
            ← latest
          </button>
        </div>
      )}
      <div className="mtr-sections">
        {groups.map((g) => {
          const isCollapsed = collapsed.has(g.source);
          const endToEndLoss = lossForSource(cycleLoss, g.source);
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
                    style={{
                      color:
                        endToEndLoss == null
                          ? "var(--text-subtle)"
                          : lossTextColor(endToEndLoss, "var(--text-subtle)"),
                    }}
                    title={
                      endToEndLoss == null
                        ? "No target-level loss reported for this cycle"
                        : undefined
                    }
                  >
                    loss {endToEndLoss == null ? "—" : `${endToEndLoss.toFixed(1)}%`}
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

// Loss at the target comes from the cycle's own round counters and from
// nowhere else — hop rows cannot answer how many rounds reached the target
// once each round stops at its own terminal. null means unknown: the server
// predates target_loss, or that source's cycle recorded no measurement.
function lossForSource(cycles: CycleLoss[] | null, source: string): number | null {
  if (cycles == null) return null;
  const row = cycles.find((c) => (c.Source ?? "") === source);
  if (row == null || row.Sent <= 0) return null;
  return row.LossPct;
}
