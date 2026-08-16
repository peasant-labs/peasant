'use client';

import { useCallback, useEffect, useSyncExternalStore } from 'react';

type Theme = 'light' | 'dark';

const APP_THEME_ATTR = 'data-theme';
const PACKAGE_THEME_ATTR = 'data-tb-theme';
const STORAGE_KEY = 'peasant-theme';

/**
 * The theme is ONE value shared by every consumer, held in a module store and
 * read through useSyncExternalStore.
 *
 * It used to be per-component `useState`. Each caller then held its own copy,
 * and a toggle only updated the component that ran it: the toggle also writes
 * `data-theme` to <html>, so everything styled by CSS re-themed and the app
 * LOOKED correct — but any component that passes the theme as a PROP kept
 * handing down its stale value. That is exactly what happened to the transcript
 * viewer, which takes `theme` and stamps its own `data-theme` on its root: the
 * shell went dark while the session page stayed light.
 *
 * A shared store means one source of truth for the value, matching the single
 * source of truth already on the DOM attributes.
 */
let current: Theme = 'dark';
const listeners = new Set<() => void>();

function emit(): void {
  for (const listener of listeners) listener();
}

function applyTheme(theme: Theme): void {
  document.documentElement.setAttribute(APP_THEME_ATTR, theme);
  document.documentElement.setAttribute(PACKAGE_THEME_ATTR, theme);
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

function getSnapshot(): Theme {
  return current;
}

/** Prerender/SSR value — matches the `data-theme` layout.tsx ships in the HTML. */
function getServerSnapshot(): Theme {
  return 'dark';
}

function commit(theme: Theme): void {
  if (current === theme) return;
  current = theme;
  applyTheme(theme);
  emit();
}

export function useTheme() {
  const theme = useSyncExternalStore(subscribe, getSnapshot, getServerSnapshot);

  useEffect(() => {
    const stored = localStorage.getItem(STORAGE_KEY) as Theme | null;
    // Reject stale/garbage stored values: only an explicit 'light' opts out of
    // the dark default (aligns with village's stricter guard).
    const initial = stored === 'light' ? 'light' : 'dark';
    // Always stamp the attributes on first mount — the store may already hold
    // `initial`, in which case commit() short-circuits and the DOM would keep
    // whatever the prerendered HTML shipped.
    applyTheme(initial);
    commit(initial);
  }, []);

  const setTheme = useCallback((next: Theme) => {
    localStorage.setItem(STORAGE_KEY, next);
    commit(next);
  }, []);

  // Reads the store rather than a captured render value, so a toggle is correct
  // no matter which component runs it or how stale that component's render is.
  const toggle = useCallback(() => {
    setTheme(current === 'light' ? 'dark' : 'light');
  }, [setTheme]);

  return { theme, setTheme, toggle };
}
