export function hopHostname(
  hostnames: Readonly<Record<string, string>> | undefined,
  ip: string,
): string | undefined {
  return hostnames?.[ip] || undefined;
}
