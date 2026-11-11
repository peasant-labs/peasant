'use client';

import { useCallback, useEffect, useReducer, useRef, useState } from 'react';
import { FaultOperation, FaultStore, WebFault } from '@/lib/faults';
import {
  TRANSCRIPT_WINDOW_SPAN,
  TranscriptRevisionConflict,
  TranscriptSegmentReadError,
  transcriptSegments,
  windowCacheKey,
  type RegionCache,
  type TranscriptSegment,
  type TranscriptSegmentDirectory,
  type TranscriptSegmentTransport,
  type TranscriptWindow,
} from './transcriptSegments';

export type TranscriptSegmentState = {
  directory: TranscriptSegmentDirectory | null;
  segments: TranscriptSegment[];
  window: TranscriptWindow | null;
  loading: boolean;
  initialFault: WebFault | null;
  windowFaults: WebFault[];
  hostWindowFaults: WebFault[];
  reload: () => void;
  loadSegment: (index: number) => Promise<void>;
  loadWindow: (anchor: number, span?: number) => Promise<void>;
  retryHostWindow: (anchor: number, span?: number) => Promise<void>;
};

type OpenedJump = {
  sessionId: string;
  opened: boolean;
  anchor: number | null;
};

/**
 * Loads the transcript as a directory plus the segments the reader has asked
 * for. A mount with no jump reads segment 0. A route opened on a turn reads
 * the window descriptor and exactly the segments it lists. A newer frame
 * revision drops the cache and reads that plan again.
 */
export function useTranscriptSegments({
  sessionId,
  enabled,
  frameRevision,
  jumpAnchor,
  transport = transcriptSegments,
}: {
  sessionId: string;
  enabled: boolean;
  frameRevision: string;
  jumpAnchor: number | null;
  transport?: TranscriptSegmentTransport;
}): TranscriptSegmentState {
  const [cache, setCache] = useState<RegionCache | null>(null);
  const [loading, setLoading] = useState(false);
  const faultStore = useRef(new FaultStore());
  // Associate ownership with the fault object itself, so replacing or clearing a
  // keyed fault cannot leave stale ownership behind.
  const viewerFaults = useRef(new WeakSet<WebFault>());
  const [, notifyFaults] = useReducer((value: number) => value + 1, 0);
  const [reloadNonce, setReloadNonce] = useState(0);
  const cacheRef = useRef<RegionCache | null>(null);
  const inflight = useRef(new Map<string, Promise<void>>());
  const opened = useRef<OpenedJump>({ sessionId: '', opened: false, anchor: null });
  const generation = useRef(0);
  const requestKey = JSON.stringify([sessionId, frameRevision]);
  const windowKeyPrefix = `${JSON.stringify([sessionId]).slice(0, -1)},`;

  const clearFault = useCallback((operation: FaultOperation, key: string) => {
    if (faultStore.current.clear(operation, key)) notifyFaults();
  }, []);
  const recordFault = useCallback((operation: FaultOperation, key: string, fault: WebFault) => {
    if (faultStore.current.set(operation, key, fault)) notifyFaults();
  }, []);
  const recordWindowFault = useCallback((key: string, anchor: number, span: number, caught: unknown, viewer: boolean) => {
    const fault = WebFault.transcriptWindowRead(anchor, span, readFailureDetail(caught));
    if (viewer) viewerFaults.current.add(fault);
    recordFault(FaultOperation.TranscriptWindowRead, key, fault);
  }, [recordFault]);

  if (opened.current.sessionId !== sessionId) {
    opened.current = { sessionId, opened: jumpAnchor != null, anchor: jumpAnchor };
  }

  const publish = useCallback((next: RegionCache | null) => {
    cacheRef.current = next;
    setCache(next);
  }, []);

  const dropCache = useCallback(() => {
    inflight.current.clear();
    publish(null);
  }, [publish]);

  const replaceDirectory = useCallback(async (signal: AbortSignal, ticket: number): Promise<RegionCache | null> => {
    dropCache();
    const directory = await transport.directory(sessionId, signal);
    if (signal.aborted || generation.current !== ticket) return null;
    const next: RegionCache = {
      sessionId,
      frameRevision,
      revision: directory.revision,
      directory,
      segments: new Map(),
      windows: new Map(),
      activeWindowKey: null,
    };
    publish(next);
    return next;
  }, [dropCache, frameRevision, publish, sessionId, transport]);

  const loadSegmentInto = useCallback(async (current: RegionCache, index: number, signal?: AbortSignal) => {
    if (current.segments.has(index)) return;
    const key = `${current.revision}:segment:${index}`;
    const pending = inflight.current.get(key);
    if (pending) return pending;
    const promise = transport.segment(sessionId, index, current.revision, signal).then((segment) => {
      const live = cacheRef.current;
      if (!live || live.revision !== segment.revision || live.sessionId !== sessionId) return;
      const segments = new Map(live.segments);
      segments.set(segment.index, segment);
      publish({ ...live, segments });
    }).finally(() => {
      if (inflight.current.get(key) === promise) inflight.current.delete(key);
    });
    inflight.current.set(key, promise);
    return promise;
  }, [publish, sessionId, transport]);

  const loadWindowInto = useCallback(async (current: RegionCache, anchor: number, span: number, signal?: AbortSignal) => {
    const keyName = windowCacheKey(anchor, span);
    const cached = current.windows.get(keyName);
    if (cached) {
      if (current.activeWindowKey !== keyName) publish({ ...current, activeWindowKey: keyName });
      return cached;
    }
    const key = `${current.revision}:window:${keyName}`;
    const pending = inflight.current.get(key);
    if (pending) {
      await pending;
      return cacheRef.current?.windows.get(keyName) ?? null;
    }
    const promise = transport.window(sessionId, anchor, span, current.revision, signal).then((descriptor) => {
      const live = cacheRef.current;
      if (!live || live.revision !== descriptor.revision || live.sessionId !== sessionId) return;
      const windows = new Map(live.windows);
      windows.set(keyName, descriptor);
      publish({ ...live, windows, activeWindowKey: keyName });
    }).finally(() => {
      if (inflight.current.get(key) === promise) inflight.current.delete(key);
    });
    inflight.current.set(key, promise);
    await promise;
    return cacheRef.current?.windows.get(windowCacheKey(anchor, span)) ?? null;
  }, [publish, sessionId, transport]);

  const loadListed = useCallback(async (descriptor: TranscriptWindow, signal?: AbortSignal) => {
    const live = cacheRef.current;
    if (!live) return;
    for (const index of descriptor.segments) {
      await loadSegmentInto(live, index, signal);
    }
  }, [loadSegmentInto]);

  // Cover exactly the descriptor's segments.
  const coverJump = useCallback(async (descriptor: TranscriptWindow, signal?: AbortSignal) => {
    await loadListed(descriptor, signal);
  }, [loadListed]);

  useEffect(() => {
    if (!enabled) {
      publish(null);
      setLoading(false);
      return undefined;
    }
    const controller = new AbortController();
    const ticket = generation.current + 1;
    generation.current = ticket;
    setLoading(true);
    const revisionChanged = cacheRef.current?.sessionId === sessionId
      && cacheRef.current.frameRevision !== frameRevision;
    const anchor = revisionChanged && opened.current.opened
      ? opened.current.anchor
      : jumpAnchor;

    const run = async () => {
      let coveringWindow = false;
      const failRead = (caught: unknown) => {
        const live = cacheRef.current;
        if (coveringWindow && anchor != null && live?.sessionId === sessionId) {
          const key = windowCacheKey(anchor, TRANSCRIPT_WINDOW_SPAN);
          const windows = new Map(live.windows);
          windows.delete(key);
          publish({ ...live, windows, activeWindowKey: live.activeWindowKey === key ? null : live.activeWindowKey });
          clearFault(FaultOperation.TranscriptInitialRead, requestKey);
          recordWindowFault(`${windowKeyPrefix}${anchor},${TRANSCRIPT_WINDOW_SPAN}]`, anchor, TRANSCRIPT_WINDOW_SPAN, caught, false);
          return;
        }
        publish(null);
        recordFault(FaultOperation.TranscriptInitialRead, requestKey, WebFault.transcriptInitialRead(readFailureDetail(caught)));
      };
      try {
        let current = cacheRef.current;
        const sameCut = current?.sessionId === sessionId && current.frameRevision === frameRevision && !revisionChanged;
        if (!sameCut || !current) {
          current = await replaceDirectory(controller.signal, ticket);
        }
        if (!current || generation.current !== ticket || controller.signal.aborted) return;
        if (current.directory.totalSegments === 0) {
          clearFault(FaultOperation.TranscriptInitialRead, requestKey);
          return;
        }
        if (anchor != null) {
          coveringWindow = true;
          const descriptor = await loadWindowInto(current, anchor, TRANSCRIPT_WINDOW_SPAN, controller.signal);
          if (descriptor) await coverJump(descriptor, controller.signal);
          if (descriptor) clearFault(FaultOperation.TranscriptWindowRead, `${windowKeyPrefix}${anchor},${TRANSCRIPT_WINDOW_SPAN}]`);
          clearFault(FaultOperation.TranscriptInitialRead, requestKey);
          return;
        }
        if (!current.segments.has(0)) await loadSegmentInto(current, 0, controller.signal);
        clearFault(FaultOperation.TranscriptInitialRead, requestKey);
      } catch (caught) {
        if (controller.signal.aborted || generation.current !== ticket) return;
        if (caught instanceof TranscriptRevisionConflict) {
          try {
            const refreshed = await replaceDirectory(controller.signal, ticket);
            if (!refreshed || generation.current !== ticket || controller.signal.aborted) return;
            if (anchor != null) {
              coveringWindow = true;
              const descriptor = await loadWindowInto(refreshed, anchor, TRANSCRIPT_WINDOW_SPAN, controller.signal);
              if (descriptor) await coverJump(descriptor, controller.signal);
              if (descriptor) clearFault(FaultOperation.TranscriptWindowRead, `${windowKeyPrefix}${anchor},${TRANSCRIPT_WINDOW_SPAN}]`);
            } else if (refreshed.directory.totalSegments > 0) {
              await loadSegmentInto(refreshed, 0, controller.signal);
            }
            clearFault(FaultOperation.TranscriptInitialRead, requestKey);
            return;
          } catch (retryError) {
            if (controller.signal.aborted || generation.current !== ticket) return;
            failRead(retryError);
            return;
          }
        }
        failRead(caught);
      } finally {
        if (generation.current === ticket) setLoading(false);
      }
    };
    void run();
    return () => controller.abort();
  }, [
    anchorKey(jumpAnchor),
    enabled,
    frameRevision,
    jumpAnchor,
    coverJump,
    loadListed,
    loadSegmentInto,
    loadWindowInto,
    reloadNonce,
    replaceDirectory,
    sessionId,
    clearFault,
    recordFault,
    recordWindowFault,
    requestKey,
    windowKeyPrefix,
  ]);

  const loadSegment = useCallback(async (index: number) => {
    const current = cacheRef.current;
    if (!current || current.segments.has(index)) return;
    await loadSegmentInto(current, index);
  }, [loadSegmentInto]);

  const requestWindow = useCallback(async (anchor: number, span: number, viewer: boolean) => {
    const current = cacheRef.current;
    if (!current) throw new Error('The transcript directory is unavailable. Reload the transcript and try again.');
    const key = windowCacheKey(anchor, span);
    const faultKey = `${windowKeyPrefix}${anchor},${span}]`;
    const ticket = generation.current;
    try {
      const descriptor = await loadWindowInto(current, anchor, span);
      if (!descriptor) throw new Error('The transcript changed while the window was loading. Reload the transcript and try again.');
      await loadListed(descriptor);
      if (generation.current === ticket && cacheRef.current?.revision === current.revision) {
        clearFault(FaultOperation.TranscriptWindowRead, faultKey);
      }
    } catch (caught) {
      const live = cacheRef.current;
      if (generation.current === ticket && live?.sessionId === current.sessionId && live.revision === current.revision) {
        const windows = new Map(live.windows);
        windows.delete(key);
        publish({ ...live, windows, activeWindowKey: live.activeWindowKey === key ? null : live.activeWindowKey });
        recordWindowFault(faultKey, anchor, span, caught, viewer);
      }
      throw caught;
    }
  }, [clearFault, loadListed, loadWindowInto, publish, recordWindowFault, windowKeyPrefix]);

  const loadWindow = useCallback((anchor: number, span = TRANSCRIPT_WINDOW_SPAN) => requestWindow(anchor, span, true), [requestWindow]);
  const retryHostWindow = useCallback((anchor: number, span = TRANSCRIPT_WINDOW_SPAN) => requestWindow(anchor, span, false), [requestWindow]);

  const live = cache?.sessionId === sessionId ? cache : null;
  const windowFaults = faultStore.current.forOperation(FaultOperation.TranscriptWindowRead, windowKeyPrefix);
  return {
    directory: live?.directory ?? null,
    segments: live ? [...live.segments.values()].sort((left, right) => left.index - right.index) : [],
    window: live?.activeWindowKey ? live.windows.get(live.activeWindowKey) ?? null : null,
    loading,
    initialFault: faultStore.current.get(FaultOperation.TranscriptInitialRead, requestKey),
    windowFaults,
    hostWindowFaults: windowFaults.filter((fault) => !viewerFaults.current.has(fault)),
    reload: () => {
      cacheRef.current = null;
      inflight.current.clear();
      setReloadNonce((value) => value + 1);
    },
    loadSegment,
    loadWindow,
    retryHostWindow,
  };
}

function readFailureDetail(error: unknown): string {
  if (error instanceof TranscriptRevisionConflict) return 'the transcript changed during the read';
  if (error instanceof TranscriptSegmentReadError && error.status === 200) return 'the local server returned an unusable response';
  if (error instanceof TranscriptSegmentReadError && error.status > 0) return `the local server returned HTTP ${error.status}`;
  return 'the local server did not answer or returned an unusable response';
}

function anchorKey(anchor: number | null): string {
  return anchor == null ? 'none' : String(anchor);
}
