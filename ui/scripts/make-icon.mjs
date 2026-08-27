// Draws the gosmokeping mark: probes radiating from a hub, each bar's length and
// colour standing for one probe's latency.
//
//   ui/public/favicon.svg  16 bars, for a browser tab
//   ui/public/icon.svg     64 bars, for anywhere it is shown large
//
// The bar pattern is synthetic and fixed, not read from a live instance. The
// icon ships to every user of this tool, so it must not encode one operator's
// probe list; the sequence below is deterministic so the file regenerates byte
// for byte.
//
// Run with: npm run icon
import { mkdirSync, writeFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

// Three entries of ui/src/palette.ts, which is gated for colourblind separation
// against the dark surface. Read as a latency ramp: quiet, warm, bad.
const LOW = '#256fb8';
const MID = '#918c1c';
const HIGH = '#d7727c';
const PLATE = '#0f1115'; // --bg
const HUB = '#1b1f27';
const CORE = '#e8ebf2';

const parse = (h) => [1, 3, 5].map((i) => parseInt(h.slice(i, i + 2), 16));
const mix = (a, b, t) => {
	const [r1, g1, b1] = parse(a);
	const [r2, g2, b2] = parse(b);
	return `#${[r1 + (r2 - r1) * t, g1 + (g2 - g1) * t, b1 + (b2 - b1) * t]
		.map((v) => Math.round(v).toString(16).padStart(2, '0'))
		.join('')}`;
};
const heat = (t) => (t < 0.5 ? mix(LOW, MID, t / 0.5) : mix(MID, HIGH, (t - 0.5) / 0.5));

// A latency distribution has a floor and a thin tail, so most bars sit low and a
// few spike. Stacked irrationals give that shape without a seed or a PRNG, and
// stay identical across runs and platforms.
const level = (i) => {
	const a = Math.sin(i * 1.7) * 0.5 + Math.sin(i * 0.61 + 2.1) * 0.3 + Math.sin(i * 2.9) * 0.2;
	return Math.min(1, Math.max(0, (a + 1) / 2) ** 2.1);
};

function icon({ size, bars, inner, minLen, maxLen, width, hubR, plate }) {
	const c = size / 2;
	const n = (v) => v.toFixed(2);
	const gap = ((bars > 24 ? 0.35 : 1.6) * Math.PI) / 180;
	const per = (2 * Math.PI - gap * bars) / bars;
	const spokes = [];
	for (let i = 0; i < bars; i++) {
		const t = level(i);
		const mid = -Math.PI / 2 + i * (per + gap) + per / 2;
		const r1 = inner + minLen + t * (maxLen - minLen);
		spokes.push(
			`<path d="M ${n(c + inner * Math.cos(mid))} ${n(c + inner * Math.sin(mid))} L ${n(c + r1 * Math.cos(mid))} ${n(c + r1 * Math.sin(mid))}" stroke="${heat(t)}" />`
		);
	}
	return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${size} ${size}" width="${size}" height="${size}">
${plate ? `\t<rect width="${size}" height="${size}" rx="${n(size * 0.19)}" fill="${PLATE}" />\n` : ''}\t<g fill="none" stroke-linecap="round" stroke-width="${width}">
\t\t${spokes.join('\n\t\t')}
\t</g>
\t<circle cx="${c}" cy="${c}" r="${hubR}" fill="${HUB}" />
\t<circle cx="${c}" cy="${c}" r="${n(hubR * 0.34)}" fill="${CORE}" />
</svg>
`;
}

// The tab icon drops to 16 bars: at 16px, 64 of them merge into a ring.
const favicon = icon({ size: 48, bars: 16, inner: 11.5, minLen: 2.6, maxLen: 9.4, width: 2.6, hubR: 4.4, plate: true });
const large = icon({ size: 512, bars: 64, inner: 116, minLen: 26, maxLen: 118, width: 9, hubR: 44, plate: true });

const out = resolve(dirname(fileURLToPath(import.meta.url)), '..', 'public');
mkdirSync(out, { recursive: true });
writeFileSync(resolve(out, 'favicon.svg'), favicon);
writeFileSync(resolve(out, 'icon.svg'), large);
console.log(`wrote favicon.svg (${favicon.length}b), icon.svg (${large.length}b)`);
