'use client';

import { createContext, useCallback, useContext, useMemo, useRef, useState, type ReactNode } from 'react';
import type { SyncPushRequest, SyncPushResponse } from '@peasant-labs/schema';
import type { RedactionCache, RedactionCacheUpdate } from '@/lib/share/redactionScan';
import { publishSessions } from '@/lib/share/publishing';

/**
 * The publish state that outlives a page: the redaction scan cache and the
 * publishes in flight.
 *
 * The scan cache is keyed by (level, session) and keeps successes, honest
 * failures, and scans still running. Living above the pages, it lets a reader
 * close the publish popup, leave the transcript, or walk the share wizard's
 * steps and come back without starting a second scan or turning a failed scan
 * into an all-clear.
 *
 * A publish is one request; it keeps running when its page unmounts. The
 * in-flight mark lives here so a transcript that is left and reopened while it
 * runs still says it is publishing and offers no second publish, and the
 * settled count tells the page to read the publication state again.
 */
export interface PublishState {
  redactionCache: RedactionCache;
  updateRedactionCache: RedactionCacheUpdate;
  /** Sessions whose publish is running, with how many collectives it publishes to. */
  publishing: ReadonlyMap<string, number>;
  /** How many publishes of each session have finished, success or not. */
  settled: ReadonlyMap<string, number>;
  /** Publish one session. The promise settles with the server's answer. */
  publish: (sessionId: string, body: SyncPushRequest, collectives: number) => Promise<SyncPushResponse>;
}

const PublishContext = createContext<PublishState | null>(null);

function usePublishStateStore(): PublishState {
  const [redactionCache, setRedactionCache] = useState<RedactionCache>(() => new Map());
  const [publishing, setPublishing] = useState<ReadonlyMap<string, number>>(() => new Map());
  const [settled, setSettled] = useState<ReadonlyMap<string, number>>(() => new Map());
  const running = useRef(new Map<string, Promise<SyncPushResponse>>());

  const publish = useCallback((sessionId: string, body: SyncPushRequest, collectives: number) => {
    const existing = running.current.get(sessionId);
    if (existing) return existing;
    setPublishing((prev) => new Map(prev).set(sessionId, collectives));
    const run = publishSessions(body).finally(() => {
      running.current.delete(sessionId);
      setPublishing((prev) => {
        const next = new Map(prev);
        next.delete(sessionId);
        return next;
      });
      setSettled((prev) => new Map(prev).set(sessionId, (prev.get(sessionId) ?? 0) + 1));
    });
    running.current.set(sessionId, run);
    return run;
  }, []);

  return useMemo(
    () => ({ redactionCache, updateRedactionCache: setRedactionCache, publishing, settled, publish }),
    [redactionCache, publishing, settled, publish],
  );
}

/** Mounted once in the app shell, above every page. */
export function PublishProvider({ children }: { children: ReactNode }) {
  const state = usePublishStateStore();
  return <PublishContext.Provider value={state}>{children}</PublishContext.Provider>;
}

/**
 * The app-level publish state. A component mounted without the provider (a
 * bare test mount) gets its own state instead, scoped to its own lifetime, which
 * is the wizard's behavior before the cache was lifted.
 */
export function usePublishState(): PublishState {
  const shared = useContext(PublishContext);
  const local = usePublishStateStore();
  return shared ?? local;
}
