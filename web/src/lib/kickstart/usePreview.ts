'use client';

import { useEffect, useRef, useState } from 'react';
import { fetchKickstartPreview } from './client';
import type { KickstartLine } from './model';

/**
 * The bounded typed preview of the forest row the reader is on. The server
 * builds the lines; this reads them once per row and keeps them for the life of
 * the page, so moving back to a row never asks again. A newer row cancels the
 * answer of an older one.
 */
export interface KickstartPreview {
  readonly lines: readonly KickstartLine[] | null;
  readonly loading: boolean;
  readonly error: Error | null;
}

export function useKickstartPreview(sessionId: string | null, nodeId: string | null): KickstartPreview {
  const cache = useRef(new Map<string, readonly KickstartLine[]>());
  const [state, setState] = useState<KickstartPreview>({ lines: null, loading: false, error: null });

  useEffect(() => {
    if (sessionId === null || nodeId === null) {
      setState({ lines: null, loading: false, error: null });
      return;
    }
    const key = `${sessionId}\u0000${nodeId}`;
    const cached = cache.current.get(key);
    if (cached !== undefined) {
      setState({ lines: cached, loading: false, error: null });
      return;
    }
    let current = true;
    setState({ lines: null, loading: true, error: null });
    fetchKickstartPreview(sessionId, nodeId)
      .then((lines) => {
        cache.current.set(key, lines);
        if (current) setState({ lines, loading: false, error: null });
      })
      .catch((failure: unknown) => {
        if (current) setState({ lines: null, loading: false, error: failure instanceof Error ? failure : new Error(String(failure)) });
      });
    return () => {
      current = false;
    };
  }, [sessionId, nodeId]);

  return state;
}
