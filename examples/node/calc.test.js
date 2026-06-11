import test from "node:test";
import assert from "node:assert";
import { add } from "./calc.js";

test("add", () => {
  assert.strictEqual(add(2, 3), 5);
});
