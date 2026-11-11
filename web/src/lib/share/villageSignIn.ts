import { fetchVillageAuth } from './publishing';

/**
 * Waiting for the browser sign-in to land. `POST /api/v1/sync/login` opens the
 * sign-in and answers at once; whether it finished is only known by asking
 * `GET /api/v1/sync/auth`, because the sign-in is not on the live topic. The
 * publish popup and the setup flow both wait this way, so the loop lives here
 * once.
 */

/** How often the sign-in is checked. */
export const SIGN_IN_POLL_MS = 2000;

/** How a wait ended. */
export type SignInWatch = 'signed-in' | 'timed-out' | 'stopped';

export interface SignInWatchOptions {
  /** Aborting ends the wait and no more checks are made. */
  signal: AbortSignal;
  /** The gap between checks. */
  pollMs?: number;
  /** Give up after this long. Without it the wait ends only when the person is signed in or it is stopped. */
  timeoutMs?: number;
  /** Check once before the first wait, instead of only after it. */
  checkNow?: boolean;
}

/** Resolves after `ms`, or at once when `signal` aborts. */
function pause(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise<void>((resolve) => {
    if (signal.aborted) {
      resolve();
      return;
    }
    const done = () => {
      clearTimeout(timer);
      signal.removeEventListener('abort', done);
      resolve();
    };
    const timer = setTimeout(done, ms);
    signal.addEventListener('abort', done, { once: true });
  });
}

/**
 * Ask whether this computer is signed in, every `pollMs`, until it is, the wait
 * is stopped, or `timeoutMs` has passed. A check that fails is not a failed
 * sign-in (the browser flow still runs), so it is asked again.
 */
export async function watchVillageSignIn({ signal, pollMs = SIGN_IN_POLL_MS, timeoutMs, checkNow = false }: SignInWatchOptions): Promise<SignInWatch> {
  const deadline = timeoutMs === undefined ? Number.POSITIVE_INFINITY : Date.now() + timeoutMs;
  let wait = !checkNow;
  for (;;) {
    if (wait) await pause(pollMs, signal);
    if (signal.aborted) return 'stopped';
    wait = true;
    try {
      const auth = await fetchVillageAuth();
      if (signal.aborted) return 'stopped';
      if (auth.authenticated) return 'signed-in';
    } catch {
      // A missed check is not a failed sign-in: the browser flow still runs.
    }
    if (Date.now() >= deadline) return 'timed-out';
  }
}
