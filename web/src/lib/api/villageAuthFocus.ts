'use client';

/**
 * One reader of this computer's village sign-in for the pages that re-read it
 * when the window regains focus: the sidebar's account row and the online
 * page's handle. A sign-in or a log-out happens in a terminal or another tab,
 * so a return to this window is when the answer may have changed.
 *
 * Every subscriber shares one `focus` listener and one read, so one window
 * focus is at most one `GET /api/v1/sync/auth` however many are mounted:
 *
 * - a focus while a read is in flight starts no read;
 * - the next read starts no sooner than `gapMs` after the last one ended;
 * - a focus that one of those rules held back is kept as one trailing read at
 *   the end of the gap, so a change made in another window is not missed until
 *   the next focus.
 *
 * A read that fails reports no account, as a failed read always has: nobody
 * guesses who is signed in. It ends like any other read, so it never holds up
 * the next one. The clock, the timer and the event target are injected, so
 * the timing is tested on a fake clock against the real reader.
 */

import type { SyncAuthResponse } from '@peasant-labs/schema';
import { fetchVillageAuth } from './settings';

/** The least time from the end of one read to the start of the next. */
export const VILLAGE_AUTH_FOCUS_GAP_MS = 2000;

/** What a subscriber is told: the sign-in as read, or null when the read failed. */
export type VillageAuthReading = SyncAuthResponse | null;
export type VillageAuthListener = (reading: VillageAuthReading) => void;

export interface VillageAuthFocusDeps {
  /** Reads the sign-in once. */
  read: () => Promise<SyncAuthResponse>;
  /** The current time in milliseconds. */
  now: () => number;
  setTimer: (run: () => void, ms: number) => unknown;
  clearTimer: (timer: unknown) => void;
  /** What fires `focus`: the window. */
  target: Pick<EventTarget, 'addEventListener' | 'removeEventListener'>;
  gapMs?: number;
}

export interface VillageAuthFocusReader {
  /**
   * Hear every reading from now on. A subscriber is told the last reading at
   * once when there is one; the first subscriber starts listening for focus,
   * and a subscriber is read for unless a read is in flight or the last one
   * ended inside the gap. Returns the unsubscribe; the last one stops
   * listening, drops a trailing read that has not started, lets a read in
   * flight go unheard, and forgets the last reading.
   */
  subscribe(listener: VillageAuthListener): () => void;
}

export function createVillageAuthFocusReader(deps: VillageAuthFocusDeps): VillageAuthFocusReader {
  const gapMs = deps.gapMs ?? VILLAGE_AUTH_FOCUS_GAP_MS;
  const listeners = new Set<VillageAuthListener>();
  let inFlight = false;
  let lastEnd: number | null = null;
  let last: { reading: VillageAuthReading } | null = null;
  let trailing: unknown = null;
  // A focus came while a read was in flight: read once more after the gap.
  let owed = false;
  // Which subscription period a read belongs to: one that outlived every
  // subscriber is let go, and its answer is heard by nobody.
  let period = 0;

  const tell = (reading: VillageAuthReading) => {
    last = { reading };
    for (const listener of [...listeners]) listener(reading);
  };

  const finish = (reading: VillageAuthReading) => {
    inFlight = false;
    lastEnd = deps.now();
    tell(reading);
    if (owed) {
      owed = false;
      schedule();
    }
  };

  const start = () => {
    inFlight = true;
    const mine = period;
    const end = (reading: VillageAuthReading) => {
      if (mine === period) finish(reading);
    };
    let reading: Promise<SyncAuthResponse>;
    try {
      reading = deps.read();
    } catch {
      end(null);
      return;
    }
    reading.then(end, () => end(null));
  };

  const schedule = () => {
    if (trailing !== null || listeners.size === 0) return;
    const wait = lastEnd === null ? 0 : Math.max(0, lastEnd + gapMs - deps.now());
    trailing = deps.setTimer(() => {
      trailing = null;
      if (listeners.size > 0 && !inFlight) start();
    }, wait);
  };

  /** A reason to read: start one now if the rules allow it, or keep it for later. */
  const ask = (keepIfHeld: boolean) => {
    if (inFlight) {
      if (keepIfHeld) owed = true;
      return;
    }
    if (trailing !== null) return;
    if (lastEnd !== null && deps.now() - lastEnd < gapMs) {
      if (keepIfHeld) schedule();
      return;
    }
    start();
  };

  const onFocus = () => ask(true);

  return {
    subscribe(listener) {
      const first = listeners.size === 0;
      listeners.add(listener);
      if (first) deps.target.addEventListener('focus', onFocus);
      if (last !== null) listener(last.reading);
      // A reading younger than the gap is the one to show: the subscriber has it.
      ask(false);
      return () => {
        if (!listeners.delete(listener) || listeners.size > 0) return;
        // Nobody is left to tell: stop listening, drop the trailing read, let
        // a read in flight go, and forget the last reading, so whoever comes
        // next reads the sign-in afresh instead of being shown an old one.
        deps.target.removeEventListener('focus', onFocus);
        if (trailing !== null) {
          deps.clearTimer(trailing);
          trailing = null;
        }
        period += 1;
        owed = false;
        inFlight = false;
        last = null;
        lastEnd = null;
      };
    },
  };
}

let shared: VillageAuthFocusReader | null = null;

/** The app's one reader, on the window and the real sign-in route. */
export function villageAuthFocusReader(): VillageAuthFocusReader {
  shared ??= createVillageAuthFocusReader({
    read: fetchVillageAuth,
    now: () => Date.now(),
    setTimer: (run, ms) => window.setTimeout(run, ms),
    clearTimer: (timer) => window.clearTimeout(timer as number),
    target: window,
  });
  return shared;
}
