import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import vm from "node:vm";

import { scriptTags, withoutComments } from "../scripts/html-script-tags.mjs";

const DARK_QUERY = "(prefers-color-scheme: dark)";

function readIndexHtml() {
  return readFileSync(new URL("../index.html", import.meta.url), "utf8");
}

function readThemeScript() {
  return readFileSync(new URL("../public/theme.js", import.meta.url), "utf8");
}

function runThemeScript({ stored, prefersDark, storage = "available" }) {
  const root = { dataset: {}, style: {} };
  const requestedKeys = [];
  const context = {
    matchMedia: (query) => ({ matches: query === DARK_QUERY && prefersDark }),
    document: { documentElement: root },
  };
  if (storage === "inaccessible") {
    Object.defineProperty(context, "localStorage", {
      get() {
        throw new Error("SecurityError: storage is disabled");
      },
    });
  } else {
    context.localStorage = {
      getItem(key) {
        requestedKeys.push(key);
        if (storage === "throws") throw new Error("QuotaExceededError");
        return stored;
      },
    };
  }
  vm.runInNewContext(readThemeScript(), context);
  return { theme: root.dataset.theme, colorScheme: root.style.colorScheme, requestedKeys };
}

test("a script tag inside an HTML comment does not count", () => {
  const html = [
    '<script src="/kept.js"></script>',
    '<!-- <script src="/theme.js"></script> -->',
    '<!--\n<script src="/multi-line.js"></script>\n-->',
    '<script type="module" src="/entry.js"></script>',
    '<!-- unterminated <script src="/after-open-comment.js"></script>',
  ].join("\n");
  assert.deepEqual(
    scriptTags(html).map((tag) => tag.attributes.get("src")),
    ["/kept.js", "/entry.js"],
  );
});

test("index.html holds no script without src", () => {
  const tags = scriptTags(readIndexHtml());
  assert.ok(tags.length > 0, "index.html holds no script tag at all");
  for (const tag of tags) {
    assert.ok(tag.attributes.has("src"), `script without src: ${JSON.stringify(Object.fromEntries(tag.attributes))}`);
    assert.equal(tag.body.trim(), "", "a script with src holds inline code");
  }
});

test("the classic /theme.js script comes before the module script", () => {
  const tags = scriptTags(readIndexHtml());
  const themeIndex = tags.findIndex((tag) => tag.attributes.get("src") === "/theme.js");
  const moduleIndex = tags.findIndex((tag) => tag.attributes.get("type") === "module");
  assert.notEqual(themeIndex, -1, "index.html does not load /theme.js");
  assert.notEqual(moduleIndex, -1, "index.html has no module entry");
  assert.ok(themeIndex < moduleIndex, "/theme.js must come before the module entry");
});

test("the theme script blocks the parser: no async, no defer, no type", () => {
  const theme = scriptTags(readIndexHtml()).find((tag) => tag.attributes.get("src") === "/theme.js");
  assert.ok(theme, "index.html does not load /theme.js");
  for (const attribute of ["async", "defer", "type"]) {
    assert.equal(theme.attributes.has(attribute), false, `the theme script has the ${attribute} attribute`);
  }
  const head = withoutComments(readIndexHtml()).split(/<\/head>/i)[0];
  assert.match(head, /<script\b[^>]*\bsrc="\/theme\.js"/, "the theme script is not in <head>");
});

test("theme.js applies the stored preference before first paint", () => {
  const cases = [
    { stored: "light", prefersDark: true, theme: "light", colorScheme: "light" },
    { stored: "light", prefersDark: false, theme: "light", colorScheme: "light" },
    { stored: "dark", prefersDark: true, theme: "dark", colorScheme: "dark" },
    { stored: "dark", prefersDark: false, theme: "dark", colorScheme: "dark" },
    { stored: "system", prefersDark: true, theme: "dark", colorScheme: "dark" },
    { stored: "system", prefersDark: false, theme: "light", colorScheme: "light" },
    { stored: null, prefersDark: true, theme: "dark", colorScheme: "dark" },
    { stored: null, prefersDark: false, theme: "light", colorScheme: "light" },
    { stored: "unknown", prefersDark: true, theme: "dark", colorScheme: "dark" },
    { stored: "unknown", prefersDark: false, theme: "light", colorScheme: "light" },
    { storage: "throws", prefersDark: false, theme: "dark", colorScheme: undefined },
    { storage: "throws", prefersDark: true, theme: "dark", colorScheme: undefined },
    { storage: "inaccessible", prefersDark: false, theme: "dark", colorScheme: undefined },
  ];
  for (const { theme, colorScheme, ...input } of cases) {
    const result = runThemeScript(input);
    assert.equal(result.theme, theme, `data-theme for ${JSON.stringify(input)}`);
    assert.equal(result.colorScheme, colorScheme, `colorScheme for ${JSON.stringify(input)}`);
  }
});

test("theme.js and readPref of theme.ts read the same key and resolve the same theme", async () => {
  const { readPref, resolveTheme } = await import("../src/theme.ts");
  const keysReadByThemeTs = [];
  for (const stored of ["light", "dark", "system", null, "unknown"]) {
    for (const prefersDark of [true, false]) {
      globalThis.localStorage = {
        getItem(key) {
          keysReadByThemeTs.push(key);
          return stored;
        },
      };
      globalThis.matchMedia = (query) => ({ matches: query === DARK_QUERY && prefersDark });
      let expected;
      try {
        expected = resolveTheme(readPref());
      } finally {
        delete globalThis.localStorage;
        delete globalThis.matchMedia;
      }
      const result = runThemeScript({ stored, prefersDark });
      assert.equal(result.theme, expected, `stored ${JSON.stringify(stored)}, prefersDark ${prefersDark}`);
      assert.deepEqual(result.requestedKeys, [keysReadByThemeTs.at(-1)], "theme.js and readPref read different keys");
    }
  }
  assert.equal(new Set(keysReadByThemeTs).size, 1, "readPref read more than one key");
});
