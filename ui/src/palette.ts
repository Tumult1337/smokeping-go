// Source identity = one of PALETTE's 9 hues × one of DASHES' 3 stroke patterns.
// The hues are searched, not hand-picked, and clear the colorblind-safety gate on
// ALL pairs rather than adjacent ones: sources are assigned by name, so any two
// can share a chart and adjacency describes nothing. Worst all-pairs CVD ΔE 8.7
// (deutan, OKLab ×100, floor 6.0, target 8.0), worst all-pairs normal-vision ΔE
// 16.1 (floor 15), minimum OKLCH hue gap 28°, all 9 inside the dark-theme
// lightness band, all >=3:1 against --bg (#0f1115). `npm run check-palette`
// re-runs those gates and the build runs it — edit the strokes and it reddens.
//
// 9 is the ceiling, measured: an annealing search over OKLCH within the dark band
// clears the gates at 9 and fails from 10 up (10 lands at ΔE 8.1/15.0), because
// deuteranopia collapses the hue circle onto roughly one axis. Past 9 concurrent
// sources the dash channel carries identity instead of a 10th hue — it survives
// CVD entirely, where another hue would not.
//
// `text` is the same hue lightened where the stroke misses the 4.5:1 small-text
// gate; strokes are graphics (3:1) and stay exactly as validated above.
import type { Theme } from "./theme";

export type PaletteEntry = {
  stroke: string;
  text: string;
  fill: (a: number) => string;
  dash?: number[];
};

type PaletteHue = { stroke: string; text: string; fill: (a: number) => string };

const PALETTE: PaletteHue[] = [
  { stroke: "#0c6f4d", text: "#348b67", fill: (a) => `rgba(12,111,77,${a})` },
  { stroke: "#c50991", text: "#dc30a5", fill: (a) => `rgba(197,9,145,${a})` },
  { stroke: "#d7727c", text: "#d7727c", fill: (a) => `rgba(215,114,124,${a})` },
  { stroke: "#15a9b0", text: "#15a9b0", fill: (a) => `rgba(21,169,176,${a})` },
  { stroke: "#5c39fc", text: "#6f67fd", fill: (a) => `rgba(92,57,252,${a})` },
  { stroke: "#a64006", text: "#c65d2e", fill: (a) => `rgba(166,64,6,${a})` },
  { stroke: "#918c1c", text: "#918c1c", fill: (a) => `rgba(145,140,28,${a})` },
  { stroke: "#256fb8", text: "#377fc9", fill: (a) => `rgba(37,111,184,${a})` },
  { stroke: "#b571e6", text: "#b571e6", fill: (a) => `rgba(181,113,230,${a})` },
];

// Light-theme data hues. Best-effort, hand-tuned darker versions of the dark
// hues in the SAME hue family so a source keeps a recognisable colour across
// themes, chosen to read on the white surfaces (>=3:1 as a stroke, text used
// for legend labels). These are NOT run through check-palette's CVD/contrast
// gate — that gate is anchored to the dark --bg only, by decision. Index i here
// is the same identity slot as PALETTE[i].
const PALETTE_LIGHT: PaletteHue[] = [
  { stroke: "#0b6446", text: "#0b6446", fill: (a) => `rgba(11,100,70,${a})` },
  { stroke: "#b3067f", text: "#b3067f", fill: (a) => `rgba(179,6,127,${a})` },
  { stroke: "#b3454f", text: "#b3454f", fill: (a) => `rgba(179,69,79,${a})` },
  { stroke: "#0d7a80", text: "#0d7a80", fill: (a) => `rgba(13,122,128,${a})` },
  { stroke: "#4b2fd6", text: "#4b2fd6", fill: (a) => `rgba(75,47,214,${a})` },
  { stroke: "#a03d06", text: "#a03d06", fill: (a) => `rgba(160,61,6,${a})` },
  { stroke: "#6d6810", text: "#6d6810", fill: (a) => `rgba(109,104,16,${a})` },
  { stroke: "#1f5f9e", text: "#1f5f9e", fill: (a) => `rgba(31,95,158,${a})` },
  { stroke: "#8a3fce", text: "#8a3fce", fill: (a) => `rgba(138,63,206,${a})` },
];

function hueTable(theme: Theme): PaletteHue[] {
  return theme === "light" ? PALETTE_LIGHT : PALETTE;
}

// Solid first so an ordinary install never draws a dashed line; the other two are
// separated in both dash and gap so they stay distinct at one-pixel stroke width.
const DASHES: (number[] | undefined)[] = [undefined, [7, 4], [2, 3]];

const HUES = PALETTE.length;
const IDENTITIES = HUES * DASHES.length;

// FNV-1a over UTF-16 code units. Not a security primitive — it only has to be
// stable across reloads and spread short slave names across the slot space.
function hashName(name: string): number {
  let h = 0x811c9dc5;
  for (let i = 0; i < name.length; i++) {
    h ^= name.charCodeAt(i);
    h = Math.imul(h, 0x01000193);
  }
  return h >>> 0;
}

function entryAt(identity: number, table: PaletteHue[]): PaletteEntry {
  // DASHES[0] is undefined, so identities below HUES are exactly the solid ones.
  return { ...table[identity % HUES], dash: DASHES[Math.floor(identity / HUES)] };
}

// Every solid hue ranks above every dashed slot, so the first HUES sources are
// always solid and distinct without the assignment needing to know how many
// sources there are. Deciding that from the count instead made crossing HUES a
// regime change that reshuffled all nine incumbents at once.
function preferences(name: string): number[] {
  const h = hashName(name);
  const dashed = IDENTITIES - HUES;
  const out: number[] = [];
  for (let i = 0; i < HUES; i++) out.push((h + i) % HUES);
  for (let i = 0; i < dashed; i++) out.push(HUES + ((h + i) % dashed));
  return out;
}

// paletteForSorted maps source names → palette entries, derived from the name
// rather than from its position, so a slave keeps its colour when another one is
// added or removed. Position-derived assignment reshuffled every source on any
// membership change; this moves at most one incumbent, and only the one whose
// slot the newcomer takes.
//
// Stability is therefore "unless another source wants the same slot", not
// absolute — the alternative, honouring the hash unconditionally, collides at
// 98% for 7 sources over 9 slots and is the defect this replaces. Past
// IDENTITIES sources the slots are all spent and entries repeat.
export function paletteForSorted(
  sortedSources: string[],
  theme: Theme = "dark",
): Map<string, PaletteEntry> {
  const table = hueTable(theme);
  // Uncontested names claim their first choice before anyone probes, so adding a
  // source cannot cascade past the one incumbent it displaces.
  const byTop = new Map<number, string[]>();
  for (const name of sortedSources) {
    const top = preferences(name)[0];
    const at = byTop.get(top);
    if (at) at.push(name);
    else byTop.set(top, [name]);
  }

  const claimed = new Map<string, number>();
  const taken = new Set<number>();
  const contested: string[] = [];
  for (const [top, names] of byTop) {
    // Sorted input makes the winner deterministic without a tiebreak of its own.
    claimed.set(names[0], top);
    taken.add(top);
    contested.push(...names.slice(1));
  }

  for (const name of contested) {
    const prefs = preferences(name);
    // Past IDENTITIES sources every slot is spoken for; keep the top choice and
    // repeat rather than looping forever looking for a free one.
    let identity = prefs[0];
    for (let i = 1; i < prefs.length; i++) {
      if (!taken.has(prefs[i])) {
        identity = prefs[i];
        break;
      }
    }
    taken.add(identity);
    claimed.set(name, identity);
  }

  // Built in the caller's order; charts index this map while walking their own
  // sorted source list, and claim order is otherwise arbitrary.
  const out = new Map<string, PaletteEntry>();
  for (const name of sortedSources) out.set(name, entryAt(claimed.get(name)!, table));
  return out;
}

// lossColor maps a per-cycle loss percentage to a status colour. The three
// thresholds (5 / 20%) match the bar chart's median-tick coloring so band
// mode and bars mode tell the same story for the same data. okColor lets the
// caller fall back to a source-specific stroke at zero loss; that way a
// per-source colored line/tick stays uniform when there's nothing to flag.
export function lossColor(pct: number, okColor: string, theme: Theme = "dark"): string {
  if (pct <= 0) return okColor;
  if (theme === "light") {
    if (pct < 5) return "#b45309";
    if (pct < 20) return "#c2410c";
    return "#dc2626";
  }
  if (pct < 5) return "#eab308";
  if (pct < 20) return "#f97316";
  return "#ef4444";
}

// lossColor's ramp with the >=20% red lightened to clear 4.5:1 as small text.
// Canvas marks keep lossColor — they are graphics, gated at 3:1. The light ramp
// is already dark enough on white, so it matches lossColor there.
export function lossTextColor(pct: number, okColor: string, theme: Theme = "dark"): string {
  if (pct <= 0) return okColor;
  if (theme === "light") {
    if (pct < 5) return "#b45309";
    if (pct < 20) return "#c2410c";
    return "#dc2626";
  }
  if (pct < 5) return "#eab308";
  if (pct < 20) return "#f97316";
  return "#f15c5c";
}
