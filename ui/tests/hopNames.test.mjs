import assert from "node:assert/strict";
import test from "node:test";

import { hopHostname } from "../src/hopNames.ts";

test("returns the PTR name keyed by the exact served IP", () => {
  assert.equal(
    hopHostname({ "192.0.2.1": "edge.example.net" }, "192.0.2.1"),
    "edge.example.net",
  );
});

test("falls back when an older server omits hostnames", () => {
  assert.equal(hopHostname(undefined, "192.0.2.1"), undefined);
});

test("does not match a different textual IP spelling", () => {
  assert.equal(
    hopHostname({ "::ffff:192.0.2.1": "edge.example.net" }, "192.0.2.1"),
    undefined,
  );
});
