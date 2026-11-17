import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";
import { build } from "vite";

import {
  checkAssetNamesCarryAContentHash,
  checkBuildConfigKeepsTheHashedDefaultNamesAndInlinesNoAsset,
  checkCodeHoldsNoDataURL,
  checkIndexHasNoInlineCode,
  checkIndexHoldsOneDarkReaderLockMetaInHeadBeforeTheFirstScript,
  checkIndexLoadsThemeScriptBeforeTheModuleEntry,
  checkPublicDirHoldsNoAssetsFolder,
  checkThemeScriptIsBuilt,
  distRules,
  resolveBuildConfig,
  uiDir,
} from "../scripts/check-dist.mjs";

const HASHED_JS = "assets/index-AbCd12_-.js";
const HASHED_CSS = "assets/index-Xy-9zZ0a.css";
const CLEAN_HEAD = '<script src="/theme.js"></script><script type="module" crossorigin src="/assets/index-AbCd12_-.js"></script><link rel="stylesheet" href="/assets/index-Xy-9zZ0a.css">';
const CLEAN_INDEX = `<!doctype html><html lang="en"><head><meta charset="UTF-8"><meta name="darkreader-lock"><title>gosmokeping</title>${CLEAN_HEAD}</head><body><div id="root"></div></body></html>`;
const DEFAULT_BUILD = { assetsInlineLimit: 0 };

function tempDir(t, files = {}) {
  const dir = mkdtempSync(path.join(tmpdir(), "check-dist-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  for (const [name, content] of Object.entries(files)) {
    const file = path.join(dir, name);
    mkdirSync(path.dirname(file), { recursive: true });
    writeFileSync(file, content);
  }
  return dir;
}

function indexFailures(t, html) {
  return checkIndexHasNoInlineCode(tempDir(t, { "index.html": html }));
}

function assertOneFailure(failures, pattern, context) {
  assert.equal(failures.length, 1, `${context}: want one failure, got ${JSON.stringify(failures)}`);
  assert.match(failures[0], pattern, context);
}

test("an asset name with no content hash fails", (t) => {
  const dist = tempDir(t, { [HASHED_JS]: "", [HASHED_CSS]: "", "assets/geist-latin-wght-normal-DC-KSUi6.woff2": "", "assets/logo.png": "" });
  assertOneFailure(checkAssetNamesCarryAContentHash(dist), /assets\/logo\.png has no content hash/, "logo.png");
  assertOneFailure(checkAssetNamesCarryAContentHash(tempDir(t)), /assets is missing/, "no assets folder");
  const emptyAssets = tempDir(t);
  mkdirSync(path.join(emptyAssets, "assets"));
  assertOneFailure(checkAssetNamesCarryAContentHash(emptyAssets), /assets holds no file/, "empty assets folder");
});

test("an assets entry in the public dir fails, because Vite copies it to dist/assets under its own name", (t) => {
  const publicFolder = tempDir(t, { "assets/export-template.csv": "a,b\n", "theme.js": "" });
  assertOneFailure(checkPublicDirHoldsNoAssetsFolder(publicFolder), /assets exists/, "public/assets/export-template.csv");
  const emptyFolder = tempDir(t);
  mkdirSync(path.join(emptyFolder, "assets"));
  assertOneFailure(checkPublicDirHoldsNoAssetsFolder(emptyFolder), /assets exists/, "an empty public/assets folder");
  assertOneFailure(checkPublicDirHoldsNoAssetsFolder(tempDir(t, { assets: "a file" })), /assets exists/, "a public/assets file");
  assert.deepEqual(checkPublicDirHoldsNoAssetsFolder(tempDir(t, { "theme.js": "", "favicon.svg": "" })), []);
  assert.deepEqual(checkPublicDirHoldsNoAssetsFolder(""), [], "publicDir: false resolves to an empty string");
});

test("the public dir rule reads the public dir of the resolved Vite config", async (t) => {
  const config = await resolveBuildConfig();
  assert.equal(config.publicDir, path.join(uiDir, "public"));
  const publicDir = tempDir(t, { "assets/export-template.csv": "a,b\n" });
  const rule = distRules(tempDir(t), { build: DEFAULT_BUILD, publicDir }).find(([name]) => name === "the public dir holds no assets folder");
  assert.ok(rule, "distRules has no public dir rule");
  assertOneFailure(rule[1](), /assets exists/, "distRules with a public assets folder");
});

test("the data: rule reads each JS, CSS and HTML file of the dist and matches only a data URL", (t) => {
  const cases = [
    { file: HASHED_JS, text: "console.log(`data:image/svg+xml,%3csvg%20xmlns='http://www.w3.org/2000/svg'%3e`)", fails: true },
    { file: HASHED_JS, text: 'const font="data:font/woff2;base64,d09GMgAB"', fails: true },
    { file: HASHED_JS, text: 'const png="DATA:image/png;base64,AAAA"', fails: true },
    { file: HASHED_JS, text: 'fetch("data:,hello")', fails: true },
    { file: "assets/chunk-AbCd12_-.mjs", text: 'export default "data:application/octet-stream;base64,AA"', fails: true },
    { file: HASHED_CSS, text: "@font-face{src:url(data:font/woff2;base64,d09G)}", fails: true },
    { file: HASHED_CSS, text: '@font-face{src:url("data:font/woff2;base64,d09G")}', fails: true },
    { file: "theme.js", text: 'document.body.style.backgroundImage="url(data:image/png;base64,AAAA)"', fails: true },
    { file: "index.html", text: '<link rel="icon" href="data:image/svg+xml,%3csvg%3e">', fails: true },
    { file: HASHED_JS, text: "L={pending:!1,data:null,method:null,action:null};wn={clipboardData:function(e){return`clipboardData`in e?e.clipboardData:window.clipboardData}};Tn={data:0};x={data:e,meta:t};y={data:e/t,z:1};m={metadata:image/png,k:1}", fails: false },
    { file: "assets/font-AbCd12_-.woff2", text: "data:image/png;base64,AAAA", fails: false },
  ];
  for (const { file, text, fails } of cases) {
    const failures = checkCodeHoldsNoDataURL(tempDir(t, { [file]: text }));
    assert.equal(failures.length, fails ? 1 : 0, `${file}: ${text}: ${JSON.stringify(failures)}`);
    if (fails) assert.match(failures[0], new RegExp(`^${file.replace(/[.]/g, "\\.")} holds the data: URL`), file);
  }
});

test("the data: rule fails on the real bundler output of an ?inline import", async (t) => {
  const project = tempDir(t, {
    "index.html": '<!doctype html><script type="module" src="/src/main.js"></script>\n',
    "src/main.js": 'import dot from "./dot.svg?inline";\nconsole.log(dot);\n',
    "src/dot.svg": '<svg xmlns="http://www.w3.org/2000/svg"><rect width="1" height="1"/></svg>',
  });
  const outDir = path.join(project, "dist");
  await build({ root: project, configFile: false, logLevel: "silent", build: { outDir, emptyOutDir: true, assetsInlineLimit: 0 } });
  assertOneFailure(checkCodeHoldsNoDataURL(outDir), /holds the data: URL data:image\/svg\+xml,/, "?inline import");
});

test("an event handler attribute in index.html fails, one in a comment does not count", (t) => {
  const failing = [
    ['<body onload="init()">', /onload on <body>/],
    ['<img src="/a.png" onerror=alert(1)>', /onerror on <img>/],
    ['<div ONCLICK="x()"></div>', /onclick on <div>/],
    ['<body title="a>b" onload="x()">', /onload on <body>/],
    ['<!-- note --!><body onload="x()"> -->', /onload on <body>/],
    ['<!--><body onload="x()"> -->', /onload on <body>/],
    ['<!---><body onload="x()"> -->', /onload on <body>/],
    ['<title><!--</title><body onload="x()">--></title>', /onload on <body>/],
  ];
  for (const [fragment, pattern] of failing) {
    assertOneFailure(indexFailures(t, CLEAN_INDEX.replace("<body>", fragment)), pattern, fragment);
  }
  for (const fragment of ['<!-- <body onload="init()"> --><body>', '<body data-onload="x" class="online">']) {
    assert.deepEqual(indexFailures(t, CLEAN_INDEX.replace("<body>", fragment)), [], fragment);
  }
});

test("an inline script in index.html fails, a script tag in a comment does not count", (t) => {
  const failing = [
    "<script>alert(1)</script>",
    '<script type="module">import "/x.js"</script>',
    '<script src="/a.js"><!--</script><script>alert(1)</script>-->',
  ];
  for (const fragment of failing) {
    assertOneFailure(indexFailures(t, CLEAN_INDEX.replace("</head>", `${fragment}</head>`)), /inline script/, fragment);
  }
  assert.deepEqual(indexFailures(t, CLEAN_INDEX), []);
  const commented = CLEAN_INDEX.replace("</head>", "<!-- <script>alert(1)</script> --></head>");
  assert.deepEqual(indexFailures(t, commented), []);
});

test("a style attribute or a <style> element in index.html fails, one in a comment does not count", (t) => {
  const failing = [
    ['<div style="color:red"></div>', /style attribute on <div>/],
    ["<style>body{margin:0}</style>", /<style> element/],
    ["<STYLE>body{margin:0}</STYLE>", /<style> element/],
  ];
  for (const [fragment, pattern] of failing) {
    assertOneFailure(indexFailures(t, CLEAN_INDEX.replace("<body>", `<body>${fragment}`)), pattern, fragment);
  }
  const commented = CLEAN_INDEX.replace("<body>", '<body><!-- <div style="color:red"></div><style>body{}</style> -->');
  assert.deepEqual(indexFailures(t, commented), []);
  assertOneFailure(checkIndexHasNoInlineCode(tempDir(t)), /index\.html is missing/, "no index.html");
});

test("theme.js must be built as a file", (t) => {
  assert.deepEqual(checkThemeScriptIsBuilt(tempDir(t, { "theme.js": "" })), []);
  assertOneFailure(checkThemeScriptIsBuilt(tempDir(t)), /theme\.js is missing/, "no theme.js");
  assertOneFailure(checkThemeScriptIsBuilt(tempDir(t, { "theme.js/x": "" })), /theme\.js is missing/, "theme.js is a folder");
});

test("index.html must load a parser-blocking /theme.js before the module entry", (t) => {
  const check = (html) => checkIndexLoadsThemeScriptBeforeTheModuleEntry(tempDir(t, { "index.html": html }));
  assert.deepEqual(check(CLEAN_INDEX), []);
  const theme = '<script src="/theme.js"></script>';
  const entry = '<script type="module" crossorigin src="/assets/index-AbCd12_-.js"></script>';
  assertOneFailure(check(CLEAN_INDEX.replace(theme, "")), /loads no \/theme\.js/, "no theme script");
  assertOneFailure(check(CLEAN_INDEX.replace(theme, `<!-- ${theme} -->`)), /loads no \/theme\.js/, "theme script in a comment");
  assertOneFailure(check(CLEAN_INDEX.replace(theme, '<script defer src="/theme.js"></script>')), /defer attribute/, "defer");
  assertOneFailure(check(CLEAN_INDEX.replace(theme, '<script async src="/theme.js"></script>')), /async attribute/, "async");
  assertOneFailure(check(CLEAN_INDEX.replace(theme, '<script type="text/javascript" src="/theme.js"></script>')), /type attribute/, "type");
  assertOneFailure(check(CLEAN_INDEX.replace(entry, "")), /no module entry/, "no module entry");
  assertOneFailure(check(CLEAN_INDEX.replace(`${theme}${entry}`, `${entry}${theme}`)), /after the module entry/, "theme after entry");
});

test("index.html must hold one darkreader-lock meta tag in head before the first script", (t) => {
  const check = (html) => checkIndexHoldsOneDarkReaderLockMetaInHeadBeforeTheFirstScript(tempDir(t, { "index.html": html }));
  const lock = '<meta name="darkreader-lock">';
  assert.deepEqual(check(CLEAN_INDEX), []);
  assertOneFailure(check(CLEAN_INDEX.replace(lock, "")), /holds no darkreader-lock meta tag/, "tag missing");
  assertOneFailure(check(CLEAN_INDEX.replace(lock, `${lock}${lock}`)), /holds 2 darkreader-lock meta tags/, "tag twice");
  assertOneFailure(check(CLEAN_INDEX.replace(lock, `<!-- ${lock} -->`)), /holds no darkreader-lock meta tag/, "tag in a comment");
  assertOneFailure(check(CLEAN_INDEX.replace(lock, "").replace("</head>", `${lock}</head>`)), /after the first script/, "tag after the first script");
  assertOneFailure(check(CLEAN_INDEX.replace(lock, "").replace("<body>", `<body>${lock}`)), /not inside head/, "tag in body");
  assertOneFailure(checkIndexHoldsOneDarkReaderLockMetaInHeadBeforeTheFirstScript(tempDir(t)), /index\.html is missing/, "no index.html");
  const rule = distRules(tempDir(t, { "index.html": CLEAN_INDEX.replace(lock, "") }), { build: DEFAULT_BUILD }).find(([name]) => name.includes("darkreader-lock"));
  assert.ok(rule, "distRules has no darkreader-lock rule");
  assertOneFailure(rule[1](), /holds no darkreader-lock meta tag/, "distRules without the tag");
});

test("the build config must keep the default output names and inline no asset", async (t) => {
  const check = checkBuildConfigKeepsTheHashedDefaultNamesAndInlinesNoAsset;
  assert.deepEqual(check(await resolveBuildConfig()), [], "ui/vite.config.ts");
  assert.deepEqual(check({ build: DEFAULT_BUILD, environments: { client: { build: DEFAULT_BUILD } } }), []);
  assertOneFailure(check({ build: { assetsInlineLimit: 4096 } }), /build\.assetsInlineLimit is 4096/, "limit 4096");
  assertOneFailure(check({ build: { assetsInlineLimit: () => true } }), /assetsInlineLimit is a function/, "limit function");
  assertOneFailure(check({ build: DEFAULT_BUILD, environments: { client: { build: {} } } }), /environments\.client\.build\.assetsInlineLimit is undefined/, "environment limit");
  assertOneFailure(check({ build: { ...DEFAULT_BUILD, rolldownOptions: { output: { assetFileNames: "[name].[ext]" } } } }), /rolldownOptions\.output\.assetFileNames is set/, "assetFileNames");
  assertOneFailure(check({ build: { ...DEFAULT_BUILD, rollupOptions: { output: [{}, { entryFileNames: "[name].js" }] } } }), /rollupOptions\.output\[1\]\.entryFileNames is set/, "entryFileNames in an output array");
  assertOneFailure(check({ build: { ...DEFAULT_BUILD, rolldownOptions: { output: { chunkFileNames: "c.js" } } } }), /chunkFileNames is set/, "chunkFileNames");
});
