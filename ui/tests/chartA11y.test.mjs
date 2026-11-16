import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

import {
  chartName,
  decodeKey,
  decodeKeyInMode,
  clampBoxStart,
  formatLocalTime,
  isKeyboardMode,
  keyboardCursorPlacement,
  nextAnnouncement,
  resolveZoomNow,
  edgeIndex,
  heatmapNoDataText,
  heatmapReadoutText,
  heatmapSummaryText,
  heatmapTooltipLeft,
  hopAddressText,
  replyCountOf,
  cellReplied,
  nextHeldName,
  stableName,
  summarizeHeatmap,
  httpReadoutText,
  httpSummaryText,
  isZoomed,
  keyHelp,
  keyHelpText,
  largeStep,
  latencyReadoutText,
  latencySummaryText,
  lossReadoutText,
  lossSummaryText,
  moveIndex,
  moveRow,
  nearestIndex,
  pickAnnouncement,
  shouldAnnounce,
  statusSummaryText,
  windowText,
  zoomAnnouncement,
  zoomIn,
  zoomOut,
  zoomReset,
} from "../src/chartA11y.ts";

const FORMAT = { locale: "en-US", timeZone: "UTC" };

function localeString(sec) {
  return new Date(sec * 1000).toLocaleString("en-US", { timeZone: "UTC" });
}

function key(name, overrides = {}) {
  return {
    key: name,
    shiftKey: false,
    ctrlKey: false,
    altKey: false,
    metaKey: false,
    repeat: false,
    isComposing: false,
    ...overrides,
  };
}

const ALL_KINDS = ["band", "bars", "http", "heatmap", "loss"];

test("arrows move one sample, Shift and Page keys move a large step", () => {
  for (const kind of ALL_KINDS) {
    assert.deepEqual(decodeKey(key("ArrowRight"), kind), { type: "step", direction: 1, large: false });
    assert.deepEqual(decodeKey(key("ArrowLeft"), kind), { type: "step", direction: -1, large: false });
    assert.deepEqual(decodeKey(key("ArrowRight", { shiftKey: true }), kind), { type: "step", direction: 1, large: true });
    assert.deepEqual(decodeKey(key("ArrowLeft", { shiftKey: true }), kind), { type: "step", direction: -1, large: true });
    assert.deepEqual(decodeKey(key("PageUp"), kind), { type: "step", direction: 1, large: true });
    assert.deepEqual(decodeKey(key("PageDown"), kind), { type: "step", direction: -1, large: true });
  }
});

test("Home and End go to the oldest and the newest sample", () => {
  for (const kind of ALL_KINDS) {
    assert.deepEqual(decodeKey(key("Home"), kind), { type: "edge", edge: "oldest" });
    assert.deepEqual(decodeKey(key("End"), kind), { type: "edge", edge: "newest" });
  }
});

test("Up and Down move a hop row on the heatmap only", () => {
  assert.deepEqual(decodeKey(key("ArrowUp"), "heatmap"), { type: "row", direction: -1 });
  assert.deepEqual(decodeKey(key("ArrowDown"), "heatmap"), { type: "row", direction: 1 });
  for (const kind of ["band", "bars", "http", "loss"]) {
    assert.equal(decodeKey(key("ArrowUp"), kind), null);
    assert.equal(decodeKey(key("ArrowDown"), kind), null);
  }
});

test("Enter and Space pick on every chart except HTTP", () => {
  for (const kind of ["band", "bars", "heatmap", "loss"]) {
    assert.deepEqual(decodeKey(key("Enter"), kind), { type: "pick" });
    assert.deepEqual(decodeKey(key(" "), kind), { type: "pick" });
  }
  assert.equal(decodeKey(key("Enter"), "http"), null);
  assert.equal(decodeKey(key(" "), "http"), null);
});

test("zoom keys act on the three uPlot charts only", () => {
  for (const kind of ["band", "bars", "http"]) {
    assert.deepEqual(decodeKey(key("+", { shiftKey: true }), kind), { type: "zoomIn" });
    assert.deepEqual(decodeKey(key("="), kind), { type: "zoomIn" });
    assert.deepEqual(decodeKey(key("-"), kind), { type: "zoomOut" });
    assert.deepEqual(decodeKey(key("0"), kind), { type: "zoomReset" });
  }
  for (const kind of ["heatmap", "loss"]) {
    for (const name of ["+", "=", "-", "0"]) {
      assert.equal(decodeKey(key(name), kind), null);
    }
  }
});

test("Escape hides the cursor on every chart", () => {
  for (const kind of ALL_KINDS) {
    assert.deepEqual(decodeKey(key("Escape"), kind), { type: "escape" });
  }
});

test("Ctrl, Alt, Meta, and composition pass every key through", () => {
  const names = ["0", "-", "=", "+", "ArrowRight", "Home", "Enter", " ", "Escape", "PageUp"];
  for (const kind of ALL_KINDS) {
    for (const name of names) {
      for (const flag of ["ctrlKey", "altKey", "metaKey", "isComposing"]) {
        assert.equal(decodeKey(key(name, { [flag]: true }), kind), null, `${flag} ${name} ${kind}`);
      }
    }
  }
});

test("Ctrl+0 gives no action", () => {
  assert.equal(decodeKey(key("0", { ctrlKey: true }), "band"), null);
});

test("Shift counts only for the arrows and for plus", () => {
  for (const name of ["Home", "End", "Enter", " ", "Escape", "PageUp", "PageDown"]) {
    assert.equal(decodeKey(key(name, { shiftKey: true }), "band"), null, name);
  }
  assert.equal(decodeKey(key("ArrowUp", { shiftKey: true }), "heatmap"), null);
});

test("the zoom characters act with or without Shift", () => {
  const expected = { "+": "zoomIn", "=": "zoomIn", "-": "zoomOut", "0": "zoomReset" };
  for (const kind of ["band", "bars", "http"]) {
    for (const [name, type] of Object.entries(expected)) {
      for (const shiftKey of [false, true]) {
        assert.deepEqual(decodeKey(key(name, { shiftKey }), kind), { type }, `${name} ${shiftKey} ${kind}`);
      }
      for (const flag of ["ctrlKey", "altKey", "metaKey"]) {
        assert.equal(decodeKey(key(name, { [flag]: true }), kind), null, `${name} ${flag} ${kind}`);
      }
    }
  }
});

test("unhandled keys pass through", () => {
  for (const name of ["Tab", "a", "ArrowRight2", "Delete", "F5"]) {
    assert.equal(decodeKey(key(name), "band"), null, name);
  }
});

test("a repeated plus gives no action", () => {
  assert.equal(decodeKey(key("+", { shiftKey: true, repeat: true }), "band"), null);
});

test("only the arrows and the Page keys follow auto-repeat", () => {
  for (const name of ["ArrowRight", "ArrowLeft", "PageUp", "PageDown"]) {
    assert.notEqual(decodeKey(key(name, { repeat: true }), "band"), null, name);
  }
  assert.notEqual(decodeKey(key("ArrowUp", { repeat: true }), "heatmap"), null);
  assert.notEqual(decodeKey(key("ArrowDown", { repeat: true }), "heatmap"), null);
  for (const name of ["Home", "End", "Enter", " ", "=", "-", "0", "Escape"]) {
    assert.equal(decodeKey(key(name, { repeat: true }), "band"), null, name);
  }
});

test("the large step is a tenth of the samples and never below one", () => {
  assert.equal(largeStep(0), 1);
  assert.equal(largeStep(1), 1);
  assert.equal(largeStep(5), 1);
  assert.equal(largeStep(14), 1);
  assert.equal(largeStep(15), 2);
  assert.equal(largeStep(100), 10);
  assert.equal(largeStep(1000), 100);
});

test("the cursor moves one sample and stays inside the list", () => {
  assert.equal(moveIndex(2, 5, 1, false), 3);
  assert.equal(moveIndex(2, 5, -1, false), 1);
  assert.equal(moveIndex(4, 5, 1, false), 4);
  assert.equal(moveIndex(0, 5, -1, false), 0);
});

test("a large step on five samples moves one", () => {
  assert.equal(moveIndex(1, 5, 1, true), 2);
  assert.equal(moveIndex(3, 5, -1, true), 2);
});

test("a large step on a hundred samples moves ten and stops at the ends", () => {
  assert.equal(moveIndex(50, 100, 1, true), 60);
  assert.equal(moveIndex(50, 100, -1, true), 40);
  assert.equal(moveIndex(95, 100, 1, true), 99);
  assert.equal(moveIndex(3, 100, -1, true), 0);
});

test("the first cursor lands on the newest sample, and an empty list has none", () => {
  assert.equal(moveIndex(null, 7, -1, false), 6);
  assert.equal(moveIndex(null, 7, 1, true), 6);
  assert.equal(moveIndex(null, 0, 1, false), null);
  assert.equal(moveIndex(3, 0, 1, false), null);
});

test("Home and End give the ends of the list", () => {
  assert.equal(edgeIndex(9, "oldest"), 0);
  assert.equal(edgeIndex(9, "newest"), 8);
  assert.equal(edgeIndex(0, "newest"), null);
});

test("the heatmap row moves one hop and stays inside the rows", () => {
  assert.equal(moveRow(2, 6, 1), 3);
  assert.equal(moveRow(2, 6, -1), 1);
  assert.equal(moveRow(5, 6, 1), 5);
  assert.equal(moveRow(0, 6, -1), 0);
  assert.equal(moveRow(null, 6, -1), 5);
  assert.equal(moveRow(null, 0, 1), null);
});

test("the nearest sample index handles ends, ties, duplicates, and an empty list", () => {
  assert.equal(nearestIndex([], 5), null);
  assert.equal(nearestIndex([10, 20, 30], 1), 0);
  assert.equal(nearestIndex([10, 20, 30], 99), 2);
  assert.equal(nearestIndex([10, 20, 30], 24), 1);
  assert.equal(nearestIndex([10, 20, 30], 26), 2);
  assert.equal(nearestIndex([10, 20, 30], 25), 1);
  assert.equal(nearestIndex([10, 20, 20, 30], 20), 1);
});

test("zoom in halves the window around the cursor in whole seconds", () => {
  assert.deepEqual(zoomIn({ from: 1000, to: 2000 }, 1500), { changed: true, window: { from: 1250, to: 1750 } });
  assert.deepEqual(zoomIn({ from: 1000, to: 1005 }, 1002), { changed: true, window: { from: 1001, to: 1003 } });
});

test("zoom in stays inside the current window at the first sample", () => {
  assert.deepEqual(zoomIn({ from: 1000, to: 2000 }, 1000), { changed: true, window: { from: 1000, to: 1500 } });
});

test("zoom in stays inside the current window at the last sample", () => {
  assert.deepEqual(zoomIn({ from: 1000, to: 2000 }, 2000), { changed: true, window: { from: 1500, to: 2000 } });
});

test("zoom in at a 3 s window gives a 1 s window", () => {
  const result = zoomIn({ from: 100, to: 103 }, 101);
  assert.deepEqual(result, { changed: true, window: { from: 101, to: 102 } });
});

test("zoom in at a 2 s window with the cursor at the center gives a 1 s window", () => {
  const result = zoomIn({ from: 100, to: 102 }, 101);
  assert.equal(result.changed, true);
  assert.equal(result.window.to - result.window.from, 1);
  assert.ok(result.window.from >= 100 && result.window.to <= 102);
  assert.ok(result.window.from <= 101 && 101 <= result.window.to);
});

test("zoom in at the 1 s floor gives no change", () => {
  assert.deepEqual(zoomIn({ from: 100, to: 101 }, 100), { changed: false });
});

test("zoom in gives whole seconds and to above from for every span and cursor", () => {
  for (let span = 2; span <= 40; span++) {
    for (let offset = 0; offset <= span; offset++) {
      const current = { from: 1000, to: 1000 + span };
      const result = zoomIn(current, 1000 + offset);
      assert.equal(result.changed, true, `span ${span} offset ${offset}`);
      const { from, to } = result.window;
      assert.ok(Number.isInteger(from) && Number.isInteger(to), `whole ${span} ${offset}`);
      assert.ok(to > from, `order ${span} ${offset}`);
      assert.ok(to - from < span, `smaller ${span} ${offset}`);
      assert.ok(from >= current.from && to <= current.to, `inside ${span} ${offset}`);
    }
  }
});

test("zoom in refuses a window that is not finite", () => {
  assert.deepEqual(zoomIn({ from: Number.NaN, to: 100 }, 50), { changed: false });
  assert.deepEqual(zoomIn({ from: 0, to: Number.POSITIVE_INFINITY }, 50), { changed: false });
});

const NOW = 1_800_000_000;
const HOUR = 3600;

test("zoom out on a chart that is not zoomed gives no change", () => {
  assert.deepEqual(zoomOut({ from: NOW - HOUR, to: NOW }, NOW - 10, HOUR, NOW), { changed: false });
});

test("zoom out doubles the window around the cursor", () => {
  const current = { from: NOW - 1000, to: NOW - 500 };
  assert.deepEqual(zoomOut(current, NOW - 750, HOUR, NOW), {
    changed: true,
    window: { from: NOW - 1250, to: NOW - 250 },
  });
});

test("zoom out resets when the doubled window passes the range", () => {
  const current = { from: NOW - 2000, to: NOW };
  assert.deepEqual(zoomOut(current, NOW - 1000, HOUR, NOW), { changed: true, window: null });
});

test("zoom out resets when the doubled window equals the range", () => {
  const current = { from: NOW - 1800, to: NOW };
  assert.deepEqual(zoomOut(current, NOW - 900, HOUR, NOW), { changed: true, window: null });
});

test("zoom out near now never passes now", () => {
  const current = { from: NOW - 300, to: NOW - 100 };
  const result = zoomOut(current, NOW - 100, HOUR, NOW);
  assert.deepEqual(result, { changed: true, window: { from: NOW - 400, to: NOW } });
});

test("zoom out holds an old window that lies far before now", () => {
  const t = NOW - 5 * HOUR;
  const current = { from: t - 3000, to: t - 2400 };
  const cursor = t - 2700;
  const result = zoomOut(current, cursor, HOUR, NOW);
  assert.deepEqual(result, { changed: true, window: { from: t - 3300, to: t - 2100 } });
});

test("zoom out from a window that ends at now stays before now", () => {
  const current = { from: NOW - 600, to: NOW };
  assert.deepEqual(zoomOut(current, NOW - 300, HOUR, NOW), { changed: true, window: { from: NOW - 1200, to: NOW } });
});

test("zoom out holds the old window and the cursor for every cursor second", () => {
  const starts = [NOW - 5 * HOUR, NOW - 2000, NOW - 700];
  for (const start of starts) {
    const current = { from: start, to: start + 600 };
    for (let cursor = current.from; cursor <= current.to; cursor += 25) {
      const result = zoomOut(current, cursor, HOUR, NOW);
      assert.equal(result.changed, true);
      const { from, to } = result.window;
      assert.equal(to - from, 1200);
      assert.ok(from <= current.from && to >= current.to, `holds ${start} ${cursor}`);
      assert.ok(from <= cursor && cursor <= to);
      assert.ok(to <= NOW);
      assert.ok(Number.isInteger(from));
    }
  }
});

test("zoom out holds the old window when the cursor lies after its end", () => {
  const current = { from: NOW - 3000, to: NOW - 2400 };
  assert.deepEqual(zoomOut(current, NOW - 1000, HOUR, NOW), { changed: true, window: { from: NOW - 3000, to: NOW - 1800 } });
});

test("zoom out with the cursor at the first and the last second of the old window", () => {
  const current = { from: NOW - 3000, to: NOW - 2400 };
  assert.deepEqual(zoomOut(current, current.from, HOUR, NOW), { changed: true, window: { from: NOW - 3600, to: NOW - 2400 } });
  assert.deepEqual(zoomOut(current, current.to, HOUR, NOW), { changed: true, window: { from: NOW - 3000, to: NOW - 1800 } });
});

test("zoom out refuses a range span that is not positive", () => {
  assert.deepEqual(zoomOut({ from: 0, to: 10 }, 5, 0, NOW), { changed: false });
});

test("reset clears a zoomed window and ignores an unzoomed one", () => {
  assert.deepEqual(zoomReset({ from: NOW - 600, to: NOW }, HOUR), { changed: true, window: null });
  assert.deepEqual(zoomReset({ from: NOW - HOUR, to: NOW }, HOUR), { changed: false });
});

test("isZoomed compares the window with the range span", () => {
  assert.equal(isZoomed({ from: 0, to: 59 }, 60), true);
  assert.equal(isZoomed({ from: 0, to: 60 }, 60), false);
});

test("a keydown announces once and a repeat stays silent", () => {
  assert.equal(shouldAnnounce("down", false, false), true);
  assert.equal(shouldAnnounce("down", true, true), false);
  assert.equal(shouldAnnounce("down", true, false), false);
});

test("a keyup announces the final value only after repeats", () => {
  assert.equal(shouldAnnounce("up", false, true), true);
  assert.equal(shouldAnnounce("up", false, false), false);
});

test("the window text holds both ends in the given zone", () => {
  assert.equal(
    windowText(1_700_000_000, 1_700_003_600, FORMAT),
    `${localeString(1_700_000_000)} to ${localeString(1_700_003_600)}`,
  );
});

test("the window text names an unknown window", () => {
  assert.equal(windowText(undefined, 10, FORMAT), "window not loaded");
});

test("the latency name holds the target, the window, and the summary", () => {
  const summary = latencySummaryText(
    { cycles: 120, sources: 3, median: 10.24, p95: 15.5, loss: 0.25 },
    false,
  );
  const name = chartName({
    subject: "Latency band chart",
    target: "core/frankfurt",
    fromSec: 1_700_000_000,
    toSec: 1_700_003_600,
    summary,
    format: FORMAT,
  });
  assert.match(name, /^Latency band chart for core\/frankfurt, /);
  assert.ok(name.includes(windowText(1_700_000_000, 1_700_003_600, FORMAT)));
  assert.ok(name.endsWith(": 120 cycles from 3 sources, average median 10.2 ms, average p95 15.5 ms, loss 0.3%"));
});

test("the name builder puts the qualifier before the window", () => {
  const name = chartName({
    subject: "MTR per-hop loss heatmap",
    target: "core/frankfurt",
    qualifier: "source slave-1",
    fromSec: 1_700_000_000,
    toSec: 1_700_003_600,
    summary: "x",
    format: FORMAT,
  });
  assert.ok(name.startsWith(`MTR per-hop loss heatmap for core/frankfurt, source slave-1, ${localeString(1_700_000_000)} to `));
});

test("the summaries read loading and no data", () => {
  assert.equal(latencySummaryText(null, true), "loading");
  assert.equal(latencySummaryText(null, false), "no data in range");
  assert.equal(httpSummaryText(null, true), "loading");
  assert.equal(httpSummaryText(null, false), "no data in range");
  assert.equal(heatmapSummaryText(null, true), "loading");
  assert.equal(lossSummaryText(null, false), "no data in range");
  assert.equal(statusSummaryText(null, true), "loading");
});

test("the latency summary uses singular nouns and marks a missing value", () => {
  assert.equal(
    latencySummaryText({ cycles: 1, sources: 1, median: null, p95: null, loss: null }, false),
    "1 cycle from 1 source, average median none, average p95 none, loss none",
  );
});

test("the HTTP summary holds the sample count, uptime, and TTFB", () => {
  assert.equal(
    httpSummaryText({ samples: 40, uptime: 97.5, p50: 120.04, p95: 300 }, false),
    "40 samples, uptime 97.5%, TTFB p50 120.0 ms, p95 300.0 ms",
  );
});

test("the heatmap summary names the worst replying hop", () => {
  assert.equal(
    heatmapSummaryText({ hops: 9, columns: 48, worstLoss: 12.5, worstHop: 4, silentHops: 0, targetLoss: null }, false),
    "9 hops, 48 columns, worst hop loss 12.5% at hop 4",
  );
  assert.equal(
    heatmapSummaryText({ hops: 9, columns: 48, worstLoss: 0, worstHop: null, silentHops: 0, targetLoss: null }, false),
    "9 hops, 48 columns, no hop loss",
  );
});

function cell(hop, replies, lossPct, targetLossPct) {
  return { hop, replies, lossPct, targetLossPct };
}

function heatmapText(cells, columns) {
  return heatmapSummaryText(summarizeHeatmap(cells, columns), false);
}

test("a path whose hops all reply with no loss has no hop loss", () => {
  assert.equal(heatmapText([cell(1, 10, 0), cell(2, 10, 0), cell(3, 10, 0)], 10), "3 hops, 10 columns, no hop loss");
});

test("a silent hop is no reply and not loss", () => {
  assert.equal(
    heatmapText([cell(1, 10, 0), cell(2, 0, 100), cell(3, 10, 0)], 10),
    "3 hops, 10 columns, no hop loss, 1 hop gave no reply",
  );
});

test("a replying hop with loss names that hop", () => {
  assert.equal(
    heatmapText([cell(1, 10, 0), cell(2, 9, 12.5), cell(3, 10, 0)], 10),
    "3 hops, 10 columns, worst hop loss 12.5% at hop 2",
  );
});

test("a silent hop and a replying hop with loss are stated apart", () => {
  assert.equal(
    heatmapText([cell(1, 0, 100), cell(2, 0, 100), cell(3, 9, 12.5)], 10),
    "3 hops, 10 columns, worst hop loss 12.5% at hop 3, 2 hops gave no reply",
  );
});

test("a hop that replied in one column is not silent", () => {
  assert.equal(
    heatmapText([cell(1, 0, 100), cell(1, 10, 0)], 2),
    "1 hop, 2 columns, no hop loss",
  );
});

test("a target loss above zero is stated and the target cell is no hop loss", () => {
  assert.equal(
    heatmapText([cell(1, 10, 0), cell(2, 0, 100, 40)], 5),
    "2 hops, 5 columns, no hop loss, worst target loss 40.0%",
  );
  assert.equal(
    heatmapText([cell(1, 10, 0), cell(2, 10, 0, 0)], 5),
    "2 hops, 5 columns, no hop loss",
  );
});

test("a heatmap with no cell has no summary", () => {
  assert.equal(summarizeHeatmap([], 0), null);
  assert.equal(heatmapText([], 0), "no data in range");
});

test("a cell replied when its reply count is above zero", () => {
  assert.equal(replyCountOf({ ReplyCount: 7, Sent: 10, LossCount: 9 }), 7);
  assert.equal(replyCountOf({ Sent: 10, LossCount: 4 }), 6);
  assert.equal(replyCountOf({ Sent: 3, LossCount: 5 }), 0);
  assert.equal(cellReplied(1), true);
  assert.equal(cellReplied(0), false);
});

test("the hop address text reads no reply for a silent cell", () => {
  assert.equal(hopAddressText("192.0.2.1", 5), "192.0.2.1");
  assert.equal(hopAddressText("192.0.2.1", 0), "no reply");
  assert.equal(hopAddressText("", 5), "no reply");
});

test("the heatmap tooltip keeps its side away from an edge and stays inside the wrapper", () => {
  assert.equal(heatmapTooltipLeft(100, 200, 1300), 112);
  assert.equal(heatmapTooltipLeft(600, 200, 1300), 388);
  for (const container of [343, 1300]) {
    for (const x of [28, 100, 220, 221, 300, container - 4]) {
      const left = heatmapTooltipLeft(x, 250, container);
      assert.ok(left >= 0 && left + 250 <= container, `${x} ${container}`);
    }
  }
  assert.equal(heatmapTooltipLeft(40, 250, 343), 52);
  assert.equal(heatmapTooltipLeft(339, 250, 343), 77);
});

test("the chart name stays at its focus-time value while the chart has focus", () => {
  assert.equal(stableName(null, "live"), "live");
  assert.equal(stableName("at focus", "live"), "at focus");
});

test("an entry with no value at all reads no sample", () => {
  assert.equal(
    latencyReadoutText("T", [{ source: "a", median: null, p95: null, loss: null }]),
    "T: a no sample",
  );
  assert.equal(
    latencyReadoutText("T", [{ source: "", median: null, p95: null, loss: null }]),
    "T: no sample",
  );
  assert.equal(
    latencyReadoutText("T", [{ source: "a", median: null, p95: null, loss: 100 }]),
    "T: a median none, p95 none, loss 100.0%",
  );
});

test("the HTTP error text is cut at 80 characters", () => {
  const text = httpReadoutText({ timeText: "T", status: 502, rtt: 1, source: "", error: "x".repeat(100) });
  assert.ok(text.endsWith(`error ${"x".repeat(80)}`));
  assert.ok(!text.includes("x".repeat(81)));
});

test("the loss summary names the worst cycle", () => {
  assert.equal(
    lossSummaryText({ lossy: 3, cycles: 60, worstLoss: 40, worstSec: 1_700_000_000 }, false, FORMAT),
    `3 of 60 cycles lost packets, worst 40.0% at ${localeString(1_700_000_000)}`,
  );
});

test("the status summary counts each class", () => {
  assert.equal(
    statusSummaryText({ "2xx": 90, "3xx": 2, "4xx": 3, "5xx": 4, err: 1 }, false),
    "90 2xx, 2 3xx, 3 4xx, 4 5xx, 1 network errors",
  );
});

test("the readouts hold the same values as the visible cursor text", () => {
  assert.equal(
    latencyReadoutText("T", [
      { source: "a", median: 10, p95: 12, loss: 0 },
      { source: "b", median: null, p95: null, loss: 100 },
    ]),
    "T: a median 10.0 ms, p95 12.0 ms, loss 0.0%; b median none, p95 none, loss 100.0%",
  );
  assert.equal(
    latencyReadoutText("T", [{ source: "", median: 10, p95: 12, loss: 0 }]),
    "T: median 10.0 ms, p95 12.0 ms, loss 0.0%",
  );
  assert.equal(
    httpReadoutText({ timeText: "T", status: 503, rtt: 12.34, source: "a", error: "boom" }),
    "T: status 503, 12.3 ms, source a, error boom",
  );
  assert.equal(
    httpReadoutText({ timeText: "T", status: 0, rtt: 0, source: "" }),
    "T: status network error",
  );
  assert.equal(
    lossReadoutText("T", [
      { source: "a", loss: 5 },
      { source: "b", loss: 100 },
    ]),
    "T: a loss 5.0%; b loss 100.0%",
  );
  assert.equal(
    heatmapReadoutText({
      hop: 3,
      timeText: "T",
      hostname: "edge.example.net",
      ip: "192.0.2.1",
      lossPct: 12.5,
      worstOfBucket: true,
      replies: 8,
      sent: 10,
      targetLoss: 2,
      unreach: "net",
    }),
    "hop 3, T: edge.example.net, 192.0.2.1, loss 12.5% worst cycle, replies 8 of 10, target loss 2.0%, trace ended: net unreachable",
  );
  assert.equal(
    heatmapReadoutText({
      hop: 4,
      timeText: "T",
      ip: "",
      lossPct: 100,
      worstOfBucket: false,
      replies: 0,
      sent: 10,
    }),
    "hop 4, T: no reply, loss 100.0%, replies 0 of 10",
  );
});

test("an empty heatmap cell reads as no data", () => {
  assert.equal(heatmapNoDataText(5, "T"), "hop 5, T: no data");
});

test("the pick and zoom announcements use the visible window text", () => {
  assert.equal(pickAnnouncement("T"), "Cycle at T picked");
  assert.equal(zoomAnnouncement({ from: 1_700_000_000, to: 1_700_000_060 }, FORMAT), `Zoom ${windowText(1_700_000_000, 1_700_000_060, FORMAT)}`);
  assert.equal(zoomAnnouncement(null, FORMAT), "Zoom reset");
});

test("each key help lists only the keys of its chart", () => {
  const text = (kind) => keyHelpText(kind);
  assert.ok(text("band").includes("Enter"));
  assert.ok(text("band").includes("zoom"));
  assert.ok(!text("band").includes("Up, Down"));
  assert.ok(!text("http").includes("Enter"));
  assert.ok(text("http").includes("zoom"));
  assert.ok(text("heatmap").includes("Up, Down"));
  assert.ok(text("heatmap").includes("Enter"));
  assert.ok(!text("heatmap").includes("zoom"));
  assert.ok(!text("loss").includes("zoom"));
  assert.ok(!text("loss").includes("Up, Down"));
  assert.ok(text("loss").includes("Enter"));
  for (const kind of ALL_KINDS) {
    assert.ok(text(kind).includes("Escape"));
    assert.ok(!/[—·]/.test(text(kind)), `${kind} has an em dash or a middle dot`);
    assert.ok(keyHelp(kind).length > 0);
  }
});

test("Space with mouse focus and no cursor gives no action", () => {
  for (const kind of ALL_KINDS) {
    assert.equal(decodeKeyInMode(key(" "), kind, false), null);
    assert.equal(decodeKeyInMode(key(" ", { repeat: true }), kind, false), null);
    assert.equal(decodeKeyInMode(key("Enter"), kind, false), null);
  }
});

test("with mouse focus only the cursor arrows act", () => {
  for (const kind of ALL_KINDS) {
    assert.deepEqual(decodeKeyInMode(key("ArrowRight"), kind, false), { type: "step", direction: 1, large: false });
    assert.deepEqual(decodeKeyInMode(key("ArrowLeft"), kind, false), { type: "step", direction: -1, large: false });
    assert.deepEqual(decodeKeyInMode(key("ArrowLeft", { shiftKey: true }), kind, false), { type: "step", direction: -1, large: true });
    for (const name of ["PageUp", "PageDown", "Home", "End", "Escape", "+", "=", "-", "0"]) {
      assert.equal(decodeKeyInMode(key(name), kind, false), null, `${name} ${kind}`);
    }
  }
  for (const kind of ALL_KINDS) {
    assert.equal(decodeKeyInMode(key("ArrowUp"), kind, false), null, `ArrowUp ${kind}`);
    assert.equal(decodeKeyInMode(key("ArrowDown"), kind, false), null, `ArrowDown ${kind}`);
  }
});

test("with keyboard mode every key of the map acts as before", () => {
  for (const kind of ALL_KINDS) {
    for (const name of ["PageUp", "Home", "End", "Escape", "ArrowRight", " ", "Enter", "+", "-", "0", "ArrowUp"]) {
      assert.deepEqual(decodeKeyInMode(key(name), kind, true), decodeKey(key(name), kind), `${name} ${kind}`);
    }
  }
});

test("mouse focus still passes modifier keys to the browser", () => {
  assert.equal(decodeKeyInMode(key("ArrowRight", { ctrlKey: true }), "band", false), null);
});

test("each announcement changes the sequence, also for equal text", () => {
  const first = nextAnnouncement({ text: "", seq: 0 }, "T: a");
  const second = nextAnnouncement(first, "T: a");
  assert.equal(second.text, first.text);
  assert.notEqual(second.seq, first.seq);
  const cleared = nextAnnouncement(second, "");
  assert.equal(cleared.text, "");
  assert.notEqual(cleared.seq, second.seq);
});

test("the zoom clock is the larger of the client clock and the echoed end", () => {
  assert.equal(resolveZoomNow(1000.4, 1120), 1120);
  assert.equal(resolveZoomNow(1120, 1120), 1120);
  assert.equal(resolveZoomNow(1200.9, 1120), 1200);
  assert.equal(resolveZoomNow(1000.4, undefined), 1000);
});

test("zoom out with a client clock behind the server keeps the cursor in the window", () => {
  const serverNow = 1_800_000_000;
  const clientNow = serverNow - 120;
  const current = { from: serverNow - 60, to: serverNow };
  const cursor = serverNow - 5;
  const result = zoomOut(current, cursor, HOUR, resolveZoomNow(clientNow, current.to));
  assert.equal(result.changed, true);
  assert.ok(result.window.from <= cursor && cursor <= result.window.to);
});

test("the local time text is the locale string of the instant", () => {
  assert.equal(formatLocalTime(1_700_000_000), new Date(1_700_000_000_000).toLocaleString());
  assert.equal(formatLocalTime(1_700_000_000, FORMAT), localeString(1_700_000_000));
});

test("every time text in a summary, a name, and an announcement comes from one formatter", () => {
  const sec = 1_700_000_000;
  const expected = formatLocalTime(sec, FORMAT);
  assert.ok(windowText(sec, sec + 60, FORMAT).startsWith(`${expected} to `));
  assert.ok(lossSummaryText({ lossy: 1, cycles: 1, worstLoss: 5, worstSec: sec }, false, FORMAT).endsWith(` at ${expected}`));
  assert.ok(zoomAnnouncement({ from: sec, to: sec + 60 }, FORMAT).startsWith(`Zoom ${expected} to `));
});

test("keyboard mode needs a cursor or keyboard focus", () => {
  assert.equal(isKeyboardMode(false, false), false);
  assert.equal(isKeyboardMode(true, false), true);
  assert.equal(isKeyboardMode(false, true), true);
  assert.equal(isKeyboardMode(true, true), true);
});

test("a box stays inside the container at the first, the middle, and the last sample", () => {
  for (const host of [343, 1300]) {
    const box = 200;
    const first = clampBoxStart(0 - box / 2, box, host, "start");
    const middle = clampBoxStart(host / 2 - box / 2, box, host, "start");
    const last = clampBoxStart(host - box / 2, box, host, "start");
    assert.equal(first, 0);
    assert.equal(middle, host / 2 - box / 2);
    assert.equal(last, host - box);
    for (const start of [first, middle, last]) {
      assert.ok(start >= 0 && start + box <= host);
    }
  }
});

test("a box that is not near an edge keeps its position", () => {
  assert.equal(clampBoxStart(120, 100, 1000, "start"), 120);
  assert.equal(clampBoxStart(120, 100, 1000, "end"), 120);
});

test("a box wider than the container starts at the start or ends at the end", () => {
  assert.equal(clampBoxStart(30, 400, 343, "start"), 0);
  assert.equal(clampBoxStart(30, 120, 80, "end"), -40);
  assert.equal(clampBoxStart(-500, 120, 80, "end"), -40);
});

test("the keyboard cursor placement hides the horizontal line", () => {
  const placement = keyboardCursorPlacement(123);
  assert.equal(placement.left, 123);
  assert.ok(placement.top < 0);
});

test("a chart focused with loaded data holds the name at focus", () => {
  assert.equal(nextHeldName(null, true, "A", false), "A");
});

test("a chart focused during a load holds no name until the data arrives", () => {
  assert.equal(nextHeldName(null, true, "window not loaded: loading", true), null);
  assert.equal(nextHeldName(null, true, "loaded name", false), "loaded name");
});

test("a refresh under focus keeps the held name, also when the load state returns", () => {
  assert.equal(nextHeldName("held", true, "other", false), "held");
  assert.equal(nextHeldName("held", true, "loading", true), "held");
});

test("blur releases the held name", () => {
  assert.equal(nextHeldName("held", false, "other", false), null);
  assert.equal(nextHeldName(null, false, "other", true), null);
});

test("a chart with no data in range holds its name at focus", () => {
  assert.equal(nextHeldName(null, true, "no data in range", false), "no data in range");
});

test("the main scroller is the containing block of the hidden chart text", () => {
  const css = readFileSync(new URL("../src/styles.css", import.meta.url), "utf8");
  const block = css.match(/^\.main\s*\{([^}]*)\}/m);
  assert.ok(block, ".main rule exists");
  assert.match(block[1], /position:\s*relative/);
  const hidden = css.match(/^\.sr-only\s*\{([^}]*)\}/m);
  assert.ok(hidden, ".sr-only rule exists");
  assert.match(hidden[1], /position:\s*absolute/);
});
