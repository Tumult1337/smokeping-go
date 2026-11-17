import { existsSync, lstatSync, readdirSync, readFileSync, realpathSync, statSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { resolveConfig } from "vite";

import { scriptTags, startTags } from "./html-script-tags.mjs";

export const uiDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const builtDistDir = path.resolve(uiDir, "../internal/ui/dist");

// Rolldown's default [hash] is 8 base64url characters, so a hash can hold "-" and "_".
const CONTENT_HASHED_NAME = /-[A-Za-z0-9_-]{8}\.[A-Za-z0-9]+$/;
const DATA_URL = /(?<![A-Za-z0-9+.-])data:(?:(?:application|audio|example|font|haptics|image|message|model|multipart|text|video)\/[A-Za-z0-9!#$&^_.+-]+)?(?:;[^\s,;"'`()]*)*,/i;
const BROWSER_CODE_FILE = /\.(?:css|html|js|mjs)$/i;
const OUTPUT_NAME_OPTIONS = ["entryFileNames", "chunkFileNames", "assetFileNames"];
const BUILD_OPTION_KEYS = ["rolldownOptions", "rollupOptions"];

function builtFiles(dir) {
  return readdirSync(dir, { recursive: true, withFileTypes: true })
    .filter((entry) => entry.isFile())
    .map((entry) => path.join(entry.parentPath, entry.name));
}

export function checkAssetNamesCarryAContentHash(distDir) {
  const assetsDir = path.join(distDir, "assets");
  if (!existsSync(assetsDir)) return [`${assetsDir} is missing`];
  const files = builtFiles(assetsDir);
  if (files.length === 0) return [`${assetsDir} holds no file`];
  return files
    .filter((file) => !CONTENT_HASHED_NAME.test(path.basename(file)))
    .map((file) => `${path.relative(distDir, file)} has no content hash in its name, so the immutable cache rule would serve it stale`);
}

export function checkPublicDirHoldsNoAssetsFolder(publicDir) {
  if (!publicDir) return [];
  const publicAssets = path.join(publicDir, "assets");
  if (lstatSync(publicAssets, { throwIfNoEntry: false }) === undefined) return [];
  return [`${publicAssets} exists: Vite copies it to dist/assets with no content hash in the names, and the server sends each file there as immutable for one year`];
}

export function checkCodeHoldsNoDataURL(distDir) {
  if (!existsSync(distDir)) return [`${distDir} is missing`];
  return builtFiles(distDir)
    .filter((file) => BROWSER_CODE_FILE.test(file))
    .flatMap((file) => {
      const dataURL = readFileSync(file, "utf8").match(DATA_URL);
      if (dataURL === null) return [];
      return [`${path.relative(distDir, file)} holds the data: URL ${dataURL[0]}..., which the CSP refuses, because no directive allows data:`];
    });
}

export function checkIndexHasNoInlineCode(distDir) {
  const indexPath = path.join(distDir, "index.html");
  if (!existsSync(indexPath)) return [`${indexPath} is missing`];
  const failures = [];
  for (const tag of startTags(readFileSync(indexPath, "utf8"))) {
    if (tag.name === "script" && !tag.attributes.has("src")) failures.push("index.html holds an inline script, which the CSP script-src 'self' refuses");
    if (tag.name === "style") failures.push("index.html holds a <style> element, which the CSP style-src 'self' refuses");
    if (tag.attributes.has("style")) failures.push(`index.html holds a style attribute on <${tag.name}>, which the CSP style-src 'self' refuses`);
    for (const name of tag.attributes.keys()) {
      if (name.startsWith("on")) failures.push(`index.html holds the event handler attribute ${name} on <${tag.name}>, which the CSP script-src 'self' refuses`);
    }
  }
  return failures;
}

export function checkThemeScriptIsBuilt(distDir) {
  const themePath = path.join(distDir, "theme.js");
  if (existsSync(themePath) && statSync(themePath).isFile()) return [];
  return [`${themePath} is missing, so the page paints before the stored theme applies`];
}

export function checkIndexLoadsThemeScriptBeforeTheModuleEntry(distDir) {
  const indexPath = path.join(distDir, "index.html");
  if (!existsSync(indexPath)) return [`${indexPath} is missing`];
  const tags = scriptTags(readFileSync(indexPath, "utf8"));
  const themeIndex = tags.findIndex((tag) => tag.attributes.get("src") === "/theme.js");
  if (themeIndex === -1) return ["index.html loads no /theme.js outside a comment, so the page paints before the stored theme applies"];
  const failures = [];
  for (const attribute of ["async", "defer", "type"]) {
    if (tags[themeIndex].attributes.has(attribute)) failures.push(`the /theme.js tag has the ${attribute} attribute, so it does not block the parser`);
  }
  const moduleIndex = tags.findIndex((tag) => tag.attributes.get("type") === "module");
  if (moduleIndex === -1) failures.push("index.html has no module entry");
  if (moduleIndex !== -1 && themeIndex > moduleIndex) failures.push("/theme.js comes after the module entry");
  return failures;
}

export function checkIndexHoldsOneDarkReaderLockMetaInHeadBeforeTheFirstScript(distDir) {
  const indexPath = path.join(distDir, "index.html");
  if (!existsSync(indexPath)) return [`${indexPath} is missing`];
  const tags = startTags(readFileSync(indexPath, "utf8"));
  const lockIndexes = tags.flatMap((tag, index) => (tag.name === "meta" && tag.attributes.get("name") === "darkreader-lock" ? [index] : []));
  if (lockIndexes.length === 0) return ["index.html holds no darkreader-lock meta tag, so Dark Reader adds an inline script that the CSP refuses"];
  if (lockIndexes.length > 1) return [`index.html holds ${lockIndexes.length} darkreader-lock meta tags, not 1`];
  const [lockIndex] = lockIndexes;
  const headIndex = tags.findIndex((tag) => tag.name === "head");
  const bodyIndex = tags.findIndex((tag) => tag.name === "body");
  if (headIndex === -1 || headIndex > lockIndex || (bodyIndex !== -1 && bodyIndex < lockIndex)) return ["the darkreader-lock meta tag is not inside head"];
  const scriptIndex = tags.findIndex((tag) => tag.name === "script");
  if (scriptIndex !== -1 && scriptIndex < lockIndex) return ["the darkreader-lock meta tag comes after the first script"];
  return [];
}

export function checkBuildConfigKeepsTheHashedDefaultNamesAndInlinesNoAsset(config) {
  const builds = [["build", config.build], ...Object.entries(config.environments ?? {}).map(([name, env]) => [`environments.${name}.build`, env.build])];
  const failures = [];
  for (const [buildPath, build] of builds) {
    const inlineLimit = build?.assetsInlineLimit;
    if (inlineLimit !== 0) {
      const shown = typeof inlineLimit === "function" ? "a function" : String(inlineLimit);
      failures.push(`${buildPath}.assetsInlineLimit is ${shown}, not 0, so a small asset becomes a data: URL that the CSP refuses`);
    }
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

export function resolveBuildConfig() {
  return resolveConfig({ configFile: path.join(uiDir, "vite.config.ts"), logLevel: "silent" }, "build", "production");
}

export function distRules(distDir, config) {
  return [
    ["asset names carry a content hash", () => checkAssetNamesCarryAContentHash(distDir)],
    ["the public dir holds no assets folder", () => checkPublicDirHoldsNoAssetsFolder(config.publicDir)],
    ["built code holds no data: URL", () => checkCodeHoldsNoDataURL(distDir)],
    ["built index.html has no inline script, style, or event handler", () => checkIndexHasNoInlineCode(distDir)],
    ["theme.js is built", () => checkThemeScriptIsBuilt(distDir)],
    ["built index.html loads /theme.js before the module entry", () => checkIndexLoadsThemeScriptBeforeTheModuleEntry(distDir)],
    ["built index.html holds one darkreader-lock meta tag in head before the first script", () => checkIndexHoldsOneDarkReaderLockMetaInHeadBeforeTheFirstScript(distDir)],
    ["build config keeps the hashed default names and inlines no asset", () => checkBuildConfigKeepsTheHashedDefaultNamesAndInlinesNoAsset(config)],
  ];
}

async function main() {
  const config = await resolveBuildConfig();
  let failed = false;
  for (const [rule, check] of distRules(builtDistDir, config)) {
    const failures = check();
    if (failures.length === 0) {
      console.log(`check-dist: pass: ${rule}`);
      continue;
    }
    failed = true;
    for (const failure of failures) console.error(`check-dist: FAIL: ${rule}: ${failure}`);
  }
  if (failed) process.exit(1);
}

if (process.argv[1] !== undefined && realpathSync(process.argv[1]) === fileURLToPath(import.meta.url)) await main();
