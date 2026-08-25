export interface Target {
  id: string;
  group: string;
  group_title?: string;
  name: string;
  title?: string;
  probe: string;
  probe_type?: string;
  host?: string;
  url?: string;
  alerts?: string[];
  sources?: string[];
}

export interface HopPoint {
  Time: string;
  Source: string;
  Index: number;
  IP: string;
  // Closed-set label of the ICMP unreachable that ended the trace at this
  // hop; empty or absent on ordinary hops and on rows predating the column.
  Unreach?: string;
  // True on the row(s) whose responder echoed as the target itself; survives
  // health redaction on /hops, and is never sent on /hops/timeline rows.
  TargetReply?: boolean;
  Min: number;
  Max: number;
  Mean: number;
  Median: number;
  LossPct: number;
  LossCount: number;
  Sent: number;
  // Timeline (bucketed) rows only: worst single-cycle loss in the bucket and
  // the exact timestamp of that cycle. Absent on the /hops?at= path-table rows.
  MaxLossPct?: number;
  WorstTime?: string;
}

export interface HopsResponse {
  target: string;
  hops: HopPoint[];
}

export interface CyclePoint {
  Time: string;
  Source: string;
  Min: number;
  Max: number;
  Mean: number;
  Median: number;
  StdDev: number;
  P5: number;
  P10: number;
  P15: number;
  P20: number;
  P25: number;
  P30: number;
  P35: number;
  P40: number;
  P45: number;
  P55: number;
  P60: number;
  P65: number;
  P70: number;
  P75: number;
  P80: number;
  P85: number;
  P90: number;
  P95: number;
  LossPct: number;
  LossCount: number;
  Sent: number;
}

export interface CyclesResponse {
  from: string;
  to: string;
  points: CyclePoint[];
}

async function jsonGet<T>(url: string, signal?: AbortSignal): Promise<T> {
  const r = await fetch(url, { signal });
  if (!r.ok) {
    const body = await r.text();
    throw new Error(`${r.status}: ${body}`);
  }
  return (await r.json()) as T;
}

export function listTargets(): Promise<Target[]> {
  return jsonGet<Target[]>("/api/v1/targets");
}

export interface SourcesResponse {
  sources: string[];
}

export function listSources(): Promise<SourcesResponse> {
  return jsonGet<SourcesResponse>("/api/v1/sources");
}

export function getCycles(
  id: string,
  from: string,
  to?: string,
  source?: string,
): Promise<CyclesResponse> {
  const params = new URLSearchParams({ from });
  if (to) params.set("to", to);
  if (source) params.set("source", source);
  return jsonGet<CyclesResponse>(`/api/v1/targets/${id}/cycles?${params}`);
}

export interface HttpPoint {
  Time: string;
  Source: string;
  RTT: number;
  Status: number;
  Seq: number;
  Err: string;
}

export interface HttpResponse {
  target: string;
  from: string;
  to: string;
  points: HttpPoint[];
}

export function getHttpSamples(
  id: string,
  from: string,
  to?: string,
  source?: string,
): Promise<HttpResponse> {
  const params = new URLSearchParams({ from });
  if (to) params.set("to", to);
  if (source) params.set("source", source);
  return jsonGet<HttpResponse>(`/api/v1/targets/${id}/http?${params}`);
}

export function getHops(
  id: string,
  atSec?: number,
  source?: string,
  signal?: AbortSignal,
): Promise<HopsResponse> {
  const params = new URLSearchParams();
  if (atSec != null) params.set("at", String(Math.floor(atSec)));
  if (source) params.set("source", source);
  const qs = params.toString();
  return jsonGet<HopsResponse>(
    `/api/v1/targets/${id}/hops${qs ? `?${qs}` : ""}`,
    signal,
  );
}

export interface HopsTimelineResponse {
  target: string;
  from: string;
  to: string;
  // Bucket width the server's ladder picked, 0 on the raw tier. The heatmap
  // needs it to size a column: row spacing cannot be measured when a window
  // holds a single bucket.
  step_sec: number;
  hops: HopPoint[];
}

export interface OverviewRow {
  id: string;
  group: string;
  group_title?: string;
  title?: string;
  probe_type?: string;
  loss_avg: number | null;
  loss_max: number | null;
  rtt_median: number | null;
  rtt_p95: number | null;
  rtt_max: number | null;
  worst_source?: string;
  last_seen: string | null;
  silent: boolean;
  sparkline: Array<number | null>;
}

export interface OverviewResponse {
  window: string;
  source?: string;
  from: string;
  to: string;
  rows: OverviewRow[];
}

export type OverviewWindow = "-1h" | "-6h" | "-24h";

export function getOverview(
  window: OverviewWindow,
  source?: string,
): Promise<OverviewResponse> {
  const params = new URLSearchParams({ window });
  if (source) params.set("source", source);
  return jsonGet<OverviewResponse>(`/api/v1/overview?${params}`);
}

export function getHopsTimeline(
  id: string,
  from: string,
  to?: string,
  source?: string,
  signal?: AbortSignal,
): Promise<HopsTimelineResponse> {
  const params = new URLSearchParams({ from });
  if (to) params.set("to", to);
  if (source) params.set("source", source);
  return jsonGet<HopsTimelineResponse>(
    `/api/v1/targets/${id}/hops/timeline?${params}`,
    signal,
  );
}
