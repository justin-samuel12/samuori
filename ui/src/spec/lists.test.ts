import assert from "node:assert/strict";
import { test } from "node:test";
import {
  commandLines,
  commandValue,
  listLines,
  listValue,
  move,
} from "./lists.ts";

test("one command line is saved as that string", () => {
  assert.equal(commandValue(["npm ci"]), "npm ci");
  assert.deepEqual(commandLines("npm ci"), ["npm ci"]);
});

test("several command lines run in order and stop at the first failure", () => {
  const v = commandValue(["npm ci", "  npm run build ", "", "npm test"]);
  assert.equal(v, "npm ci && npm run build && npm test");
  assert.deepEqual(commandLines(v), ["npm ci", "npm run build", "npm test"]);
});

test("a command string survives the form unchanged", () => {
  for (const s of [
    "npm ci --omit=dev && echo done",
    'sh -c "a && b"',
    "make&&make install",
  ])
    assert.equal(commandValue(commandLines(s) ?? []), s);
});

test("empty commands are removed from the spec", () => {
  assert.equal(commandValue([]), undefined);
  assert.equal(commandValue(["", "  "]), undefined);
  assert.deepEqual(commandLines(undefined), []);
});

test("argument vectors and multi-line scripts are left to the YAML view", () => {
  assert.equal(commandLines(["node", "server.js"]), null);
  assert.equal(commandLines("set -e\nnpm ci"), null);
});

test("lists keep their entries and drop empty ones", () => {
  assert.deepEqual(listValue(["git", " ", "python3 "]), ["git", "python3"]);
  assert.equal(listValue([""]), undefined);
  assert.deepEqual(listLines(["git"]), ["git"]);
  assert.deepEqual(listLines(undefined), []);
  assert.equal(listLines("git"), null);
});

test("move reorders within bounds only", () => {
  assert.deepEqual(move(["a", "b", "c"], 0, 1), ["b", "a", "c"]);
  assert.deepEqual(move(["a", "b", "c"], 2, -1), ["a", "c", "b"]);
  assert.deepEqual(move(["a", "b"], 0, -1), ["a", "b"]);
  assert.deepEqual(move(["a", "b"], 1, 1), ["a", "b"]);
});
