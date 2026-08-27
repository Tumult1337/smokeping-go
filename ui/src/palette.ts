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
export type PaletteEntry = {
  stroke: string;
  text: string;
  fill: (a: number) => string;
  dash?: number[];
};

const PALETTE: { stroke: string; text: string; fill: (a: number) => string }[] = [
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

function entryAt(identity: number): PaletteEntry {
  // DASHES[0] is undefined, so identities below HUES are exactly the solid ones.
  return { ...PALETTE[identity % HUES], dash: DASHES[Math.floor(identity / HUES)] };
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
export function paletteForSorted(sortedSources: string[]): Map<string, PaletteEntry> {
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
  for (const name of sortedSources) out.set(name, entryAt(claimed.get(name)!));
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

// lossColor's ramp with the >=20% red lightened to clear 4.5:1 as small text.
// Canvas marks keep lossColor — they are graphics, gated at 3:1.
export function lossTextColor(pct: number, okColor: string): string {
  if (pct <= 0) return okColor;
  if (pct < 5) return "#eab308";
  if (pct < 20) return "#f97316";
  return "#f15c5c";
}
