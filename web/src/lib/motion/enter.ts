'use client';

import { useEffect, type RefObject } from 'react';

/** The one easing every entrance uses: a quick start that settles softly. */
export const ENTER_EASING = 'cubic-bezier(0.23, 1, 0.32, 1)';
const ENTER_MS = 280;
/** How far a page rises as it arrives. */
const ENTER_RISE = '8px';

/**
 * Plays a short entrance on `element`: it fades in and, unless `rise` is off,
 * rises the last few pixels into place. Opacity and transform only, so it runs
 * on the compositor. It does nothing when the reader asks for reduced motion
 * or the browser has no Web Animations, and a newer entrance replaces one
 * still playing. Returns a function that stops it.
 */
export function playEnter(element: HTMLElement, { rise = true }: { rise?: boolean } = {}): () => void {
  if (typeof element.animate !== 'function') return () => {};
  if (typeof window.matchMedia === 'function' && !window.matchMedia('(prefers-reduced-motion: no-preference)').matches) return () => {};
  for (const running of element.getAnimations?.() ?? []) if (running.id === 'enter') running.cancel();
  const animation = element.animate(
    rise
      ? [{ opacity: 0, transform: `translateY(${ENTER_RISE})` }, { opacity: 1, transform: 'none' }]
      : [{ opacity: 0 }, { opacity: 1 }],
    { duration: ENTER_MS, easing: ENTER_EASING },
  );
  animation.id = 'enter';
  return () => animation.cancel();
}

/** Plays the entrance on the referenced element each time `key` changes. */
export function useEnter(ref: RefObject<HTMLElement | null>, key: unknown, options?: { rise?: boolean }): void {
  const rise = options?.rise ?? true;
  useEffect(() => {
    const element = ref.current;
    if (!element) return;
    return playEnter(element, { rise });
  }, [ref, key, rise]);
}
