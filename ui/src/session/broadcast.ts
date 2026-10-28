export interface Deadlines {
  /** Local-clock milliseconds. */
  idleExpiresAt: number;
  absoluteExpiresAt: number;
  idleTimeoutMs: number;
  warningMs: number;
  renewWith?: "password" | "provider";
  providerName?: string;
}

export type SessionMessage =
  | { type: "deadlines"; deadlines: Deadlines }
  | { type: "signed-out"; notice: string }
  | { type: "signed-in" };

const name = "samuori-session";
const channel =
  typeof BroadcastChannel === "undefined" ? null : new BroadcastChannel(name);
const listeners = new Set<(m: SessionMessage) => void>();

channel?.addEventListener("message", (e: MessageEvent<SessionMessage>) =>
  listeners.forEach((fn) => fn(e.data)),
);

if (!channel) {
  window.addEventListener("storage", (e) => {
    if (e.key !== name || !e.newValue) return;
    try {
      const m = (JSON.parse(e.newValue) as { m: SessionMessage }).m;
      listeners.forEach((fn) => fn(m));
    } catch {
      /* ignore malformed values */
    }
  });
}

/** Tells the other tabs of this browser; the sending tab does not receive its own message. */
export function broadcast(m: SessionMessage) {
  if (channel) {
    channel.postMessage(m);
    return;
  }
  try {
    localStorage.setItem(
      name,
      JSON.stringify({ m, n: `${Date.now()}-${Math.random()}` }),
    );
  } catch {
    /* storage unavailable: tabs fall back to their own polling */
  }
}

export function onBroadcast(fn: (m: SessionMessage) => void): () => void {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

const returnKey = "samuori-return-to";

/** Remembers the page to come back to across an identity-provider sign-in. */
export function rememberReturn() {
  try {
    sessionStorage.setItem(
      returnKey,
      window.location.pathname + window.location.search,
    );
  } catch {
    /* without storage the provider sign-in lands on Home */
  }
}

export function takeReturn(): string | undefined {
  try {
    const to = sessionStorage.getItem(returnKey);
    sessionStorage.removeItem(returnKey);
    return to?.startsWith("/") && !to.startsWith("//") ? to : undefined;
  } catch {
    return undefined;
  }
}
