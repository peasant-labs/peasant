'use client';

import { useCallback, useEffect, useRef, useState } from 'react';
import {
  TRANSCRIPT_WINDOW_SPAN,
  TranscriptRevisionConflict,
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
  error: string | null;
  windowFault: { anchor: number; span: number } | null;
  reload: () => void;
  loadSegment: (index: number) => Promise<void>;
  loadWindow: (anchor: number, span?: number) => Promise<void>;
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
  const [error, setError] = useState<string | null>(null);
  const [windowFault, setWindowFault] = useState<{ anchor: number; span: number } | null>(null);
  const [reloadNonce, setReloadNonce] = useState(0);
  const cacheRef = useRef<RegionCache | null>(null);
  const inflight = useRef(new Map<string, Promise<void>>());
  const opened = useRef<OpenedJump>({ sessionId: '', opened: false, anchor: null });
  const generation = useRef(0);

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
      setError(null);
      setWindowFault(null);
      return undefined;
    }
    const controller = new AbortController();
    const ticket = generation.current + 1;
    generation.current = ticket;
    setLoading(true);
    setError(null);
    if (cacheRef.current?.sessionId !== sessionId || cacheRef.current.frameRevision !== frameRevision) {
      setWindowFault(null);
    }
    const revisionChanged = cacheRef.current?.sessionId === sessionId
      && cacheRef.current.frameRevision !== frameRevision;
    const anchor = revisionChanged && opened.current.opened
      ? opened.current.anchor
      : jumpAnchor;

    const run = async () => {
      try {
        let current = cacheRef.current;
        const sameCut = current?.sessionId === sessionId && current.frameRevision === frameRevision && !revisionChanged;
        if (!sameCut || !current) {
          current = await replaceDirectory(controller.signal, ticket);
        }
        if (!current || generation.current !== ticket || controller.signal.aborted) return;
        if (current.directory.totalSegments === 0) return;
        if (anchor != null) {
          const descriptor = await loadWindowInto(current, anchor, TRANSCRIPT_WINDOW_SPAN, controller.signal);
          if (descriptor) await coverJump(descriptor, controller.signal);
          return;
        }
        if (!current.segments.has(0)) await loadSegmentInto(current, 0, controller.signal);
      } catch (caught) {
        if (controller.signal.aborted || generation.current !== ticket) return;
        if (caught instanceof TranscriptRevisionConflict) {
          try {
            const refreshed = await replaceDirectory(controller.signal, ticket);
            if (!refreshed || generation.current !== ticket || controller.signal.aborted) return;
            if (anchor != null) {
              const descriptor = await loadWindowInto(refreshed, anchor, TRANSCRIPT_WINDOW_SPAN, controller.signal);
              if (descriptor) await coverJump(descriptor, controller.signal);
            } else if (refreshed.directory.totalSegments > 0) {
              await loadSegmentInto(refreshed, 0, controller.signal);
            }
            return;
          } catch (retryError) {
            if (controller.signal.aborted || generation.current !== ticket) return;
            publish(null);
            setError(retryError instanceof Error ? retryError.message : String(retryError));
            return;
          }
        }
        publish(null);
        setError(caught instanceof Error ? caught.message : String(caught));
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
  ]);

  const loadSegment = useCallback(async (index: number) => {
    const current = cacheRef.current;
    if (!current || current.segments.has(index)) return;
    await loadSegmentInto(current, index);
  }, [loadSegmentInto]);

  const loadWindow = useCallback(async (anchor: number, span: number = TRANSCRIPT_WINDOW_SPAN) => {
    const current = cacheRef.current;
    if (!current) throw new Error('The transcript directory is unavailable. Reload the transcript and try again.');
    const key = windowCacheKey(anchor, span);
    const ticket = generation.current;
    try {
      const descriptor = await loadWindowInto(current, anchor, span);
      if (!descriptor) throw new Error('The transcript changed while the window was loading. Reload the transcript and try again.');
      await loadListed(descriptor);
      if (generation.current === ticket && cacheRef.current?.revision === current.revision) {
        setWindowFault((fault) => fault?.anchor === anchor && fault.span === span ? null : fault);
      }
    } catch (caught) {
      const live = cacheRef.current;
      if (generation.current === ticket && live?.sessionId === current.sessionId && live.revision === current.revision) {
        const windows = new Map(live.windows);
        windows.delete(key);
        publish({ ...live, windows, activeWindowKey: live.activeWindowKey === key ? null : live.activeWindowKey });
        setWindowFault({ anchor, span });
      }
      throw caught;
    }
  }, [loadListed, loadWindowInto, publish]);

  const live = cache?.sessionId === sessionId ? cache : null;
  return {
    directory: live?.directory ?? null,
    segments: live ? [...live.segments.values()].sort((left, right) => left.index - right.index) : [],
    window: live?.activeWindowKey ? live.windows.get(live.activeWindowKey) ?? null : null,
    loading,
    error,
    windowFault,
    reload: () => {
      cacheRef.current = null;
      inflight.current.clear();
      setWindowFault(null);
      setReloadNonce((value) => value + 1);
    },
    loadSegment,
    loadWindow,
  };
}

function anchorKey(anchor: number | null): string {
  return anchor == null ? 'none' : String(anchor);
}
