import { useSyncExternalStore } from "react";

export type Theme = "system" | "light" | "dark";

const key = "samuori.theme";
const listeners = new Set<() => void>();

function read(): Theme {
  try {
    const v = window.localStorage.getItem(key);
    return v === "light" || v === "dark" ? v : "system";
  } catch {
    return "system";
  }
}

let current: Theme = read();

export function applyTheme(t: Theme = current) {
  const root = document.documentElement;
  if (t === "system") delete root.dataset.theme;
  else root.dataset.theme = t;
}

export function setTheme(t: Theme) {
  current = t;
  try {
    if (t === "system") window.localStorage.removeItem(key);
    else window.localStorage.setItem(key, t);
  } catch {
    // The choice still applies to this page when storage is unavailable.
  }
  applyTheme(t);
  listeners.forEach((l) => l());
}

export function useTheme(): Theme {
  return useSyncExternalStore(
    (l) => {
      listeners.add(l);
      return () => listeners.delete(l);
    },
    () => current,
  );
}
