import type uPlot from "uplot";
import type { CyclePoint } from "./api";

// Window loss is packets lost over packets sent: averaging per-cycle
// percentages weights a 1-ping cycle the same as a 100-ping one.
export function windowLoss(points: CyclePoint[]): number | null {
  let sent = 0;
  let lost = 0;
  for (const p of points) {
    sent += p.Sent;
    lost += p.LossCount;
  }
  return sent > 0 ? (100 * lost) / sent : null;
}

// A bucketed column spans [t, t+step), so the column under a click is the last
// bucket start at or before it — nearest-timestamp lands one column right past
// the midpoint. Raw-tier cycles sit on no grid and stay nearest-wins.
export function cycleAtSec(cycles: number[], stepSec: number, sec: number): number | null {
  if (cycles.length === 0) return null;
  if (stepSec > 0) {
    for (let i = cycles.length - 1; i >= 0; i--) {
      if (cycles[i] <= sec) {
        return sec < cycles[i] + stepSec ? cycles[i] : nearestCycle(cycles, sec);
      }
    }
  }
  return nearestCycle(cycles, sec);
}

function nearestCycle(cycles: number[], sec: number): number {
  let best = cycles[0];
  let bestDist = Math.abs(sec - best);
  for (const t of cycles) {
    const d = Math.abs(sec - t);
    if (d < bestDist) {
      best = t;
      bestDist = d;
    }
  }
  return best;
}

// A partial-loss cycle whose Min is 0 had it poisoned by a fully-lost
// sub-cycle, so the drawn floor is P5 (or the median) rather than 0ms.
export function effectiveMin(p: CyclePoint): number {
  return p.Min === 0 && p.LossPct > 0 ? p.P5 || p.Median : p.Min;
}

// Length-prefixed so no delimiter can appear inside a name: joining on "|"
// alone makes ["a|b","c"] and ["a","b|c"] the same key, and a chart that
// skips its rebuild keeps a solo filter matching none of its series.
export function sourcesKey(sources: string[]): string {
  return sources.map((s) => `${s.length}:${s}`).join("|");
}

export function unixSec(iso: string): number {
  return Math.floor(new Date(iso).getTime() / 1000);
}

// LOG_Y_FLOOR is the smallest value a log y-axis may show; 0 has no log.
// Shared because the band chart and the bar chart are two arms of one
// toggle over the same data and must draw the same gridlines.
export const LOG_Y_FLOOR = 0.01;

// decadeSplits returns one tick per power-of-ten across the visible y range.
// Replaces uPlot's default log splits (which add minor 2/3/5/7 ticks per
// decade) so the grid stays readable at log10.
export function decadeSplits(_u: uPlot, _axisIdx: number, scaleMin: number, scaleMax: number): number[] {
  const lo = Math.floor(Math.log10(Math.max(scaleMin, LOG_Y_FLOOR)));
  const hi = Math.ceil(Math.log10(Math.max(scaleMax, LOG_Y_FLOOR * 10)));
  const decades: number[] = [];
  for (let i = lo; i <= hi; i++) decades.push(Math.pow(10, i));
  const within = decades.filter((v) => v >= scaleMin && v <= scaleMax);
  // A view that never crosses a decade boundary (e.g. a stable target's
  // 25-35ms band) leaves zero power-of-ten ticks inside range — the axis
  // would render with no labels at all. Fall back to evenly spaced ticks.
  return within.length >= 2 ? within : niceLinearTicks(scaleMin, scaleMax);
}

// niceLinearTicks picks a human-friendly step (1/2/5 × 10^n) and returns
// ticks at multiples of it spanning [min, max] — same heuristic most
// charting libraries use for linear axes, used here as the log-axis
// fallback when the visible range doesn't span a full decade.
export function niceLinearTicks(min: number, max: number, targetCount = 5): number[] {
  if (!(max > min)) return [min];
  const rawStep = (max - min) / targetCount;
  const mag = Math.pow(10, Math.floor(Math.log10(rawStep)));
  const norm = rawStep / mag;
  const step = (norm < 1.5 ? 1 : norm < 3 ? 2 : norm < 7 ? 5 : 10) * mag;
  const out: number[] = [];
  for (let v = Math.ceil(min / step) * step; v <= max + step * 1e-9; v += step) {
    out.push(Math.round(v * 1e6) / 1e6);
  }
  return out.length > 0 ? out : [min, max];
}
