import { createContext, useCallback, useContext, useEffect, useState } from "react";

// Theme preference persisted across reloads. "system" follows the browser's
// prefers-color-scheme; "light"/"dark" are explicit overrides. The same key
// is read by the pre-paint inline script in index.html, so keep it in sync.
export type ThemePref = "system" | "light" | "dark";
export type Theme = "light" | "dark";

const THEME_KEY = "gosmokeping.theme";
const DARK_QUERY = "(prefers-color-scheme: dark)";

export function readPref(): ThemePref {
  try {
    const v = localStorage.getItem(THEME_KEY);
    if (v === "light" || v === "dark" || v === "system") return v;
  } catch {
    // localStorage unavailable — fall through to the default.
  }
  return "system";
}

function systemTheme(): Theme {
  if (typeof matchMedia === "undefined") return "dark";
  return matchMedia(DARK_QUERY).matches ? "dark" : "light";
}

export function resolveTheme(pref: ThemePref): Theme {
  return pref === "system" ? systemTheme() : pref;
}

// applyTheme writes the resolved theme onto <html> synchronously. Charts read
// their chrome colors from CSS variables via getComputedStyle, so the attribute
// must land before React re-renders their effects — the caller does this in the
// same tick it changes state, ahead of the child re-render.
export function applyTheme(theme: Theme): void {
  const root = document.documentElement;
  root.dataset.theme = theme;
  root.style.colorScheme = theme;
}

// ThemeContext carries the effective theme so a chart can put it in an effect's
// dependency list and repaint when the theme flips. Default matches the CSS
// default (dark) for a consumer rendered outside the provider.
export const ThemeContext = createContext<Theme>("dark");

export function useEffectiveTheme(): Theme {
  return useContext(ThemeContext);
}

export function useThemeController() {
  const [pref, setPrefState] = useState<ThemePref>(() => readPref());
  const [effective, setEffective] = useState<Theme>(() => resolveTheme(readPref()));

  // Follow OS changes only while the preference is "system".
  useEffect(() => {
    if (typeof matchMedia === "undefined") return;
    const mq = matchMedia(DARK_QUERY);
    const onChange = () => {
      if (pref !== "system") return;
      const next = systemTheme();
      applyTheme(next);
      setEffective(next);
    };
    mq.addEventListener("change", onChange);
    return () => mq.removeEventListener("change", onChange);
  }, [pref]);

  const setPref = useCallback((next: ThemePref) => {
    try {
      localStorage.setItem(THEME_KEY, next);
    } catch {
      // localStorage unavailable — the choice holds for this session only.
    }
    setPrefState(next);
    const resolved = resolveTheme(next);
    applyTheme(resolved);
    setEffective(resolved);
  }, []);

  return { pref, effective, setPref };
}
