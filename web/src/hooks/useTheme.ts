'use client';

import { useState, useEffect, useCallback } from 'react';

type Theme = 'light' | 'dark';

const APP_THEME_ATTR = 'data-theme';
const PACKAGE_THEME_ATTR = 'data-tb-theme';

function applyTheme(theme: Theme) {
  document.documentElement.setAttribute(APP_THEME_ATTR, theme);
  document.documentElement.setAttribute(PACKAGE_THEME_ATTR, theme);
}

export function useTheme() {
  const [theme, setThemeState] = useState<Theme>('dark');

  useEffect(() => {
    const stored = localStorage.getItem('peasant-theme') as Theme | null;
    // Reject stale/garbage stored values: only an explicit 'light' opts out of
    // the dark default (aligns with village's stricter guard).
    const initial = stored === 'light' ? 'light' : 'dark';
    setThemeState(initial);
    applyTheme(initial);
  }, []);

  const setTheme = useCallback((next: Theme) => {
    setThemeState(next);
    localStorage.setItem('peasant-theme', next);
    applyTheme(next);
  }, []);

  const toggle = useCallback(() => {
    setTheme(theme === 'light' ? 'dark' : 'light');
  }, [theme, setTheme]);

  return { theme, setTheme, toggle };
}
