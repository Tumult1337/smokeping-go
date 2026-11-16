import { existsSync, readdirSync, readFileSync, statSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { resolveConfig } from "vite";

const uiDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const distDir = path.resolve(uiDir, "../internal/ui/dist");
const assetsDir = path.join(distDir, "assets");

// Rolldown's default [hash] is 8 base64url characters, so a hash can hold "-" and "_".
const CONTENT_HASHED_NAME = /-[A-Za-z0-9_-]{8}\.[A-Za-z0-9]+$/;
const OUTPUT_NAME_OPTIONS = ["entryFileNames", "chunkFileNames", "assetFileNames"];
const BUILD_OPTION_KEYS = ["rolldownOptions", "rollupOptions"];

function builtFiles(dir) {
  return readdirSync(dir, { recursive: true, withFileTypes: true })
    .filter((entry) => entry.isFile())
    .map((entry) => path.join(entry.parentPath, entry.name));
}

function checkAssetNamesCarryAContentHash() {
  if (!existsSync(assetsDir)) return [`${assetsDir} is missing`];
  const files = builtFiles(assetsDir);
  if (files.length === 0) return [`${assetsDir} holds no file`];
  return files
    .filter((file) => !CONTENT_HASHED_NAME.test(path.basename(file)))
    .map((file) => `${path.relative(distDir, file)} has no content hash in its name, so the immutable cache rule would serve it stale`);
}

function checkCSSInlinesNoDataURL() {
  if (!existsSync(assetsDir)) return [];
  return builtFiles(assetsDir)
    .filter((file) => file.endsWith(".css"))
    .filter((file) => /url\(\s*["']?\s*data:/i.test(readFileSync(file, "utf8")))
    .map((file) => `${path.relative(distDir, file)} holds url(data:...), which the CSP font-src 'self' and img-src 'self' refuse`);
}

function checkIndexHasNoInlineScriptOrStyle() {
  const indexPath = path.join(distDir, "index.html");
  if (!existsSync(indexPath)) return [`${indexPath} is missing`];
  const html = readFileSync(indexPath, "utf8");
  const failures = [];
  for (const match of html.matchAll(/<script\b([^>]*)>/gi)) {
    if (!/\ssrc\s*=/i.test(` ${match[1]}`)) failures.push(`index.html holds an inline script, which the CSP script-src 'self' refuses: ${match[0]}`);
  }
  for (const match of html.matchAll(/<[a-z][^>]*\sstyle\s*=[^>]*>/gi)) {
    failures.push(`index.html holds a style attribute, which the CSP style-src 'self' refuses: ${match[0]}`);
  }
  if (/<style\b/i.test(html)) failures.push("index.html holds a <style> element, which the CSP style-src 'self' refuses");
  return failures;
}

function checkThemeScriptIsBuilt() {
  const themePath = path.join(distDir, "theme.js");
  if (existsSync(themePath) && statSync(themePath).isFile()) return [];
  return [`${themePath} is missing, so the page paints before the stored theme applies`];
}

async function checkBuildConfigKeepsTheHashedDefaultNames() {
  const config = await resolveConfig({ configFile: path.join(uiDir, "vite.config.ts"), logLevel: "silent" }, "build", "production");
  const builds = [["build", config.build], ...Object.entries(config.environments ?? {}).map(([name, env]) => [`environments.${name}.build`, env.build])];
  const failures = [];
  for (const [buildPath, build] of builds) {
    for (const optionsKey of BUILD_OPTION_KEYS) {
      const output = build?.[optionsKey]?.output;
      if (output == null) continue;
      const outputs = Array.isArray(output) ? output : [output];
      outputs.forEach((entry, index) => {
        const entryPath = Array.isArray(output) ? `${buildPath}.${optionsKey}.output[${index}]` : `${buildPath}.${optionsKey}.output`;
        for (const option of OUTPUT_NAME_OPTIONS) {
          if (entry?.[option] !== undefined) failures.push(`${entryPath}.${option} is set; only the default name carries a content hash for certain`);
        }
      });
    }
  }
  return failures;
}

const rules = [
  ["asset names carry a content hash", checkAssetNamesCarryAContentHash],
  ["built CSS inlines no data: URL", checkCSSInlinesNoDataURL],
  ["built index.html has no inline script or style", checkIndexHasNoInlineScriptOrStyle],
  ["theme.js is built", checkThemeScriptIsBuilt],
  ["build config keeps the hashed default names", checkBuildConfigKeepsTheHashedDefaultNames],
];

let failed = false;
for (const [rule, check] of rules) {
  const failures = await check();
  if (failures.length === 0) {
    console.log(`check-dist: pass: ${rule}`);
    continue;
  }
  failed = true;
  for (const failure of failures) console.error(`check-dist: FAIL: ${rule}: ${failure}`);
}
if (failed) process.exit(1);
