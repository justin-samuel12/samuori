export const stateText: Record<string, string> = {
  draft: "Draft",
  pending_admission: "Checking",
  queued: "Waiting for a worker",
  assigned: "Assigned",
  preparing: "Preparing",
  building: "Building",
  committing: "Saving the image",
  succeeded: "Built",
  blocked: "Blocked",
  failed: "Failed",
  timed_out: "Timed out",
  cancelled: "Cancelled",
  expired: "Expired",
  discarded: "Discarded",
};

export const terminal = new Set([
  "succeeded",
  "blocked",
  "failed",
  "timed_out",
  "cancelled",
  "expired",
  "discarded",
]);

export function tone(state: string): string {
  if (state === "succeeded") return "ok";
  if (state === "failed" || state === "blocked" || state === "timed_out")
    return "unavailable";
  if (state === "cancelled" || state === "expired") return "degraded";
  return "pending";
}

export function badgeTone(state: string): "ok" | "bad" | "warn" | "info" {
  const t = tone(state);
  if (t === "ok") return "ok";
  if (t === "unavailable") return "bad";
  if (t === "degraded") return "warn";
  return "info";
}
