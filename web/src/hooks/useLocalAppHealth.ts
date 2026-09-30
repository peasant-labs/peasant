'use client';

import { useCallback, useEffect, useRef, useState } from 'react';
import { useConnectionState } from '@/contexts/WebSocketContext';
import { getApiBaseUrl } from '@/lib/api/base';

/** The local app's existing health route. A 2xx answer means the app is running. */
export const LOCAL_HEALTH_ROUTE = '/api/v1/health';
/**
 * How long the socket may stay down while the app still answers its health
 * route before the page calls the app stopped. It covers the first connect and
 * a reconnect blip, so neither flashes the notice.
 */
export const SOCKET_GRACE_MS = 2000;
/** How often the page checks again while the app looks stopped. */
export const OFFLINE_RECHECK_MS = 5000;
/** A health check that has not answered by then counts as failed. */
export const HEALTH_TIMEOUT_MS = 3000;

export interface LocalAppHealth {
  /** The local app is not reachable: its health route failed or the socket stayed down. */
  offline: boolean;
  /** When the page last asked the health route, or null before the first check. */
  checkedAt: Date | null;
  /** A `try again` check is in flight. */
  retrying: boolean;
  /**
   * Check now; when the app answers, the socket reconnects at once. Resolves to
   * whether it answered, or null when a newer check made the answer moot.
   */
  retry: () => Promise<boolean | null>;
}

async function healthAnswers(): Promise<boolean> {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), HEALTH_TIMEOUT_MS);
  try {
    const response = await fetch(`${getApiBaseUrl()}${LOCAL_HEALTH_ROUTE}`, { cache: 'no-store', signal: controller.signal });
    return response.ok;
  } catch {
    return false;
  } finally {
    clearTimeout(timer);
  }
}

/**
 * Whether the peasant app on this computer is reachable, from the two signals
 * the page already has: the WebSocket and `GET /api/v1/health`. Adds no backend.
 *
 * - The socket drops → check the health route at once. No answer means the app
 *   stopped, and the page says so right away.
 * - The route answers but the socket stays down past SOCKET_GRACE_MS → the page
 *   can no longer update, and says so too. A blip reconnects inside the grace
 *   through the provider's own backoff, so it shows nothing.
 * - While offline the page checks again every OFFLINE_RECHECK_MS (and on
 *   `try again`). When the route answers, the socket reconnects at once instead
 *   of waiting out its backoff. Past the grace the notice stays until the
 *   socket is back; inside it, an answer alone clears the notice (the app is
 *   running, and the socket is reconnecting).
 * - A connected socket always wins: the app is evidently running.
 */
export function useLocalAppHealth(): LocalAppHealth {
  const { connected, reconnect } = useConnectionState();
  const [socketDown, setSocketDown] = useState(false);
  const [healthFailed, setHealthFailed] = useState(false);
  const [checkedAt, setCheckedAt] = useState<Date | null>(null);
  const [retrying, setRetrying] = useState(false);
  // The newest check started. An older check that answers late (a timeout from
  // an earlier drop) must not overwrite what a newer one found.
  const latestCheck = useRef(0);

  // Only the offline paths reconnect on an answer. A fresh drop leaves the
  // reconnect to the provider's backoff, so a socket that opens and closes at
  // once cannot be retried faster than that backoff.
  const check = useCallback(
    async (reconnectIfAnswered: boolean) => {
      const id = ++latestCheck.current;
      const answered = await healthAnswers();
      if (id !== latestCheck.current) return null;
      setCheckedAt(new Date());
      setHealthFailed(!answered);
      if (answered && reconnectIfAnswered) reconnect();
      return answered;
    },
    [reconnect],
  );

  useEffect(() => {
    setSocketDown(false);
    setHealthFailed(false);
    if (connected) return;
    void check(false);
    const grace = setTimeout(() => setSocketDown(true), SOCKET_GRACE_MS);
    return () => clearTimeout(grace);
  }, [connected, check]);

  const offline = !connected && (socketDown || healthFailed);

  useEffect(() => {
    if (!offline) return;
    const recheck = setInterval(() => void check(true), OFFLINE_RECHECK_MS);
    return () => clearInterval(recheck);
  }, [offline, check]);

  const retry = useCallback(() => {
    setRetrying(true);
    return check(true).finally(() => setRetrying(false));
  }, [check]);

  return { offline, checkedAt, retrying, retry };
}
