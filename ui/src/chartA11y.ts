export type ChartKind = "band" | "bars" | "http" | "heatmap" | "loss";

export interface KeyInput {
  key: string;
  shiftKey: boolean;
  ctrlKey: boolean;
  altKey: boolean;
  metaKey: boolean;
  repeat: boolean;
  isComposing: boolean;
}

export type KeyAction =
  | { type: "step"; direction: 1 | -1; large: boolean }
  | { type: "edge"; edge: "oldest" | "newest" }
  | { type: "row"; direction: 1 | -1 }
  | { type: "pick" }
  | { type: "zoomIn" }
  | { type: "zoomOut" }
  | { type: "zoomReset" }
  | { type: "escape" };

export interface ZoomWindow {
  from: number;
  to: number;
}

export type ZoomResult = { changed: false } | { changed: true; window: ZoomWindow | null };

export interface TimeFormat {
  locale?: string;
  timeZone?: string;
}

export const ZOOM_FACTOR = 2;
export const MIN_ZOOM_SPAN_SEC = 1;
export const LARGE_STEP_DIVISOR = 10;

const ZOOM_KINDS: readonly ChartKind[] = ["band", "bars", "http"];

function supportsZoom(kind: ChartKind): boolean {
  return ZOOM_KINDS.includes(kind);
}

export function decodeKey(input: KeyInput, kind: ChartKind): KeyAction | null {
  if (input.ctrlKey || input.altKey || input.metaKey || input.isComposing) return null;
  const { key, shiftKey, repeat } = input;
  switch (key) {
    case "ArrowRight":
      return { type: "step", direction: 1, large: shiftKey };
    case "ArrowLeft":
      return { type: "step", direction: -1, large: shiftKey };
    case "PageUp":
      return shiftKey ? null : { type: "step", direction: 1, large: true };
    case "PageDown":
      return shiftKey ? null : { type: "step", direction: -1, large: true };
    case "ArrowUp":
      return shiftKey || kind !== "heatmap" ? null : { type: "row", direction: -1 };
    case "ArrowDown":
      return shiftKey || kind !== "heatmap" ? null : { type: "row", direction: 1 };
    case "Home":
      return shiftKey || repeat ? null : { type: "edge", edge: "oldest" };
    case "End":
      return shiftKey || repeat ? null : { type: "edge", edge: "newest" };
    case "Enter":
    case " ":
      return shiftKey || repeat || kind === "http" ? null : { type: "pick" };
    case "+":
      return repeat || !supportsZoom(kind) ? null : { type: "zoomIn" };
    case "=":
      return shiftKey || repeat || !supportsZoom(kind) ? null : { type: "zoomIn" };
    case "-":
      return shiftKey || repeat || !supportsZoom(kind) ? null : { type: "zoomOut" };
    case "0":
      return shiftKey || repeat || !supportsZoom(kind) ? null : { type: "zoomReset" };
    case "Escape":
      return shiftKey || repeat ? null : { type: "escape" };
    default:
      return null;
  }
}

export function largeStep(sampleCount: number): number {
  return Math.max(1, Math.round(sampleCount / LARGE_STEP_DIVISOR));
}

export function moveIndex(current: number | null, sampleCount: number, direction: 1 | -1, large: boolean): number | null {
  if (sampleCount <= 0) return null;
  if (current == null) return sampleCount - 1;
  const step = large ? largeStep(sampleCount) : 1;
  return Math.min(sampleCount - 1, Math.max(0, current + direction * step));
}

export function edgeIndex(sampleCount: number, edge: "oldest" | "newest"): number | null {
  if (sampleCount <= 0) return null;
  return edge === "oldest" ? 0 : sampleCount - 1;
}

export function moveRow(current: number | null, rowCount: number, direction: 1 | -1): number | null {
  if (rowCount <= 0) return null;
  if (current == null) return rowCount - 1;
  return Math.min(rowCount - 1, Math.max(0, current + direction));
}

export function nearestIndex(times: readonly number[], sec: number): number | null {
  if (times.length === 0) return null;
  let best = 0;
  let bestDistance = Math.abs(times[0] - sec);
  for (let i = 1; i < times.length; i++) {
    const distance = Math.abs(times[i] - sec);
    if (distance < bestDistance) {
      best = i;
      bestDistance = distance;
    }
  }
  return best;
}

export function isZoomed(current: ZoomWindow, rangeSpanSec: number): boolean {
  return current.to - current.from < rangeSpanSec;
}

function wholeWindow(current: ZoomWindow): ZoomWindow | null {
  if (!Number.isFinite(current.from) || !Number.isFinite(current.to)) return null;
  return { from: Math.ceil(current.from), to: Math.floor(current.to) };
}

export function zoomIn(current: ZoomWindow, cursorSec: number): ZoomResult {
  const whole = wholeWindow(current);
  if (!whole || !Number.isFinite(cursorSec)) return { changed: false };
  const span = whole.to - whole.from;
  if (!(span > MIN_ZOOM_SPAN_SEC)) return { changed: false };
  const newSpan = Math.max(MIN_ZOOM_SPAN_SEC, Math.floor(span / ZOOM_FACTOR));
  const from = Math.max(whole.from, Math.min(Math.round(cursorSec - newSpan / 2), whole.to - newSpan));
  return { changed: true, window: { from, to: from + newSpan } };
}

export function zoomOut(current: ZoomWindow, cursorSec: number, rangeSpanSec: number, nowSec: number): ZoomResult {
  const whole = wholeWindow(current);
  if (!whole || !Number.isFinite(cursorSec) || !Number.isFinite(nowSec)) return { changed: false };
  if (!(rangeSpanSec > 0) || !isZoomed(current, rangeSpanSec)) return { changed: false };
  const span = whole.to - whole.from;
  if (!(span > 0)) return { changed: false };
  const newSpan = span * ZOOM_FACTOR;
  if (newSpan >= rangeSpanSec) return { changed: true, window: null };
  const now = Math.floor(nowSec);
  const earliest = now - Math.floor(rangeSpanSec);
  const from = Math.max(earliest, Math.min(Math.round(cursorSec - newSpan / 2), now - newSpan));
  return { changed: true, window: { from, to: from + newSpan } };
}

export function zoomReset(current: ZoomWindow, rangeSpanSec: number): ZoomResult {
  if (!isZoomed(current, rangeSpanSec)) return { changed: false };
  return { changed: true, window: null };
}

export function shouldAnnounce(phase: "down" | "up", repeat: boolean, repeatedSinceDown: boolean): boolean {
  return phase === "down" ? !repeat : repeatedSinceDown;
}

export function formatInstant(sec: number, format?: TimeFormat): string {
  const formatter = new Intl.DateTimeFormat(format?.locale, {
    dateStyle: "medium",
    timeStyle: "medium",
    timeZone: format?.timeZone,
  });
  return formatter.format(new Date(sec * 1000)).replace(/ /g, " ");
}

export function windowText(fromSec: number | undefined, toSec: number | undefined, format?: TimeFormat): string {
  if (fromSec == null || toSec == null) return "window not loaded";
  return `${formatInstant(fromSec, format)} to ${formatInstant(toSec, format)}`;
}

export interface ChartNameInput {
  subject: string;
  target: string;
  qualifier?: string;
  fromSec: number | undefined;
  toSec: number | undefined;
  summary: string;
  format?: TimeFormat;
}

export function chartName(input: ChartNameInput): string {
  const qualifier = input.qualifier ? `, ${input.qualifier}` : "";
  return `${input.subject} for ${input.target}${qualifier}, ${windowText(input.fromSec, input.toSec, input.format)}: ${input.summary}`;
}

function plural(count: number, noun: string): string {
  return `${count} ${noun}${count === 1 ? "" : "s"}`;
}

function number1(value: number | null, unit: string): string {
  return value == null ? "none" : `${value.toFixed(1)}${unit}`;
}

function summaryState<T>(summary: T | null, loading: boolean, build: (value: T) => string): string {
  if (loading) return "loading";
  if (summary == null) return "no data in range";
  return build(summary);
}

export interface LatencySummary {
  cycles: number;
  sources: number;
  median: number | null;
  p95: number | null;
  loss: number | null;
}

export function latencySummaryText(summary: LatencySummary | null, loading: boolean): string {
  return summaryState(summary, loading, (s) =>
    `${plural(s.cycles, "cycle")} from ${plural(s.sources, "source")}, average median ${number1(s.median, " ms")}, average p95 ${number1(s.p95, " ms")}, loss ${number1(s.loss, "%")}`,
  );
}

export interface HttpSummary {
  samples: number;
  uptime: number;
  p50: number | null;
  p95: number | null;
}

export function httpSummaryText(summary: HttpSummary | null, loading: boolean): string {
  return summaryState(summary, loading, (s) =>
    `${plural(s.samples, "sample")}, uptime ${number1(s.uptime, "%")}, TTFB p50 ${number1(s.p50, " ms")}, p95 ${number1(s.p95, " ms")}`,
  );
}

export interface HeatmapSummary {
  hops: number;
  columns: number;
  worstLoss: number | null;
  worstHop: number | null;
}

export function heatmapSummaryText(summary: HeatmapSummary | null, loading: boolean): string {
  return summaryState(summary, loading, (s) => {
    const worst =
      s.worstLoss != null && s.worstLoss > 0 && s.worstHop != null
        ? `worst hop loss ${number1(s.worstLoss, "%")} at hop ${s.worstHop}`
        : "no hop loss";
    return `${plural(s.hops, "hop")}, ${plural(s.columns, "column")}, ${worst}`;
  });
}

export interface LossSummary {
  lossy: number;
  cycles: number;
  worstLoss: number | null;
  worstSec: number | null;
}

export function lossSummaryText(summary: LossSummary | null, loading: boolean, format?: TimeFormat): string {
  return summaryState(summary, loading, (s) => {
    const head = `${s.lossy} of ${plural(s.cycles, "cycle")} lost packets`;
    if (s.worstLoss == null || s.worstSec == null) return head;
    return `${head}, worst ${number1(s.worstLoss, "%")} at ${formatInstant(s.worstSec, format)}`;
  });
}

export interface StatusDistribution {
  "2xx": number;
  "3xx": number;
  "4xx": number;
  "5xx": number;
  err: number;
}

export function statusSummaryText(dist: StatusDistribution | null, loading: boolean): string {
  return summaryState(dist, loading, (d) =>
    `${d["2xx"]} 2xx, ${d["3xx"]} 3xx, ${d["4xx"]} 4xx, ${d["5xx"]} 5xx, ${d.err} network errors`,
  );
}

export interface LatencyReadoutEntry {
  source: string;
  median: number | null;
  p95: number | null;
  loss: number | null;
}

export function latencyReadoutText(timeText: string, entries: readonly LatencyReadoutEntry[]): string {
  const parts = entries.map((entry) => {
    const label = entry.source !== "" ? entry.source : entries.length > 1 ? "default source" : "";
    const values = `median ${number1(entry.median, " ms")}, p95 ${number1(entry.p95, " ms")}, loss ${number1(entry.loss, "%")}`;
    return label ? `${label} ${values}` : values;
  });
  return `${timeText}: ${parts.join("; ")}`;
}

export interface HttpReadoutInput {
  timeText: string;
  status: number;
  rtt: number;
  source: string;
  error?: string;
}

const HTTP_ERROR_READOUT_MAX = 80;

export function httpReadoutText(input: HttpReadoutInput): string {
  const status = input.status === 0 ? "network error" : String(input.status);
  let text = `${input.timeText}: status ${status}`;
  if (input.status !== 0) text += `, ${input.rtt.toFixed(1)} ms`;
  if (input.source) text += `, source ${input.source}`;
  if (input.error) text += `, error ${input.error.slice(0, HTTP_ERROR_READOUT_MAX)}`;
  return text;
}

export function lossReadoutText(timeText: string, entries: ReadonlyArray<{ source: string; loss: number }>): string {
  const parts = entries.map((entry) => {
    const label = entry.source !== "" ? entry.source : entries.length > 1 ? "default source" : "";
    const value = `loss ${entry.loss.toFixed(1)}%`;
    return label ? `${label} ${value}` : value;
  });
  return `${timeText}: ${parts.join("; ")}`;
}

export interface HeatmapReadoutInput {
  hop: number;
  timeText: string;
  hostname?: string;
  ip: string;
  lossPct: number;
  worstOfBucket: boolean;
  replies: number;
  sent: number;
  targetLoss?: number | null;
  unreach?: string;
}

export function heatmapReadoutText(input: HeatmapReadoutInput): string {
  const parts: string[] = [];
  if (input.hostname) parts.push(input.hostname);
  parts.push(input.ip || "no reply");
  parts.push(`loss ${input.lossPct.toFixed(1)}%${input.worstOfBucket ? " worst cycle" : ""}`);
  parts.push(`replies ${input.replies} of ${input.sent}`);
  if (input.targetLoss != null && input.targetLoss > 0) parts.push(`target loss ${input.targetLoss.toFixed(1)}%`);
  if (input.unreach) parts.push(`trace ended: ${input.unreach} unreachable`);
  return `hop ${input.hop}, ${input.timeText}: ${parts.join(", ")}`;
}

export function heatmapNoDataText(hop: number, timeText: string): string {
  return `hop ${hop}, ${timeText}: no data`;
}

export function pickAnnouncement(timeText: string): string {
  return `Cycle at ${timeText} picked`;
}

export function zoomAnnouncement(window: ZoomWindow | null, format?: TimeFormat): string {
  return window ? `Zoom ${windowText(window.from, window.to, format)}` : "Zoom reset";
}

export interface KeyHelpEntry {
  keys: string;
  action: string;
}

export function keyHelp(kind: ChartKind): KeyHelpEntry[] {
  const entries: KeyHelpEntry[] = [
    { keys: "Left, Right", action: kind === "heatmap" ? "move one column" : "move one sample" },
    { keys: "Shift+Left, Shift+Right, Page Up, Page Down", action: "move a tenth of the samples" },
    { keys: "Home, End", action: "oldest or newest sample" },
  ];
  if (kind === "heatmap") entries.push({ keys: "Up, Down", action: "move one hop row" });
  if (kind !== "http") entries.push({ keys: "Enter, Space", action: "open the cycle" });
  if (supportsZoom(kind)) {
    entries.push({ keys: "Plus", action: "zoom in" });
    entries.push({ keys: "Minus", action: "zoom out" });
    entries.push({ keys: "0", action: "reset zoom" });
  }
  entries.push({ keys: "Escape", action: "hide the cursor" });
  return entries;
}

export function keyHelpText(kind: ChartKind): string {
  return keyHelp(kind)
    .map((entry) => `${entry.keys}: ${entry.action}`)
    .join(". ");
}
