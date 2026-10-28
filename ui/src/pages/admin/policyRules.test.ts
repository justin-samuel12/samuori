import assert from "node:assert/strict";
import { test } from "node:test";
import {
  frontendProblem,
  put,
  ruleIdProblem,
  sensitivePathProblem,
  without,
} from "./policyRules.ts";

const digest = "a".repeat(64);

test("frontends must be pinned by a full sha256 digest", () => {
  assert.equal(
    frontendProblem(`docker.io/docker/dockerfile@sha256:${digest}`),
    undefined,
  );
  assert.ok(frontendProblem("docker/dockerfile:1"));
  assert.ok(frontendProblem(`docker/dockerfile@sha256:${digest.slice(1)}`));
  assert.ok(frontendProblem(`Docker/Dockerfile@sha256:${digest}`));
  assert.ok(frontendProblem(""));
});

test("rule IDs are slugs and unique", () => {
  assert.equal(ruleIdProblem("no-proxy", []), undefined);
  assert.ok(ruleIdProblem("No Proxy", []));
  assert.ok(ruleIdProblem("no-proxy", ["no-proxy"]));
});

test("sensitive path rules stay inside the image", () => {
  assert.equal(sensitivePathProblem("etc/app/*.key"), undefined);
  assert.ok(sensitivePathProblem("/etc/shadow"));
  assert.ok(sensitivePathProblem("app/../../etc"));
});

test("list edits leave the original untouched", () => {
  const list = ["a", "b"];
  assert.deepEqual(put(list, 2, "c"), ["a", "b", "c"]);
  assert.deepEqual(put(list, 0, "z"), ["z", "b"]);
  assert.deepEqual(without(list, 0), ["b"]);
  assert.deepEqual(list, ["a", "b"]);
});
