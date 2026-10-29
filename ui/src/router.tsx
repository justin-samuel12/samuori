import { useSyncExternalStore } from "react";

const listeners = new Set<() => void>();

function emit() {
  listeners.forEach((l) => l());
}

window.addEventListener("popstate", emit);

let notice: string | undefined;

/** Navigates; notice is a one-line confirmation the next page shows once (takeNotice). */
export function navigate(to: string, message?: string) {
  notice = message;
  if (to === window.location.pathname + window.location.search) return;
  window.history.pushState(null, "", to);
  window.scrollTo(0, 0);
  emit();
}

/** Replaces the current history entry, so Back skips a page that only forwards. */
export function redirect(to: string) {
  window.history.replaceState(null, "", to);
  emit();
}

export function takeNotice(): string | undefined {
  const n = notice;
  notice = undefined;
  return n;
}

function subscribe(l: () => void) {
  listeners.add(l);
  return () => listeners.delete(l);
}

export function usePath(): string {
  return useSyncExternalStore(subscribe, () => window.location.pathname);
}

/** Matches "/projects/:id" style patterns, returning params or null. */
export function match(
  pattern: string,
  path: string,
): Record<string, string> | null {
  const a = pattern.split("/").filter(Boolean);
  const b = path.split("/").filter(Boolean);
  if (a.length !== b.length) return null;
  const params: Record<string, string> = {};
  for (let i = 0; i < a.length; i++) {
    const seg = a[i] as string;
    const val = b[i] as string;
    if (seg.startsWith(":")) params[seg.slice(1)] = decodeURIComponent(val);
    else if (seg !== val) return null;
  }
  return params;
}
