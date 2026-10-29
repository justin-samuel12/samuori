import assert from "node:assert/strict";
import { test } from "node:test";
import {
  attemptState,
  checkOutcome,
  cleanupState,
  imageName,
  isolationText,
  plural,
  selfTestState,
  stepStatus,
  workerState,
} from "./display.ts";

const digest = "sha256:0123456789abcdef0123";

test("images are named by name and version, falling back to the short digest", () => {
  assert.equal(
    imageName({ name: "shop", version: "2026-10-29.1", digest }),
    "shop · 2026-10-29.1",
  );
  assert.equal(imageName({ name: "shop", digest }), "shop · 0123456789ab");
  assert.equal(imageName({ version: "2026-10-29.1", digest }), "2026-10-29.1");
  assert.equal(imageName({ name: " ", digest }), "0123456789ab");
  assert.equal(imageName({}), "Image");
});

test("raw states read as plain labels with a tone", () => {
  assert.deepEqual(attemptState("completed"), {
    text: "Completed",
    tone: "ok",
  });
  assert.equal(cleanupState("destroyed").text, "Cleaned up");
  assert.equal(cleanupState(undefined).text, "Not cleaned up yet");
  assert.equal(cleanupState("failed-quarantined").tone, "bad");
  assert.equal(
    isolationText("rootless-dedicated-vm"),
    "Rootless, dedicated VM",
  );
  assert.equal(isolationText("new-profile"), "new-profile");
  assert.equal(attemptState("something-new").text, "something-new");
});

test("a skipped check is never shown as success", () => {
  assert.notEqual(checkOutcome("skipped").tone, "ok");
  assert.equal(checkOutcome("passed").tone, "ok");
});

test("wizard steps show done, current, not reached and problems", () => {
  assert.equal(stepStatus(2, 2, 4, true), "current");
  assert.equal(stepStatus(1, 2, 4, false), "done");
  assert.equal(stepStatus(3, 2, 4, true), "problem");
  assert.equal(stepStatus(5, 2, 4, false), "todo");
  assert.equal(stepStatus(5, 2, 4, true), "todo");
});

test("worker states read as one plain label; a lost enrollment wins", () => {
  assert.deepEqual(
    workerState({ enrollmentState: "enrolled", operationalState: "active" }),
    { text: "Active", tone: "ok" },
  );
  assert.equal(
    workerState({ enrollmentState: "revoked", operationalState: "active" })
      .text,
    "Revoked",
  );
  assert.equal(
    selfTestState({ status: "passed", current: false }).tone,
    "warn",
  );
  assert.equal(selfTestState({ status: "never-run" }).text, "Not run yet");
  assert.equal(
    selfTestState({ status: "failed", running: true }).text,
    "Running",
  );
});

test("counts use the right plural", () => {
  assert.equal(plural(1, "worker"), "1 worker");
  assert.equal(plural(2, "worker"), "2 workers");
  assert.equal(plural(0, "file"), "0 files");
});
