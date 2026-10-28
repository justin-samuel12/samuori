// Client-side hints only; the server validates every policy document (internal/policy/policy.go).

const pinnedFrontend = /^[a-z0-9][a-z0-9._/:-]*@sha256:[a-f0-9]{64}$/;
const ruleId = /^[a-z0-9][a-z0-9-]{0,62}$/;

export function frontendProblem(ref: string): string | undefined {
  const r = ref.trim();
  if (!r) return "Enter a frontend image.";
  if (!r.includes("@sha256:"))
    return "Pin the frontend by digest: name@sha256:<64 hex>.";
  if (!pinnedFrontend.test(r))
    return "Use a lower-case image name followed by @sha256: and 64 hex digits.";
  return undefined;
}

export function ruleIdProblem(
  id: string,
  taken: readonly string[],
): string | undefined {
  if (!ruleId.test(id))
    return "Use lower-case letters, digits and dashes, starting with a letter or digit.";
  if (taken.includes(id)) return "Another rule already uses this ID.";
  return undefined;
}

export function sensitivePathProblem(path: string): string | undefined {
  const p = path.trim();
  if (!p) return "Enter a path pattern.";
  if (p.startsWith("/"))
    return "Use a path relative to the image root, such as app/*.pem.";
  if (p.includes("..")) return "The pattern cannot contain '..'.";
  return undefined;
}

export const outcomes = ["warn", "require-exception", "block"] as const;
export const buildSecretClass = "build-secret";

/** Returns the list without the item at index. */
export function without<T>(list: readonly T[], index: number): T[] {
  return list.filter((_, i) => i !== index);
}

/** Replaces the item at index, or appends when index is the list's length. */
export function put<T>(list: readonly T[], index: number, item: T): T[] {
  const out = [...list];
  out[index] = item;
  return out;
}
