// Browser-local display preferences: theme (pre-login key kept for first-paint
// compatibility), default table density, and how times are shown. Display
// settings only; nothing protected is stored. A server-owned preference can
// replace this store later without changing its callers.
import { useSyncExternalStore } from "react";

export type Theme = "light" | "dark" | "system";
export type DateStyle = "relative" | "absolute";
export type Prefs = { theme: Theme; density: "comfortable" | "compact"; dateStyle: DateStyle };

const THEME_KEY = "blaxsmith-theme";
const PREFS_KEY = "blaxsmith:prefs:v1";
const defaults: Prefs = { theme: "system", density: "comfortable", dateStyle: "relative" };
const listeners = new Set<() => void>();

function load(): Prefs {
  try {
    const raw = JSON.parse(globalThis.localStorage?.getItem(PREFS_KEY) || "{}") as Partial<Prefs>;
    const theme = globalThis.localStorage?.getItem(THEME_KEY) as Theme | null;
    return {
      theme: theme === "light" || theme === "dark" || theme === "system" ? theme : defaults.theme,
      density: raw.density === "compact" ? "compact" : "comfortable",
      dateStyle: raw.dateStyle === "absolute" ? "absolute" : "relative",
    };
  } catch {
    return defaults;
  }
}

let current: Prefs = load();

export function getPrefs(): Prefs { return current; }

export function setPrefs(patch: Partial<Prefs>) {
  current = { ...current, ...patch };
  try {
    globalThis.localStorage?.setItem(THEME_KEY, current.theme);
    globalThis.localStorage?.setItem(PREFS_KEY, JSON.stringify({ density: current.density, dateStyle: current.dateStyle }));
  } catch { /* storage unavailable: preferences last for this page view */ }
  for (const listener of listeners) listener();
}

export function usePrefs(): Prefs {
  return useSyncExternalStore((listener) => { listeners.add(listener); return () => listeners.delete(listener); }, getPrefs, getPrefs);
}

export function applyTheme(theme: Theme) {
  const dark = theme === "dark" || (theme === "system" && globalThis.matchMedia?.("(prefers-color-scheme: dark)").matches);
  globalThis.document?.documentElement.classList.toggle("dark", Boolean(dark));
}
