'use client';

import { useLayoutEffect, useRef, useState } from 'react';
import { fetchKickstartPreview } from './client';
import type { KickstartLine } from './model';

/**
 * The bounded typed preview of the forest row the reader is on. The server
 * builds the lines; this reads them once per row and keeps them for the life of
 * the page, so moving back to a completed row never asks again. Leaving a row
 * aborts its request and discards its answer even if the transport ignores abort.
 */
export interface KickstartPreview {
  readonly lines: readonly KickstartLine[] | null;
  readonly loading: boolean;
  readonly error: Error | null;
}

const EMPTY_PREVIEW: KickstartPreview = { lines: null, loading: false, error: null };
const LOADING_PREVIEW: KickstartPreview = { lines: null, loading: true, error: null };

export function useKickstartPreview(sessionId: string | null, nodeId: string | null): KickstartPreview {
  const cache = useRef(new Map<string, readonly KickstartLine[]>());
  const key = sessionId === null || nodeId === null ? null : `${sessionId}\u0000${nodeId}`;
  const [state, setState] = useState<{ key: string | null; preview: KickstartPreview }>({ key: null, preview: EMPTY_PREVIEW });

  // Cleanup runs during the selection commit, before a stale promise can resume
  // between that commit and a deferred passive effect.
  useLayoutEffect(() => {
    if (sessionId === null || nodeId === null || key === null) {
      setState({ key: null, preview: EMPTY_PREVIEW });
      return;
    }
    const cached = cache.current.get(key);
    if (cached !== undefined) {
      setState({ key, preview: { lines: cached, loading: false, error: null } });
      return;
    }
    let current = true;
    const controller = new AbortController();
    setState({ key, preview: LOADING_PREVIEW });
    fetchKickstartPreview(sessionId, nodeId, controller.signal)
      .then((lines) => {
        if (!current) return;
        cache.current.set(key, lines);
        setState({ key, preview: { lines, loading: false, error: null } });
      })
      .catch((failure: unknown) => {
        if (current) setState({ key, preview: { lines: null, loading: false, error: failure instanceof Error ? failure : new Error(String(failure)) } });
      });
    return () => {
      current = false;
      controller.abort();
    };
  }, [sessionId, nodeId, key]);

  // A render for a new selection must not expose the preceding row's result,
  // even before the effect has installed the new request or its cached value.
  return state.key === key ? state.preview : key === null ? EMPTY_PREVIEW : LOADING_PREVIEW;
}
