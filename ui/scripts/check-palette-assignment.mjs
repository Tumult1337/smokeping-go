// Gate for paletteForSorted's slot assignment. Run by `npm run check-palette`,
// which `build` depends on.
//
// Imports the real src/palette.ts (node strips the types) rather than restating
// its arithmetic — a duplicated model stops matching silently and the gate keeps
// passing against code it no longer describes.
//
// The churn bounds below are swept, not chosen: the sweep is deterministic (fixed
// name sequences, no randomness), so a bound that moves is a behaviour change.
import { paletteForSorted } from "../src/palette.ts";

const HUES = 9;
const IDENTITIES = 27;
const CHURN_SOLID = 1;  // k=1..8,  measured max, at k=1
const CHURN_MID = 4;    // k=1..20, measured max, at k=17
const CHURN_FULL = 10;  // k=1..27, measured max, at k=27 as the slots run out

const id = (e) => e.stroke + "|" + (e.dash ? e.dash.join(",") : "solid");
const names = (k, p = "node") => Array.from({ length: k }, (_, i) => p + i);
const fail = [];

// Distinctness. Below HUES every source must get its own hue AND stay solid: the
// bar and strip views draw no stroke, so dash carries nothing for them and two
// sources sharing a hue there are indistinguishable however they differ on a line.
for (let k = 1; k <= HUES; k++) {
  const m = paletteForSorted(names(k));
  const hues = new Set([...m.values()].map((e) => e.stroke));
  const dashed = [...m.values()].filter((e) => e.dash !== undefined);
  if (hues.size !== k) fail.push(`k=${k}: ${hues.size} distinct hues, expected ${k}`);
  if (dashed.length) fail.push(`k=${k}: ${dashed.length} dashed entries below the hue ceiling, expected all solid`);
}

// Between HUES and IDENTITIES the dash channel carries identity, and exactly HUES
// entries stay solid.
for (let k = HUES + 1; k <= IDENTITIES; k++) {
  const m = paletteForSorted(names(k));
  const distinct = new Set([...m.values()].map(id));
  const solid = [...m.values()].filter((e) => e.dash === undefined);
  if (distinct.size !== k) fail.push(`k=${k}: ${distinct.size} distinct identities, expected ${k}`);
  if (solid.length !== HUES) fail.push(`k=${k}: ${solid.length} solid entries, expected ${HUES}`);
}

// Past IDENTITIES the slots are spent: every source must still get an entry and
// the loop must terminate. Repetition is the documented cost, a hang is not.
for (const k of [IDENTITIES + 1, 50, 200]) {
  const m = paletteForSorted(names(k));
  if (m.size !== k) fail.push(`k=${k}: ${m.size} entries, expected ${k}`);
  if ([...m.values()].some((e) => !e || !e.stroke)) fail.push(`k=${k}: an entry has no stroke`);
  const distinct = new Set([...m.values()].map(id));
  if (distinct.size !== IDENTITIES) fail.push(`k=${k}: ${distinct.size} distinct identities, expected saturation at ${IDENTITIES}`);
}

// Determinism and caller order. Charts index this map while walking their own
// sorted source list, so a different iteration order silently mismatches them.
const seq = names(12);
const a = paletteForSorted(seq), b = paletteForSorted(seq);
if ([...a.keys()].join() !== seq.join()) fail.push("iteration order does not match the caller's order");
if (seq.some((n) => id(a.get(n)) !== id(b.get(n)))) fail.push("two calls with identical input disagree");

// Stability: adding a source must not reshuffle the rest. This is the property the
// change exists for — position-derived assignment moved every source on any
// membership change.
function churnMax(lo, hi) {
  let worst = 0, at = "";
  for (let k = lo; k <= hi; k++) {
    const base = names(k);
    for (let i = 0; i < 150; i++) {
      const before = paletteForSorted([...base].sort());
      const after = paletteForSorted([...base, "added" + i].sort());
      const moved = base.filter((n) => id(before.get(n)) !== id(after.get(n))).length;
      if (moved > worst) { worst = moved; at = `k=${k}/added${i}`; }
    }
  }
  return [worst, at];
}
const [c1, at1] = churnMax(1, 8);
const [c2, at2] = churnMax(1, 20);
const [c3, at3] = churnMax(1, IDENTITIES);
if (c1 > CHURN_SOLID) fail.push(`churn k=1..8 is ${c1} (${at1}), above ${CHURN_SOLID}`);
if (c2 > CHURN_MID) fail.push(`churn k=1..20 is ${c2} (${at2}), above ${CHURN_MID}`);
if (c3 > CHURN_FULL) fail.push(`churn k=1..${IDENTITIES} is ${c3} (${at3}), above ${CHURN_FULL}`);

// A bound only ever checked from above passes when assignment stops moving
// anything at all — including when it stops depending on the name.
if (c3 === 0) fail.push("no source ever changes slot; assignment is not contending at all");

console.log("assignment:");
console.log(`  distinct hues below ${HUES} sources, ${IDENTITIES} identities to ${IDENTITIES} sources, saturates past it`);
console.log(`  churn adding one source: k<=8 ${c1}  k<=20 ${c2}  k<=${IDENTITIES} ${c3}`);

if (fail.length) {
  console.error("\nassignment check FAILED:");
  for (const f of fail) console.error("  - " + f);
  process.exit(1);
}
