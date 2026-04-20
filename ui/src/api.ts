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
  Index: number;
  IP: string;
  Min: number;
  Max: number;
  Mean: number;
  Median: number;
  LossPct: number;
  LossCount: number;
  Sent: number;
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
  resolution: string;
  from: string;
  to: string;
  points: CyclePoint[];
}

export type Resolution = "raw" | "1h" | "1d" | "auto";

async function jsonGet<T>(url: string): Promise<T> {
  const r = await fetch(url);
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
  resolution?: Resolution,
  source?: string,
): Promise<CyclesResponse> {
  const params = new URLSearchParams({ from });
  if (to) params.set("to", to);
  if (resolution && resolution !== "auto") params.set("resolution", resolution);
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
): Promise<HopsResponse> {
  const params = new URLSearchParams();
  if (atSec != null) params.set("at", String(Math.floor(atSec)));
  if (source) params.set("source", source);
  const qs = params.toString();
  return jsonGet<HopsResponse>(
    `/api/v1/targets/${id}/hops${qs ? `?${qs}` : ""}`,
  );
}

export interface HopsTimelineResponse {
  target: string;
  from: string;
  to: string;
  hops: HopPoint[];
}

export function getHopsTimeline(
  id: string,
  from: string,
  to?: string,
  source?: string,
): Promise<HopsTimelineResponse> {
  const params = new URLSearchParams({ from });
  if (to) params.set("to", to);
  if (source) params.set("source", source);
  return jsonGet<HopsTimelineResponse>(
    `/api/v1/targets/${id}/hops/timeline?${params}`,
  );
}
