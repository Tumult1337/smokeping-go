// Gate for src/palette.ts's hues. Run by `npm run check-palette`, which `build`
// depends on, so a stroke edited without re-validating fails the build instead of
// shipping two sources nobody can tell apart.
//
// ALL pairs, not adjacent ones: sources are assigned by name, so any two hues can
// share a chart and adjacency describes nothing about what a reader sees. The
// shipped set was found by search under exactly these thresholds.
//
// Math is Machado, Oliveira & Fernandes (2009) severity-1.0 CVD simulation with
// distances as Euclidean OKLab ×100 — the same model dataviz's validate_palette.js
// uses, and the thresholds are calibrated to it. Swapping in another simulation
// (e.g. Viénot-1999) moves borderline pairs and would need them recalibrated.
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const SURFACE = "#0f1115"; // --bg
const CVD_FLOOR = 6.0, CVD_TARGET = 8.0, NORMAL_FLOOR = 15.0;
const BAND = [0.48, 0.67], CHROMA_FLOOR = 0.1, CONTRAST_MIN = 3.0, TEXT_MIN = 4.5;
const HUE_GAP_MIN = 20; // OKLCH degrees; keeps two entries from reading as one colour name

const MACHADO = {
  protan: [[0.152286, 1.052583, -0.204868], [0.114503, 0.786281, 0.099216], [-0.003882, -0.048116, 1.051998]],
  deutan: [[0.367322, 0.860646, -0.227968], [0.280085, 0.672501, 0.047413], [-0.011820, 0.042940, 0.968881]],
};

const s2lin = (c) => (c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4);
const lin = (hex) => [0, 2, 4].map((i) => parseInt(hex.slice(1 + i, 3 + i), 16) / 255).map(s2lin);
const oklabFromLin = ([r, g, b]) => {
  const l = Math.cbrt(0.4122214708 * r + 0.5363325363 * g + 0.0514459929 * b);
  const m = Math.cbrt(0.2119034982 * r + 0.6806995451 * g + 0.1073969566 * b);
  const s = Math.cbrt(0.0883024619 * r + 0.2817188376 * g + 0.6299787005 * b);
  return [
    0.2104542553 * l + 0.7936177850 * m - 0.0040720468 * s,
    1.9779984951 * l - 2.4285922050 * m + 0.4505937099 * s,
    0.0259040371 * l + 0.7827717662 * m - 0.8086757660 * s,
  ];
};
const simulate = (hex, kind) => {
  const [r, g, b] = lin(hex), M = MACHADO[kind], cl = (c) => Math.max(0, Math.min(1, c));
  return [cl(M[0][0] * r + M[0][1] * g + M[0][2] * b), cl(M[1][0] * r + M[1][1] * g + M[1][2] * b), cl(M[2][0] * r + M[2][1] * g + M[2][2] * b)];
};
const dE = (a, b, kind) => {
  const p = oklabFromLin(kind ? simulate(a, kind) : lin(a));
  const q = oklabFromLin(kind ? simulate(b, kind) : lin(b));
  return 100 * Math.hypot(p[0] - q[0], p[1] - q[1], p[2] - q[2]);
};
const relLum = (hex) => { const [r, g, b] = lin(hex); return 0.2126 * r + 0.7152 * g + 0.0722 * b; };
const contrast = (a, b) => { const [hi, lo] = [relLum(a), relLum(b)].sort((x, y) => y - x); return (hi + 0.05) / (lo + 0.05); };
const oklch = (hex) => { const [L, a, b] = oklabFromLin(lin(hex)); return [L, Math.hypot(a, b), ((Math.atan2(b, a) * 180) / Math.PI + 360) % 360]; };

const src = readFileSync(join(dirname(fileURLToPath(import.meta.url)), "..", "src", "palette.ts"), "utf8");

// Every element of the literal must parse, rather than every element that happens
// to match: a scan that skips what it cannot read validates the remainder and
// passes, so one renamed key silently drops an entry out of the gate.
// Lazy up to the assignment: the type annotation contains `=>`, so a negated
// character class on `=` stops inside it and never reaches the literal.
const literal = src.match(/const PALETTE\b[\s\S]*?=\s*\[\r?\n([\s\S]*?)\n\];/);
if (!literal) {
  console.error("palette check FAILED:\n  - no `const PALETTE ... = [ ... ];` literal in src/palette.ts");
  process.exit(1);
}
const chunks = literal[1].split("},").map((c) => c.trim()).filter(Boolean);
const fail = [];
const entries = [];
for (const [i, chunk] of chunks.entries()) {
  const m = chunk.match(/^\{\s*stroke:\s*"(#[0-9a-f]{6})",\s*text:\s*"(#[0-9a-f]{6})",\s*fill:/);
  if (!m) fail.push(`PALETTE entry ${i} does not parse as { stroke, text, fill }: ${chunk.split("\n")[0]}`);
  else entries.push({ stroke: m[1], text: m[2] });
}
if (entries.length < 2) fail.push(`parsed ${entries.length} palette entries — the table or this parser moved`);

for (const { stroke, text } of entries) {
  const [L, C] = oklch(stroke);
  if (L < BAND[0] || L > BAND[1]) fail.push(`${stroke} L ${L.toFixed(3)} outside dark band ${BAND[0]}–${BAND[1]}`);
  if (C < CHROMA_FLOOR) fail.push(`${stroke} C ${C.toFixed(3)} below chroma floor ${CHROMA_FLOOR} (reads gray)`);
  if (contrast(stroke, SURFACE) < CONTRAST_MIN) fail.push(`${stroke} contrast ${contrast(stroke, SURFACE).toFixed(2)}:1 vs ${SURFACE}, below ${CONTRAST_MIN}:1`);
  if (contrast(text, SURFACE) < TEXT_MIN) fail.push(`text ${text} contrast ${contrast(text, SURFACE).toFixed(2)}:1 vs ${SURFACE}, below ${TEXT_MIN}:1 for small text`);
}

function score(strokes) {
  let cvd = [Infinity], normal = [Infinity], hue = [360];
  for (let i = 0; i < strokes.length; i++) {
    for (let j = i + 1; j < strokes.length; j++) {
      const [a, b] = [strokes[i], strokes[j]];
      const c = Math.min(dE(a, b, "protan"), dE(a, b, "deutan"));
      if (c < cvd[0]) cvd = [c, a, b];
      const n = dE(a, b);
      if (n < normal[0]) normal = [n, a, b];
      const d = Math.abs(oklch(a)[2] - oklch(b)[2]);
      const g = Math.min(d, 360 - d);
      if (g < hue[0]) hue = [g, a, b];
    }
  }
  return { cvd, normal, hue };
}

// The gates above only prove the shipped strokes satisfy whatever this file
// measures; they say nothing about the measuring. Neuter dE or oklch and every
// check still passes, because a distance that is always large clears every floor.
// So the scoring runs once against the palette this change replaced — measured at
// CVD ΔE 1.6 / normal 7.1 by dataviz's validate_palette.js — and must call it bad.
const KNOWN_BAD = ["#3987e5", "#d95926", "#199e70", "#c98500", "#d55181", "#008300", "#9085e9", "#e66767"];
const bad = score(KNOWN_BAD);
if (bad.cvd[0] >= CVD_FLOOR || bad.normal[0] >= NORMAL_FLOOR || bad.hue[0] >= HUE_GAP_MIN) {
  console.error("palette check FAILED:\n  - scoring is broken: the pre-2026-08 palette measured "
    + `CVD ${bad.cvd[0].toFixed(1)} / normal ${bad.normal[0].toFixed(1)} / hue ${bad.hue[0].toFixed(1)}deg, `
    + `but it is known-bad (1.6 / 7.1 / 17.8) and must fall below the floors `
    + `${CVD_FLOOR} / ${NORMAL_FLOOR} / ${HUE_GAP_MIN}`);
  process.exit(1);
}

// Same reasoning for the per-entry measurements, both directions: a neutered
// contrast() or oklch() would clear every check silently, and a floor edited to
// zero would too. Literals, so they pin the functions rather than the palette.
const selfFail = [];
if (!(contrast("#111111", SURFACE) < CONTRAST_MIN)) selfFail.push("contrast() calls near-black legible against the background");
if (!(contrast("#ffffff", SURFACE) >= TEXT_MIN)) selfFail.push("contrast() calls white illegible against the background");
// #757575 is 4.10:1 here — above the graphics gate, below the small-text one. It
// pins TEXT_MIN's value rather than its direction: asserting only that white
// clears it stays true for every floor, including zero.
if (!(TEXT_MIN > CONTRAST_MIN)) selfFail.push(`TEXT_MIN ${TEXT_MIN} is not stricter than CONTRAST_MIN ${CONTRAST_MIN}`);
if (!(contrast("#757575", SURFACE) < TEXT_MIN)) selfFail.push("TEXT_MIN admits 4.10:1, which is below the 4.5:1 small-text gate");
if (!(contrast("#757575", SURFACE) >= CONTRAST_MIN)) selfFail.push("CONTRAST_MIN rejects 4.10:1, which clears the 3:1 graphics gate");
if (!(oklch("#000000")[0] < BAND[0])) selfFail.push("oklch() puts black inside the dark lightness band");
if (!(oklch("#808080")[1] < CHROMA_FLOOR)) selfFail.push("oklch() gives mid-gray chroma above the floor");
if (selfFail.length) {
  console.error("palette check FAILED:");
  for (const f of selfFail) console.error("  - scoring is broken: " + f);
  process.exit(1);
}

const { cvd: worstCvd, normal: worstNormal, hue: worstHue } = score(entries.map((e) => e.stroke));
if (worstCvd[0] < CVD_FLOOR) fail.push(`all-pairs CVD ΔE ${worstCvd[0].toFixed(1)} (${worstCvd[1]}↔${worstCvd[2]}) below floor ${CVD_FLOOR}`);
if (worstNormal[0] < NORMAL_FLOOR) fail.push(`all-pairs normal-vision ΔE ${worstNormal[0].toFixed(1)} (${worstNormal[1]}↔${worstNormal[2]}) below floor ${NORMAL_FLOOR}`);
if (worstHue[0] < HUE_GAP_MIN) fail.push(`all-pairs OKLCH hue gap ${worstHue[0].toFixed(0)}° (${worstHue[1]}↔${worstHue[2]}) below ${HUE_GAP_MIN}° — they read as one colour`);

// make-icon.mjs copies three of these strokes and says so in a comment; without
// this the comment silently becomes false the first time a stroke here changes.
const icon = readFileSync(join(dirname(fileURLToPath(import.meta.url)), "make-icon.mjs"), "utf8");
const ramp = [...icon.matchAll(/const (?:LOW|MID|HIGH) = '(#[0-9a-f]{6})';/g)].map((m) => m[1]);
if (ramp.length !== 3) fail.push(`found ${ramp.length} of the 3 LOW/MID/HIGH constants in make-icon.mjs`);
for (const c of ramp) {
  if (!entries.some((e) => e.stroke === c)) fail.push(`make-icon.mjs uses ${c}, which is not a PALETTE stroke`);
}

console.log(`palette: ${entries.length} hues`);
console.log(`  all-pairs CVD ΔE        ${worstCvd[0].toFixed(1)}  (${worstCvd[1]}↔${worstCvd[2]}, floor ${CVD_FLOOR} / target ${CVD_TARGET})`);
console.log(`  all-pairs normal ΔE     ${worstNormal[0].toFixed(1)}  (${worstNormal[1]}↔${worstNormal[2]}, floor ${NORMAL_FLOOR})`);
console.log(`  all-pairs hue gap       ${worstHue[0].toFixed(0)}°  (${worstHue[1]}↔${worstHue[2]}, floor ${HUE_GAP_MIN}°)`);

if (fail.length) {
  console.error("\npalette check FAILED:");
  for (const f of fail) console.error("  - " + f);
  process.exit(1);
}
console.log(`  ${worstCvd[0] >= CVD_TARGET ? "all gates pass, CVD above target" : "all gates pass, CVD in the floor band (needs the dash channel)"}`);
