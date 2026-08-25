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
