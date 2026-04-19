// Palette rotated per source so multi-source "all" view stays readable. First
// entry is the historical single-source teal so plain 1-source charts look
// unchanged.
export const PALETTE: { stroke: string; fill: (a: number) => string }[] = [
  { stroke: "#5eead4", fill: (a) => `rgba(94,234,212,${a})` },
  { stroke: "#f0b429", fill: (a) => `rgba(240,180,41,${a})` },
  { stroke: "#e879f9", fill: (a) => `rgba(232,121,249,${a})` },
  { stroke: "#38bdf8", fill: (a) => `rgba(56,189,248,${a})` },
  { stroke: "#fb7185", fill: (a) => `rgba(251,113,133,${a})` },
];

// paletteForSorted maps sorted source names → palette entries the way both
// chart components do, so callers can render UI affordances in the same colour
// the line on the chart uses.
export function paletteForSorted(sortedSources: string[]): Map<string, { stroke: string; fill: (a: number) => string }> {
  const out = new Map<string, { stroke: string; fill: (a: number) => string }>();
  sortedSources.forEach((name, i) => {
    out.set(name, PALETTE[i % PALETTE.length]);
  });
  return out;
}
