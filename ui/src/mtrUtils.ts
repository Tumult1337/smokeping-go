import { useCallback, useState } from "react";
import type { HopPoint } from "./api";

// COLLAPSED_SOURCES_KEY is the localStorage key shared by MtrHeatmap and
// MtrSection so a user who collapses a source in one place stays collapsed
// when the same source appears in the other. Lives here instead of either
// component so the contract is one-sided — changing the key updates both.
export const COLLAPSED_SOURCES_KEY = "gosmokeping.collapsedHopSources";

export interface HopsGroup {
  source: string;
  // First row's timestamp — the representative the per-source headings show.
  time: string;
  hops: HopPoint[];
}

// groupBySource buckets hops by their Source tag while preserving the
// first-seen order, so the per-source heatmaps/sections render in the
// same order each refresh. Missing Source is coerced to "" so a row from
// a pre-cluster (untagged) write doesn't break grouping.
export function groupBySource(hops: HopPoint[]): HopsGroup[] {
  const order: string[] = [];
  const byKey = new Map<string, HopsGroup>();
  for (const h of hops) {
    const s = h.Source ?? "";
    const existing = byKey.get(s);
    if (existing) {
      existing.hops.push(h);
    } else {
      order.push(s);
      byKey.set(s, { source: s, time: h.Time, hops: [h] });
    }
  }
  return order.map((s) => byKey.get(s)!);
}

// countDistinct returns the number of distinct hop indices in a group —
// what the section header shows as "N hops". Counts indices, not row count,
// because a bucketed timeline emits one row per (bucket, ttl, hop_addr).
export function countDistinct(hops: HopPoint[]): number {
  const seen = new Set<number>();
  for (const h of hops) seen.add(h.Index);
  return seen.size;
}

// useCollapsedSources owns the persistent collapse set keyed by source
// name. Both MtrHeatmap and MtrSection mount it; they share storage via
// COLLAPSED_SOURCES_KEY so a chevron click in one place mirrors in the
// other on the next render of either. The setter writes through synchronously
// so a refresh tick during a click doesn't lose the toggle.
export function useCollapsedSources(): {
  collapsed: Set<string>;
  toggle: (src: string) => void;
} {
  const [collapsed, setCollapsed] = useState<Set<string>>(() => {
    try {
      const raw =
        typeof localStorage !== "undefined"
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
  return { collapsed, toggle };
}
