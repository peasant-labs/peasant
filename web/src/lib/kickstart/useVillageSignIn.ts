'use client';

/**
 * The village sign-in the setup flow offers, on the routes the app already has:
 * `POST /api/v1/sync/login` opens the browser sign-in and answers at once, and
 * `GET /api/v1/sync/auth` says when it finished. The poll runs only while a
 * sign-in the person started is waiting; it is the one place this module polls,
 * because the sign-in is not on the live topic.
 */

import { useCallback, useEffect, useRef, useState } from 'react';
import { startVillageSignIn } from '@/lib/share/publishing';
import { SIGN_IN_POLL_MS, watchVillageSignIn } from '@/lib/share/villageSignIn';

/** How long the sign-in is given. The server waits three minutes. */
export const SIGN_IN_TIMEOUT_MS = 180_000;

export interface VillageSignIn {
  /** A sign-in is open in the browser and not yet finished. */
  readonly waiting: boolean;
  /** Why the last attempt did not sign in, or null. */
  readonly failure: string | null;
  /**
   * Start the sign-in. Resolves true once this computer is signed in, false
   * when the attempt failed (see `failure`) or was abandoned.
   */
  signIn(): Promise<boolean>;
  /** Stop waiting. The browser sign-in may still finish; it is not undone. */
  cancel(): void;
}

function messageOf(failure: unknown): string {
  return failure instanceof Error ? failure.message : String(failure);
}

export function useVillageSignIn(
  { pollMs = SIGN_IN_POLL_MS, timeoutMs = SIGN_IN_TIMEOUT_MS }: { pollMs?: number; timeoutMs?: number } = {},
): VillageSignIn {
  const [waiting, setWaiting] = useState(false);
  const [failure, setFailure] = useState<string | null>(null);
  const attempt = useRef(0);
  const watching = useRef<AbortController | null>(null);
  const alive = useRef(true);

  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
      attempt.current += 1;
      watching.current?.abort();
    };
  }, []);

  const cancel = useCallback(() => {
    attempt.current += 1;
    watching.current?.abort();
    watching.current = null;
    setWaiting(false);
  }, []);

  const signIn = useCallback(async (): Promise<boolean> => {
    attempt.current += 1;
    watching.current?.abort();
    const mine = attempt.current;
    const current = () => alive.current && mine === attempt.current;
    setFailure(null);
    setWaiting(true);
    try {
      await startVillageSignIn();
    } catch (error) {
      if (current()) {
        setFailure(messageOf(error));
        setWaiting(false);
      }
      return false;
    }
    if (!current()) return false;
    const watch = new AbortController();
    watching.current = watch;
    const outcome = await watchVillageSignIn({ signal: watch.signal, pollMs, timeoutMs, checkNow: true });
    if (!current()) return false;
    if (outcome === 'signed-in') {
      setWaiting(false);
      return true;
    }
    if (outcome === 'timed-out') setFailure('the sign-in did not finish in the browser within three minutes. try again.');
    setWaiting(false);
    return false;
  }, [pollMs, timeoutMs]);

  return { waiting, failure, signIn, cancel };
}
