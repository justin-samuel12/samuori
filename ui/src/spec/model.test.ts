import assert from "node:assert/strict";
import { test } from "node:test";
import {
  comboKey,
  formPaths,
  fromFinding,
  frontendBackendPairs,
  get,
  migrateDraft,
  set,
  unrepresented,
  withFrontend,
} from "./model.ts";

test("drafts saved in Guided or Detailed mode open in the form", () => {
  for (const mode of ["guided", "detailed", undefined]) {
    const d = migrateDraft({
      step: 2,
      mode,
      doc: { metadata: { name: "api" } },
      provenance: { "metadata.name": "guided", "spec.build.args": "detailed" },
    });
    assert.equal(d.mode, "form");
    assert.equal(d.step, 2);
    assert.deepEqual(d.doc, { metadata: { name: "api" } });
    assert.deepEqual(d.provenance, {
      "metadata.name": "form",
      "spec.build.args": "form",
    });
  }
});

test("YAML drafts stay in YAML and keep YAML provenance", () => {
  const d = migrateDraft({ mode: "yaml", provenance: { "x-a": "yaml" } });
  assert.equal(d.mode, "yaml");
  assert.deepEqual(d.provenance, { "x-a": "yaml" });
  assert.deepEqual(migrateDraft({ mode: "yaml" }).provenance, {});
});

test("the form edits every field it shows, and lists only YAML-only fields", () => {
  let doc = set({}, ["metadata", "description"], "Orders API");
  doc = set(
    doc,
    ["spec", "build", "recipe", "systemPackages", "build"],
    ["git"],
  );
  doc = set(doc, ["spec", "outputs", "image", "labels"], { team: "a" });
  doc = set(doc, ["spec", "build", "args"], [{ name: "A", value: "1" }]);
  doc = set(doc, ["spec", "target", "platforms"], ["linux/arm64"]);
  doc = set(doc, ["x-team"], "a");
  assert.deepEqual(unrepresented(doc, formPaths), ["x-team"]);
});

test("removing the last system package removes systemPackages", () => {
  const path = ["spec", "build", "recipe", "systemPackages", "runtime"];
  const doc = set({ spec: { build: { recipe: { id: "r" } } } }, path, ["curl"]);
  assert.deepEqual(set(doc, path, undefined), {
    spec: { build: { recipe: { id: "r" } } },
  });
});

test("a frontend and a backend finding make one launcher image", () => {
  const be = {
    detectorId: "python",
    detectorVersion: "1",
    sourceSubpath: "backend",
    language: "python",
    applicationKind: "http-service" as const,
    suggestedCommands: { start: "uvicorn app.main:app --port 8000" },
    evidence: [],
    confidence: "high" as const,
  };
  const fe = {
    ...be,
    detectorId: "node",
    sourceSubpath: "frontend",
    language: "typescript",
    applicationKind: "static-site" as const,
    suggestedCommands: { install: "npm ci", build: "npm run build" },
  };
  const doc = withFrontend(fromFinding("audit", be, undefined), fe);
  assert.equal(get(doc, ["spec", "build", "recipe", "path"]), "backend");
  assert.deepEqual(get(doc, ["spec", "build", "recipe", "frontend"]), {
    path: "frontend",
    install: "npm ci",
    build: "npm run build",
    apiPrefixes: ["/api"],
    backendPort: 8000,
  });
  assert.equal(get(doc, ["spec", "application", "port"]), 8080);
  assert.equal(get(doc, ["spec", "application", "type"]), "http-service");
  assert.deepEqual(unrepresented(doc, formPaths), []);
  const pairs = frontendBackendPairs([be, fe, { ...fe, sourceSubpath: "." }]);
  assert.deepEqual(
    pairs.map((p) => comboKey(p.fe, p.be)),
    ["frontend+backend:node:frontend|python:backend"],
  );
});
