import type { Schemas } from "../../api/client";

type Worker = Schemas["Worker"];

/** The one set of worker figures every administration page shows (docs/operators/worker-identity.md#worker-status). */
export interface WorkerFigures {
  enrolled: number;
  inPool: number;
  connected: number;
  ready: number;
}

export const figureLabels: Record<keyof WorkerFigures, string> = {
  enrolled: "enrolled",
  inPool: "in a pool",
  connected: "connected now",
  ready: "ready to build",
};

export const figureMeaning: Record<keyof WorkerFigures, string> = {
  enrolled: "Holds a current worker certificate and is not being removed.",
  inPool: "Enrolled and assigned to a worker pool.",
  connected: "Enrolled and holding a live stream to a running controller.",
  ready:
    "Connected and eligible for uploaded source: an enabled pool, a current self-test and strict egress.",
};

function isEnrolled(w: Worker): boolean {
  return w.enrollmentState === "enrolled" && !w.removal;
}

export function isReady(w: Worker): boolean {
  return isEnrolled(w) && w.connected && w.eligibility.untrustedBuilds;
}

export function workerFigures(list: readonly Worker[]): WorkerFigures {
  const enrolled = list.filter(isEnrolled);
  return {
    enrolled: enrolled.length,
    inPool: enrolled.filter((w) => !!w.poolId).length,
    connected: enrolled.filter((w) => w.connected).length,
    ready: enrolled.filter(isReady).length,
  };
}

export function figuresText(f: WorkerFigures): string {
  return (Object.keys(figureLabels) as (keyof WorkerFigures)[])
    .map((k) => `${f[k]} ${figureLabels[k]}`)
    .join(" · ");
}

/** One worker's answer to "can it build uploaded source now?", with eligibility reasons in plain words. */
export function readinessText(
  w: Worker,
  reasonText: Record<string, string>,
): string {
  if (isReady(w)) return "Yes";
  const why = w.eligibility.reasons.map((r) => reasonText[r] ?? r);
  if (!w.connected && isEnrolled(w)) why.unshift("not connected now");
  return why.length ? `No: ${why.join("; ")}` : "No";
}
