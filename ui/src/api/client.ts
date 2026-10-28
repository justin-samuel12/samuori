import createClient, { type Middleware } from "openapi-fetch";
import type { components, paths } from "./generated/schema";

export type Schemas = components["schemas"];
export type Problem = Schemas["Problem"];

export function readCookie(name: string): string | undefined {
  for (const part of document.cookie.split("; ")) {
    const eq = part.indexOf("=");
    if (part.slice(0, eq) === name)
      return decodeURIComponent(part.slice(eq + 1));
  }
  return undefined;
}

const csrf: Middleware = {
  onRequest({ request }) {
    if (request.method !== "GET" && request.method !== "HEAD") {
      const token = readCookie("samuori_csrf");
      if (token) request.headers.set("X-CSRF-Token", token);
    }
    return request;
  },
};

const unauthenticated = new Set<() => void>();

const sessionWatch: Middleware = {
  onResponse({ response, request }) {
    if (response.status === 401 && !request.url.endsWith("/auth/login")) {
      unauthenticated.forEach((fn) => fn());
    }
    return response;
  },
};

// The release this tab first talked to, and listeners for a newer one (X-Samuori-Version, after an update).
let loadedVersion: string | undefined;
let newerVersion: string | undefined;
const updated = new Set<(version: string) => void>();

/** The release this tab first talked to, once a response has said. */
export function loadedServerVersion(): string | undefined {
  return loadedVersion;
}

/** Whether a response has reported a newer release than the one this tab loaded. */
export function serverIsNewer(): boolean {
  return newerVersion !== undefined;
}

/** Orders a.b.c[-pre] releases; build metadata after + is ignored, as on the server. */
export function compareVersions(a: string, b: string): number {
  const parse = (v: string) => {
    const [core = "", pre = ""] = (v.split("+")[0] ?? "").split(/-(.*)/s);
    return { nums: core.split(".").map((n) => Number(n) || 0), pre };
  };
  const x = parse(a);
  const y = parse(b);
  for (let i = 0; i < 3; i++) {
    const d = (x.nums[i] ?? 0) - (y.nums[i] ?? 0);
    if (d !== 0) return Math.sign(d);
  }
  if (x.pre === y.pre) return 0;
  if (!x.pre) return 1;
  if (!y.pre) return -1;
  return x.pre < y.pre ? -1 : 1;
}

const versionWatch: Middleware = {
  onResponse({ response }) {
    const v = response.headers.get("X-Samuori-Version");
    if (!v) return response;
    if (!loadedVersion) loadedVersion = v;
    else if (
      v !== loadedVersion &&
      v !== newerVersion &&
      compareVersions(v, loadedVersion) >= 0
    ) {
      newerVersion = v;
      updated.forEach((fn) => fn(v));
    }
    return response;
  },
};

/** Calls fn when the server runs a newer release than the one this tab loaded. */
export function onServerUpdated(fn: (version: string) => void): () => void {
  updated.add(fn);
  if (newerVersion) fn(newerVersion);
  return () => updated.delete(fn);
}

export function onUnauthenticated(fn: () => void): () => void {
  unauthenticated.add(fn);
  return () => unauthenticated.delete(fn);
}

export const api = createClient<paths>({
  baseUrl: `${window.location.origin}/api/v1`,
  credentials: "same-origin",
});
api.use(csrf, sessionWatch, versionWatch);

/** Converts an API error body into a sentence suitable for an alert. */
export function describeError(
  error: unknown,
  fallback = "Something went wrong.",
): string {
  const msg = (error as Partial<Problem> | undefined)?.message;
  if (!msg) return fallback;
  const sentence = msg.charAt(0).toUpperCase() + msg.slice(1);
  return /[.!?]$/.test(sentence) ? sentence : `${sentence}.`;
}
