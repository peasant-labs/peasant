'use client';

import { useCallback, useEffect, useState, type ReactNode } from 'react';
import { X, HelpCircle } from 'lucide-react';

/**
 * The "What am I looking at?" block. One per surface: a few short lines saying
 * what the screen shows, what its words mean, and what to click. COLLAPSED by
 * default (the payload leads; the box is one click away on the square "?"
 * toggle beside the header); a user who opens it stays opted-in (persisted per
 * surface in localStorage). Never a modal, never a tour. Voice and placement
 * follow the values-driven UX rules in web/DESIGN_SYSTEM.md.
 *
 *   const explainer = useExplainer('changes');           // collapsed by default
 *   <ExplainerToggle explainer={explainer} />   // the "?" beside the header
 *   <Explainer explainer={explainer} title="what am I looking at?">
 *     <p>…short lines, with <Term/> on first-use words…</p>
 *   </Explainer>
 */

const STORAGE_PREFIX = 'peasant.explainer.';

export interface ExplainerState {
  id: string;
  open: boolean;
  hydrated: boolean;
  show: () => void;
  hide: () => void;
}

export function useExplainer(id: string, defaultOpen = false): ExplainerState {
  // Collapsed by default (defaultOpen=false) so the payload leads on every
  // surface; reconcile with localStorage after mount so SSR/first render is
  // deterministic (no hydration mismatch). A user who explicitly opened it
  // stays opted-in; one who dismissed it stays collapsed.
  const [open, setOpen] = useState(defaultOpen);
  const [hydrated, setHydrated] = useState(false);

  useEffect(() => {
    try {
      const stored = window.localStorage.getItem(STORAGE_PREFIX + id);
      if (stored === 'hidden') setOpen(false);
      else if (stored === 'open') setOpen(true);
    } catch {
      /* localStorage unavailable — keep the default */
    }
    setHydrated(true);
  }, [id]);

  const persist = useCallback(
    (next: boolean) => {
      setOpen(next);
      try {
        window.localStorage.setItem(STORAGE_PREFIX + id, next ? 'open' : 'hidden');
      } catch {
        /* ignore */
      }
    },
    [id],
  );

  const show = useCallback(() => persist(true), [persist]);
  const hide = useCallback(() => persist(false), [persist]);

  return { id, open, hydrated, show, hide };
}

export function Explainer({
  explainer,
  title = 'What am I looking at?',
  children,
}: {
  explainer: ExplainerState;
  title?: string;
  children: ReactNode;
}) {
  if (!explainer.open) return null;
  return (
    // `w-full block` so the box always spans the full content width of whatever
    // container it sits in (it was collapsing to content width inside the flex
    // header rows on Review & Map). `animate-explainer-in` gives a subtle
    // fade+slide on open (frozen to instant under prefers-reduced-motion). The
    // region's accessible name is the literal "Explanation" — distinct from the
    // "?" toggle's name ("What am I looking at?") so they don't collide.
    <div
      role="region"
      aria-label="Explanation"
      className="block w-full border border-rule bg-surface px-4 py-3 relative animate-explainer-in"
    >
      <button
        type="button"
        onClick={explainer.hide}
        aria-label="Hide explanation"
        className="absolute top-2 right-2 p-1 text-ink-4 hover:text-ink hover:bg-surface-hover transition-colors focus-mono cursor-pointer"
      >
        <X size={14} aria-hidden />
      </button>
      <p className="v2-eyebrow mb-1.5">{title}</p>
      <div className="text-[13px] text-ink-2 leading-relaxed pr-6 flex flex-col gap-1">
        {children}
      </div>
    </div>
  );
}

/**
 * The square "?" the host renders beside its header. Only visible once the
 * explainer is collapsed (and only after hydration, so it never flashes).
 */
export function ExplainerToggle({ explainer }: { explainer: ExplainerState }) {
  if (!explainer.hydrated || explainer.open) return null;
  return (
    <button
      type="button"
      onClick={explainer.show}
      aria-label="what am I looking at?"
      title="what am I looking at?"
      className="shrink-0 inline-flex items-center justify-center size-7 border border-rule bg-surface text-ink-3 hover:text-ink hover:bg-surface-hover transition-colors focus-mono cursor-pointer"
    >
      <HelpCircle size={15} aria-hidden />
    </button>
  );
}
