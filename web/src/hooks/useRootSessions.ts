'use client';

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { fetchPublications, fetchSyncSessions } from '@/lib/api/publications';
import { visibleRootRows, type RootSessionRow } from '@/lib/home/publishState';

export type RootSessionsState =
  | { status: 'loading'; rows: null; error: null }
  | { status: 'ready'; rows: RootSessionRow[]; error: null }
  | { status: 'error'; rows: null; error: unknown };

const LOADING: RootSessionsState = { status: 'loading', rows: null, error: null };

/**
 * The root page's session rows: the flat sync list joined with the publication
 * state of every session in it, keeping only what the publications read says
 * the saved selection shows.
 *
 * `invalidationKey` is any value that changes when the WebSocket sessions
 * channel delivers a new session set; the rows are read again then. While a
 * reread is in flight the last rows stay on screen. `enabled: false` (a
 * selection failure or recovery state owns the page) drops the rows and reads
 * nothing, so no stale row survives a fail-closed state.
 */
export function useRootSessions({
  enabled,
  invalidationKey,
}: {
  enabled: boolean;
  invalidationKey?: unknown;
}): RootSessionsState & { reload: () => void } {
  const [state, setState] = useState<RootSessionsState>(LOADING);
  const [reloadCount, setReloadCount] = useState(0);
  const reload = useCallback(() => setReloadCount((value) => value + 1), []);

  useEffect(() => {
    if (!enabled) {
      setState(LOADING);
      return;
    }
    let cancelled = false;
    (async () => {
      try {
        const sync = await fetchSyncSessions();
        const publications = await fetchPublications(sync.map((session) => session.id));
        if (!cancelled) setState({ status: 'ready', rows: visibleRootRows(sync, publications), error: null });
      } catch (error: unknown) {
        if (!cancelled) setState({ status: 'error', rows: null, error });
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [enabled, invalidationKey, reloadCount]);

  return { ...state, reload };
}

/**
 * How many collectives can read each of the named published sessions, read
 * from Village through the publications read (`include=audience`). Only the
 * rows on screen are asked for, and each session once. A failed read leaves
 * those sessions out, so their label states no count rather than a wrong one.
 */
export function usePublicationAudience(sessionIds: readonly string[]): ReadonlyMap<string, number> {
  const [counts, setCounts] = useState<ReadonlyMap<string, number>>(() => new Map());
  const asked = useRef(new Set<string>());
  const mounted = useRef(true);
  const key = useMemo(() => [...new Set(sessionIds)].sort().join(','), [sessionIds]);

  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);

  useEffect(() => {
    const missing = key === '' ? [] : key.split(',').filter((id) => !asked.current.has(id));
    if (missing.length === 0) return;
    for (const id of missing) asked.current.add(id);
    // An answer that arrives after the rows on screen changed still applies:
    // the counts are keyed by session, and each session is asked once.
    fetchPublications(missing, { audience: true })
      .then((publications) => {
        if (!mounted.current) return;
        setCounts((previous) => {
          const next = new Map(previous);
          for (const publication of publications) {
            if (publication.audience) next.set(publication.sessionId, publication.audience.filter((member) => member.status === 'approved').length);
          }
          return next;
        });
      })
      .catch(() => {
        // Village could not be read. The labels keep stating no count; the
        // next change of the rows on screen asks again.
        for (const id of missing) asked.current.delete(id);
      });
  }, [key]);

  return counts;
}
