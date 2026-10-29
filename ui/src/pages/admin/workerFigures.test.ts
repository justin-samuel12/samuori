import assert from "node:assert/strict";
import { test } from "node:test";
import type { Schemas } from "../../api/client";
import { figuresText, readinessText, workerFigures } from "./workerFigures.ts";

type Worker = Schemas["Worker"];

const worker = (over: Partial<Worker>): Worker =>
  ({
    id: "w",
    name: "w",
    poolId: "p",
    enrollmentState: "enrolled",
    operationalState: "active",
    connected: true,
    eligibility: { untrustedBuilds: true, reasons: [] },
    selfTest: { status: "passed", current: true },
    ...over,
  }) as Worker;

test("an eligible worker without a live stream is not ready", () => {
  const f = workerFigures([
    worker({ id: "a" }),
    worker({ id: "b", connected: false }),
    worker({
      id: "c",
      poolId: undefined,
      connected: true,
      eligibility: { untrustedBuilds: false, reasons: ["unassigned"] },
    }),
    worker({ id: "d", enrollmentState: "revoked", connected: false }),
    worker({
      id: "e",
      removal: { state: "draining", reason: "old", requestedAt: "" },
    }),
  ]);
  assert.deepEqual(f, { enrolled: 3, inPool: 2, connected: 2, ready: 1 });
  assert.equal(
    figuresText(f),
    "3 enrolled · 2 in a pool · 2 connected now · 1 ready to build",
  );
});

test("readiness names the missing connection before eligibility reasons", () => {
  assert.equal(readinessText(worker({}), {}), "Yes");
  assert.equal(
    readinessText(worker({ connected: false }), {}),
    "No: not connected now",
  );
  assert.equal(
    readinessText(
      worker({
        connected: false,
        eligibility: { untrustedBuilds: false, reasons: ["cordoned"] },
      }),
      { cordoned: "cordoned" },
    ),
    "No: not connected now; cordoned",
  );
});
