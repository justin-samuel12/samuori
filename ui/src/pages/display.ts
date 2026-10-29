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
