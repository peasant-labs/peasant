'use client';

import { useCallback, useEffect, useRef, useState } from 'react';
import { useConnectionState } from '@/contexts/WebSocketContext';

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
  /** Check now; when the app answers, the socket reconnects at once. */
  retry: () => void;
}

async function healthAnswers(): Promise<boolean> {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), HEALTH_TIMEOUT_MS);
  try {
    const response = await fetch(LOCAL_HEALTH_ROUTE, { cache: 'no-store', signal: controller.signal });
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
 *   can no longer update, and says so too.
 * - While offline the page checks again every OFFLINE_RECHECK_MS. When the route
 *   answers, the socket reconnects at once instead of waiting out its backoff;
 *   the notice clears when the socket is back.
 */
export function useLocalAppHealth(): LocalAppHealth {
  const { connected, reconnect } = useConnectionState();
  const [socketDown, setSocketDown] = useState(false);
  const [healthFailed, setHealthFailed] = useState(false);
  const [checkedAt, setCheckedAt] = useState<Date | null>(null);
  const [retrying, setRetrying] = useState(false);
  const mounted = useRef(false);
  const connectedRef = useRef(connected);
  connectedRef.current = connected;

  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);

  const check = useCallback(async () => {
    const answered = await healthAnswers();
    if (!mounted.current) return;
    setCheckedAt(new Date());
    // A socket that connected while the check was in flight wins: the app is
    // evidently running, so a late failure must not raise the notice.
    setHealthFailed(!answered && !connectedRef.current);
    if (answered) reconnect();
  }, [reconnect]);

  useEffect(() => {
    if (connected) {
      setSocketDown(false);
      setHealthFailed(false);
      return;
    }
    void check();
    const grace = setTimeout(() => setSocketDown(true), SOCKET_GRACE_MS);
    return () => clearTimeout(grace);
  }, [connected, check]);

  const offline = socketDown || healthFailed;

  useEffect(() => {
    if (!offline) return;
    const recheck = setInterval(() => void check(), OFFLINE_RECHECK_MS);
    return () => clearInterval(recheck);
  }, [offline, check]);

  const retry = useCallback(() => {
    setRetrying(true);
    void check().finally(() => {
      if (mounted.current) setRetrying(false);
    });
  }, [check]);

  return { offline, checkedAt, retrying, retry };
}
