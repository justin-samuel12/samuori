import { shortDigest } from "../format.ts";

export type Tone = "ok" | "warn" | "bad" | "info" | "neutral";

export interface Label {
  text: string;
  tone: Tone;
}

/** How people name an image: its app or project and build version, falling back to the short digest. */
export function imageName(i: {
  name?: string;
  version?: string;
  digest?: string;
}): string {
  const name = i.name?.trim();
  if (name && i.version) return `${name} · ${i.version}`;
  if (i.version) return i.version;
  if (i.digest)
    return name ? `${name} · ${shortDigest(i.digest)}` : shortDigest(i.digest);
  return name || "Image";
}

const known = (map: Record<string, Label>, value: string | undefined): Label =>
  (value && map[value]) || { text: value ?? "Unknown", tone: "neutral" };

const attemptStates: Record<string, Label> = {
  leased: { text: "Assigned", tone: "info" },
  preparing: { text: "Preparing", tone: "info" },
  building: { text: "Building", tone: "info" },
  committing: { text: "Saving the image", tone: "info" },
  completed: { text: "Completed", tone: "ok" },
  failed: { text: "Failed", tone: "bad" },
  abandoned: { text: "Abandoned", tone: "warn" },
  superseded: { text: "Replaced by a retry", tone: "neutral" },
};

export const attemptState = (s?: string) => known(attemptStates, s);

const cleanups: Record<string, Label> = {
  pending: { text: "Not cleaned up yet", tone: "info" },
  destroyed: { text: "Cleaned up", tone: "ok" },
  "failed-quarantined": { text: "Cleanup failed, quarantined", tone: "bad" },
};

export const cleanupState = (s?: string) => known(cleanups, s ?? "pending");

const isolation: Record<string, string> = {
  "vm-per-job": "Separate VM for each build",
  "rootless-dedicated-vm": "Rootless, dedicated VM",
  "rootless-host": "Rootless, on the host",
};

export const isolationText = (p?: string) =>
  p ? (isolation[p] ?? p) : undefined;

const outcomes: Record<string, Label> = {
  passed: { text: "Passed", tone: "ok" },
  failed: { text: "Failed", tone: "bad" },
  blocked: { text: "Blocked", tone: "bad" },
  skipped: { text: "Skipped", tone: "warn" },
  errored: { text: "Errored", tone: "bad" },
  incomplete: { text: "Incomplete", tone: "warn" },
};

/** A validation verdict or check outcome; skipped is never shown as success. */
export const checkOutcome = (s?: string) => known(outcomes, s);

const publications: Record<string, Label> = {
  requested: { text: "Requested", tone: "info" },
  awaiting_approval: { text: "Awaiting approval", tone: "warn" },
  approved: { text: "Approved", tone: "info" },
  gate_check: { text: "Checking", tone: "info" },
  publishing: { text: "Publishing", tone: "info" },
  verifying: { text: "Verifying", tone: "info" },
  published: { text: "Published", tone: "ok" },
  retired: { text: "Retired", tone: "neutral" },
  rejected: { text: "Rejected", tone: "bad" },
  expired: { text: "Expired", tone: "warn" },
  blocked: { text: "Blocked", tone: "bad" },
  failed: { text: "Failed", tone: "bad" },
  cancelled: { text: "Cancelled", tone: "neutral" },
};

export const publicationState = (s?: string) => known(publications, s);

const exceptions: Record<string, Label> = {
  requested: { text: "Requested", tone: "info" },
  approved: { text: "Approved", tone: "ok" },
  rejected: { text: "Rejected", tone: "bad" },
  revoked: { text: "Revoked", tone: "neutral" },
};

export const exceptionState = (s?: string) => known(exceptions, s);

const health: Record<string, Label> = {
  ok: { text: "Healthy", tone: "ok" },
  degraded: { text: "Degraded", tone: "warn" },
  unavailable: { text: "Unavailable", tone: "bad" },
  "not-configured": { text: "Not configured", tone: "neutral" },
};

export const healthState = (s?: string) => known(health, s);

const enrollment: Record<string, Label> = {
  expired: { text: "Enrollment expired", tone: "warn" },
  revoked: { text: "Revoked", tone: "bad" },
};

const operational: Record<string, Label> = {
  active: { text: "Active", tone: "ok" },
  cordoned: { text: "Cordoned", tone: "warn" },
  draining: { text: "Draining", tone: "info" },
  quarantined: { text: "Quarantined", tone: "bad" },
};

/** A worker's enrollment and operational state as one label; an expired or revoked enrollment wins. */
export const workerState = (w: {
  enrollmentState: string;
  operationalState: string;
}): Label =>
  enrollment[w.enrollmentState] ?? known(operational, w.operationalState);

const selfTests: Record<string, Label> = {
  "never-run": { text: "Not run yet", tone: "neutral" },
  passed: { text: "Passed", tone: "ok" },
  failed: { text: "Failed", tone: "bad" },
};

export function selfTestState(t: {
  status: string;
  current?: boolean;
  running?: boolean;
}): Label {
  if (t.running) return { text: "Running", tone: "info" };
  const l = known(selfTests, t.status);
  if (t.status !== "never-run" && t.current === false)
    return { text: `${l.text}, outdated`, tone: "warn" };
  return l;
}

const recipeStatuses: Record<string, Label> = {
  draft: { text: "Draft", tone: "neutral" },
  approved: { text: "Approved", tone: "ok" },
  deprecated: { text: "Deprecated", tone: "warn" },
  withdrawn: { text: "Withdrawn", tone: "bad" },
};

/** Recipes, recipe versions and runtimes share these states. */
export const recipeStatus = (s?: string) => known(recipeStatuses, s);

const conversions: Record<string, Label> = {
  queued: { text: "Queued", tone: "info" },
  assigned: { text: "Assigned", tone: "info" },
  running: { text: "Running", tone: "info" },
  "needs-answers": { text: "Needs answers", tone: "warn" },
  succeeded: { text: "Generated", tone: "ok" },
  failed: { text: "Failed", tone: "bad" },
  "timed-out": { text: "Timed out", tone: "bad" },
  cancelled: { text: "Cancelled", tone: "neutral" },
};

export const conversionState = (s?: string) => known(conversions, s);

const severities: Record<string, Label> = {
  error: { text: "Error", tone: "bad" },
  review: { text: "Review", tone: "warn" },
  warning: { text: "Warning", tone: "warn" },
  info: { text: "Info", tone: "info" },
};

export const findingSeverity = (s?: string) => known(severities, s);

const rollouts: Record<string, Label> = {
  running: { text: "Running", tone: "info" },
  succeeded: { text: "Succeeded", tone: "ok" },
  failed: { text: "Failed", tone: "bad" },
  cancelled: { text: "Cancelled", tone: "warn" },
};

export const rolloutState = (s?: string) => known(rollouts, s);

const rolloutSteps: Record<string, Label> = {
  pending: { text: "Waiting", tone: "neutral" },
  draining: { text: "Draining", tone: "info" },
  requested: { text: "Requested", tone: "info" },
  applying: { text: "Installing", tone: "info" },
  testing: { text: "Self-testing", tone: "info" },
  rolling_back: { text: "Rolling back", tone: "warn" },
  succeeded: { text: "Updated", tone: "ok" },
  failed: { text: "Failed", tone: "bad" },
  skipped: { text: "Skipped", tone: "neutral" },
};

export const rolloutStep = (s?: string) => known(rolloutSteps, s);

const checks: Record<string, Label> = {
  passed: { text: "Passed", tone: "ok" },
  warning: { text: "Warning", tone: "warn" },
  failed: { text: "Failed", tone: "bad" },
};

export const checkState = (s?: string) => known(checks, s);

const importActions: Record<string, Label> = {
  create: { text: "Create", tone: "ok" },
  update: { text: "Update", tone: "info" },
  unchanged: { text: "Unchanged", tone: "neutral" },
  skip: { text: "Skip", tone: "warn" },
};

export const importAction = (s?: string) => known(importActions, s);

export type StepStatus = "current" | "done" | "todo" | "problem";

/** A wizard step is done once passed, unless what it asks for is still missing or will fail. */
export function stepStatus(
  step: number,
  current: number,
  furthest: number,
  problem: boolean,
): StepStatus {
  if (step === current) return "current";
  if (step > furthest) return "todo";
  return problem ? "problem" : "done";
}

export function plural(n: number, one: string, many = `${one}s`): string {
  return `${n} ${n === 1 ? one : many}`;
}
