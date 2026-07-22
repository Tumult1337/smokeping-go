// Palette rotated per source so multi-source "all" view stays readable.
// Fixed order, 8 slots, chosen (not generated) so every adjacent pair clears
// the colorblind-safety gate: worst adjacent CVD ΔE 8.4 (protan/deutan,
// OKLab ×100, floor 6.0), worst adjacent normal-vision ΔE 19.3 (floor 15),
// all 8 inside the dark-theme OKLCH lightness band, all >=3:1 against
// --bg (#0f1115). Verified with dataviz's validate_palette.js — re-run it
// against any edit here:
//   node validate_palette.js "<8 hex>" --mode dark --surface "#0f1115"
// The previous 5-entry palette silently repeated past 5 sources (2 gosmokeping
// slaves ended up sharing a color) and had already failed this same check
// (sky/fuchsia were ΔE 0.3 under deutan — indistinguishable). This set fixes
// both: no repeat through 8 concurrent sources.
// SIMPLIFIED: caps at 8 distinct hues — that's close to the practical ceiling
// for hue-only categorical identity at a fixed lightness/chroma band (the
// dataviz skill's own reference tops out here too). A 9th+ source still wraps
// via `i % PALETTE.length` and repeats a hue. If gosmokeping regularly runs
// >8 slaves against one target, the upgrade path is a secondary channel (e.g.
// a dashed stroke on the repeated half) rather than adding more hues — more
// hues at this point stop being reliably distinguishable, colorblind or not.
export const PALETTE: { stroke: string; fill: (a: number) => string }[] = [
  { stroke: "#3987e5", fill: (a) => `rgba(57,135,229,${a})` },
  { stroke: "#d95926", fill: (a) => `rgba(217,89,38,${a})` },
  { stroke: "#199e70", fill: (a) => `rgba(25,158,112,${a})` },
  { stroke: "#c98500", fill: (a) => `rgba(201,133,0,${a})` },
  { stroke: "#d55181", fill: (a) => `rgba(213,81,129,${a})` },
  { stroke: "#008300", fill: (a) => `rgba(0,131,0,${a})` },
  { stroke: "#9085e9", fill: (a) => `rgba(144,133,233,${a})` },
  { stroke: "#e66767", fill: (a) => `rgba(230,103,103,${a})` },
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

// lossColor maps a per-cycle loss percentage to a status colour. The three
// thresholds (5 / 20%) match the bar chart's median-tick coloring so band
// mode and bars mode tell the same story for the same data. okColor lets the
// caller fall back to a source-specific stroke at zero loss; that way a
// per-source colored line/tick stays uniform when there's nothing to flag.
export function lossColor(pct: number, okColor: string): string {
  if (pct <= 0) return okColor;
  if (pct < 5) return "#eab308";
  if (pct < 20) return "#f97316";
  return "#ef4444";
}
