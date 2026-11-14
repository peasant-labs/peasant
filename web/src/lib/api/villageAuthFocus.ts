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
 * Each read has a deadline that aborts the HTTP request, including consumption
 * of its body. The guard stays held until the read actually settles, and a
 * timeout reports no account just like any other failure: nobody guesses who
 * is signed in. A focus held during the read is retried after the usual gap.
 * A read started for a subscriber who has left is not canceled on unsubscribe:
 * its answer is told to nobody, but its end releases the in-flight guard and
 * starts the gap for whoever is mounted now, so a remount never overlaps it or
 * silently waits for a focus. The end time survives a period with no
 * subscribers, but the account reading does not: a remount inside the gap
 * schedules one fresh read at its end rather than showing an old account. The
 * clock, the timer and the event target are injected, so the timing is tested
 * on a fake clock against the real reader.
 */

import type { SyncAuthResponse } from '@peasant-labs/schema';
import { fetchVillageAuth } from './settings';

/** The least time from the end of one read to the start of the next. */
export const VILLAGE_AUTH_FOCUS_GAP_MS = 2000;

/** Only focus reads get ten seconds for a slow local server and its response body. */
export const VILLAGE_AUTH_FOCUS_TIMEOUT_MS = 10_000;

/** What a subscriber is told: the sign-in as read, or null when the read failed. */
export type VillageAuthReading = SyncAuthResponse | null;
export type VillageAuthListener = (reading: VillageAuthReading) => void;

export interface VillageAuthFocusDeps {
  /** Reads the sign-in once; abort must stop the request and body consumption. */
  read: (signal: AbortSignal) => Promise<SyncAuthResponse>;
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
   * once when there is one; with no reading and a read inside the gap it
   * schedules one fresh read for the end of that gap, so a quick remount
   * neither shows an old account nor waits for a focus. The first subscriber
   * starts listening for focus, and a subscriber is read for unless a read is
   * in flight. Returns the unsubscribe; the last one stops listening, drops a
   * trailing read that has not started, lets a read in flight go unheard, and
   * forgets the last reading but keeps its end time for the gap.
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
    const controller = new AbortController();
    // Do not release the guard here: wait for the aborted request to settle so
    // another focus cannot overlap a still-live fetch or response body.
    const deadline = deps.setTimer(() => controller.abort(), VILLAGE_AUTH_FOCUS_TIMEOUT_MS);
    const end = (reading: VillageAuthReading) => {
      deps.clearTimer(deadline);
      // A dependency must not deliver an account after its deadline, even if
      // it resolves instead of rejecting in response to the abort.
      if (controller.signal.aborted) reading = null;
      if (mine === period) {
        finish(reading);
        return;
      }
      // The subscriber this read belonged to has left. Its answer is not told,
      // but its end releases the guard and keeps the gap for whoever mounts
      // now, and a fresh read runs at the end of that gap.
      inFlight = false;
      lastEnd = deps.now();
      owed = false;
      if (listeners.size > 0) schedule();
    };
    let reading: Promise<SyncAuthResponse>;
    try {
      reading = deps.read(controller.signal);
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
      // With no reading to show (the period emptied and forgot it), a mount
      // inside the gap waits for one fresh read at the end of the gap instead
      // of showing an old account or waiting for a focus. A mount with a
      // reading to show, or with a read in flight, asks as before and does not
      // add a trailing read to an active period.
      if (last === null && !inFlight && trailing === null && lastEnd !== null && deps.now() - lastEnd < gapMs) {
        schedule();
      } else {
        ask(false);
      }
      return () => {
        if (!listeners.delete(listener) || listeners.size > 0) return;
        // Nobody is left to tell: stop listening, drop the trailing read, and
        // forget the last reading, so whoever comes next is not shown an old
        // account. Keep the end time: a remount inside the gap waits for one
        // fresh read at its end. A read in flight is not canceled; it keeps the
        // guard until it ends, and its completion starts the gap for whoever
        // mounts next.
        deps.target.removeEventListener('focus', onFocus);
        if (trailing !== null) {
          deps.clearTimer(trailing);
          trailing = null;
        }
        period += 1;
        owed = false;
        last = null;
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
