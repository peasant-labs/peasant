'use client';

import { useCallback, useEffect, useMemo, useState } from 'react';

/**
 * localStorage key for first-run tour completion. Mirrors the `peasant-theme`
 * pattern in {@link useTheme}: a single namespaced key, JSON-encoded payload.
 */
export const TOUR_STORAGE_KEY = 'peasant-tour';

/** Persisted shape. `completedAt` is set whenever the tour ends (finish OR skip). */
export interface TourRecord {
  /** ISO timestamp of the moment the tour was completed or skipped. */
  completedAt: string;
}

/**
 * Read the persisted tour record. Returns `null` when nothing is stored or the
 * value is unparseable (defensive against hand-edited localStorage).
 */
export function readTourRecord(): TourRecord | null {
  if (typeof window === 'undefined') return null;
  try {
    const raw = window.localStorage.getItem(TOUR_STORAGE_KEY);
    if (!raw) return null;
    const parsed = JSON.parse(raw) as Partial<TourRecord>;
    if (typeof parsed?.completedAt === 'string') {
      return { completedAt: parsed.completedAt };
    }
    return null;
  } catch {
    return null;
  }
}

/** Persist (or clear) the tour record. */
function writeTourRecord(record: TourRecord | null): void {
  if (typeof window === 'undefined') return;
  try {
    if (record === null) {
      window.localStorage.removeItem(TOUR_STORAGE_KEY);
    } else {
      window.localStorage.setItem(TOUR_STORAGE_KEY, JSON.stringify(record));
    }
  } catch {
    /* private mode / quota — non-fatal, tour just won't persist */
  }
}

export interface TourState {
  /** Whether the tour overlay is currently visible. */
  active: boolean;
  /** Zero-based index of the current step. Meaningful only while `active`. */
  index: number;
  /** Whether a completion/skip record exists in localStorage. */
  completed: boolean;
  /** Whether `index` points at the final step. */
  isLast: boolean;
  /** Whether `index` points at the first step. */
  isFirst: boolean;
}

export interface TourControls extends TourState {
  /** Begin the tour from step 0. No-op if already active. */
  start: () => void;
  /** Advance to the next step, completing the tour after the last step. */
  next: () => void;
  /** Move to the previous step. No-op on the first step. */
  prev: () => void;
  /** Jump directly to a step index (clamped to range). */
  goTo: (index: number) => void;
  /** End the tour early and persist completion so it never re-triggers. */
  skip: () => void;
  /** Mark the tour finished and persist completion. */
  complete: () => void;
}

export interface UseTourStateOptions {
  /** Total number of steps. Must be >= 1. */
  stepCount: number;
}

/**
 * Pure-ish tour state machine. Owns the step cursor and the
 * "don't show again" persistence; knows nothing about the DOM, routing, or
 * rendering. Both {@link skip} and {@link complete} write a `completedAt`
 * record so a returning visitor is never re-prompted.
 *
 * Extracted from the provider so the state transitions (advance / skip /
 * complete / no-retrigger) are unit-testable without mounting React Portals.
 */
export function useTourState({ stepCount }: UseTourStateOptions): TourControls {
  const [active, setActive] = useState(false);
  const [index, setIndex] = useState(0);
  // `completed` reflects localStorage. Initialised lazily on mount (SSR-safe).
  const [completed, setCompleted] = useState(false);

  useEffect(() => {
    setCompleted(readTourRecord() !== null);
  }, []);

  const lastIndex = Math.max(0, stepCount - 1);

  const persistDone = useCallback(() => {
    const record: TourRecord = { completedAt: new Date().toISOString() };
    writeTourRecord(record);
    setCompleted(true);
  }, []);

  const start = useCallback(() => {
    setActive((wasActive) => {
      if (wasActive) return wasActive;
      setIndex(0);
      return true;
    });
  }, []);

  const complete = useCallback(() => {
    setActive(false);
    persistDone();
  }, [persistDone]);

  const skip = useCallback(() => {
    setActive(false);
    persistDone();
  }, [persistDone]);

  const next = useCallback(() => {
    if (index >= lastIndex) {
      // Past the last step → finish (same effect as `complete`).
      setActive(false);
      persistDone();
      return;
    }
    setIndex((i) => Math.min(lastIndex, i + 1));
  }, [index, lastIndex, persistDone]);

  const prev = useCallback(() => {
    setIndex((i) => Math.max(0, i - 1));
  }, []);

  const goTo = useCallback(
    (target: number) => {
      setIndex(() => Math.min(lastIndex, Math.max(0, target)));
    },
    [lastIndex],
  );

  return useMemo<TourControls>(
    () => ({
      active,
      index,
      completed,
      isFirst: index === 0,
      isLast: index === lastIndex,
      start,
      next,
      prev,
      goTo,
      skip,
      complete,
    }),
    [active, index, completed, lastIndex, start, next, prev, goTo, skip, complete],
  );
}
