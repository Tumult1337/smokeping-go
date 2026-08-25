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
