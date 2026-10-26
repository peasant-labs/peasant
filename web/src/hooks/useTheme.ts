'use client';

import { useCallback, useEffect, useSyncExternalStore } from 'react';
import { LIGHT_SCHEME, THEME_STORAGE_KEY } from '@/lib/themeBoot';

type Theme = 'light' | 'dark';

const APP_THEME_ATTR = 'data-theme';
const PACKAGE_THEME_ATTR = 'data-tb-theme';

/** The choice stored on this device, if it is one of the two themes. */
function storedTheme(): Theme | null {
  try {
    const value = localStorage.getItem(THEME_STORAGE_KEY);
    return value === 'light' || value === 'dark' ? value : null;
  } catch {
    return null;
  }
}

/** The system's light/dark query, where the browser has one. */
function schemeQuery(): MediaQueryList | null {
  return typeof window !== 'undefined' && typeof window.matchMedia === 'function' ? window.matchMedia(LIGHT_SCHEME) : null;
}

/** The person's own choice when they made one on this device, else the system's scheme (dark where it cannot say). */
function preferredTheme(): Theme {
  return storedTheme() ?? (schemeQuery()?.matches ? 'light' : 'dark');
}

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
    // A stored choice wins; with none (or a garbage value) the page follows
    // the system's scheme, the same rule the head script applied before paint.
    const initial = preferredTheme();
    // Always stamp the attributes on first mount — the store may already hold
    // `initial`, in which case commit() short-circuits and the DOM would keep
    // whatever the prerendered HTML shipped.
    applyTheme(initial);
    commit(initial);
    // With no stored choice, a system switch (a scheduled dark mode, a changed
    // setting) re-themes the page too.
    const query = schemeQuery();
    const follow = () => { if (storedTheme() === null) commit(preferredTheme()); };
    query?.addEventListener?.('change', follow);
    return () => query?.removeEventListener?.('change', follow);
  }, []);

  const setTheme = useCallback((next: Theme) => {
    try {
      localStorage.setItem(THEME_STORAGE_KEY, next);
    } catch {
      // The theme still applies to this page; keeping it is best effort.
    }
    commit(next);
  }, []);

  // Reads the store rather than a captured render value, so a toggle is correct
  // no matter which component runs it or how stale that component's render is.
  const toggle = useCallback(() => {
    setTheme(current === 'light' ? 'dark' : 'light');
  }, [setTheme]);

  return { theme, setTheme, toggle };
}
