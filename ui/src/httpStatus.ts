// HTTP status → color / label / class. Shared by HttpChart (bars, summary,
// tooltip) and StatusStrip so both agree on what a 404 or a network error looks
// like. One source of truth for the status palette.

// Color by status class. Network error (status == 0) gets its own grey so you
// can tell "server said no" apart from "never reached server".
export function colorFor(status: number): string {
  if (status === 0) return "#6b7280";
  if (status >= 500) return "#ef4444";
  if (status >= 400) return "#f59e0b";
  if (status >= 300) return "#60a5fa";
  if (status >= 200) return "#5eead4";
  return "#8a93a6";
}

export function statusLabel(status: number): string {
  if (status === 0) return "network error";
  return String(status);
}

export type StatusClass = "2xx" | "3xx" | "4xx" | "5xx" | "err";

export function statusClass(status: number): StatusClass {
  if (status >= 500) return "5xx";
  if (status >= 400) return "4xx";
  if (status >= 300) return "3xx";
  if (status >= 200) return "2xx";
  return "err";
}

// A request is a success when the probe would NOT count it as loss: status in
// [200, 400). Mirrors internal/probe/http.go:204 (`< 200 || >= 400` is loss).
export function isSuccess(status: number): boolean {
  return status >= 200 && status < 400;
}
