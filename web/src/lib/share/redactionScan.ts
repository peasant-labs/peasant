'use client';

import { useCallback, useEffect, useMemo, useRef } from 'react';
import { fetchMockRedactionPreview } from '@/lib/share/mock-data';
import type { MockRedaction } from '@/lib/session-detail/mock-redactions';
import { fetchRedactionPreview, type RedactionLevel } from '@/lib/share/redactions';
import type { Redaction } from '@/types/messages';

// ---------------------------------------------------------------------------
// The local redaction scan behind every publish surface: a real `redact.Detect`
// per session (GET /sync/redactions), or the deterministic mock when the page
// runs in mock mode.
//
// Results are cached by (level, session). A success and an honest failure are
// both kept, so leaving and returning — to a wizard step, or to a transcript
// and its publish popup — neither repeats the work nor turns a failed scan into
// an all-clear. An entry that is still scanning is kept too, so returning
// during a scan waits for it instead of starting a second one. Only an explicit
// re-scan replaces a kept entry.
// ---------------------------------------------------------------------------

// The engine's rule-activation order, used to decide which mock rules a level
// would fire. It is NOT a menu: SELECTABLE_REDACTION_LEVELS is.
const REDACTION_LEVEL_ORDER = {
  minimal: 0,
  standard: 1,
  maximum: 2,
} as const satisfies Record<RedactionLevel, number>;

/** Apply the fixture's rule-level minimum (the real endpoint filters server-side). */
function filterMockByLevel(items: MockRedaction[], level: RedactionLevel): Redaction[] {
  return items.filter(
    (item) => REDACTION_LEVEL_ORDER[item.minimumLevel] <= REDACTION_LEVEL_ORDER[level],
  );
}

/** Cache key: a session's scan result is unique per (level, session). */
export function redactionCacheKey(level: RedactionLevel, sessionId: string): string {
  return `${level}:${sessionId}`;
}

/**
 * One cached scan. `version` names the content the scan read, when the caller
 * knows it (the transcript page passes its turn count): a successful scan of an
 * older version is re-run on the next automatic scan, because the session grew
 * after it. A failure is kept whatever the version, so only an explicit re-scan
 * retries it.
 */
export type RedactionCacheEntry =
  | { status: 'scanning'; version?: string }
  | { status: 'success'; redactions: Redaction[]; version?: string }
  | { status: 'failure'; error: string; version?: string };

export type RedactionCache = Map<string, RedactionCacheEntry>;

export type RedactionCacheUpdate = (updater: (prev: RedactionCache) => RedactionCache) => void;

/** A session to scan, with the version of its content when the caller knows it. */
export interface RedactionScanTarget {
  id: string;
  version?: string;
}

/** Whether a cached entry still stands for the target's current content. */
function entryIsCurrent(entry: RedactionCacheEntry | undefined, target: RedactionScanTarget): boolean {
  if (!entry) return false;
  if (entry.status !== 'success') return true;
  return entry.version === undefined || target.version === undefined || entry.version === target.version;
}

export interface RedactionPipeline {
  phase: 'scanning' | 'ready';
  scanProgress: number;
  scannedCount: number;
  failureCount: number;
  /** Successful results for the current level, by session id. */
  sessionRedactions: Map<string, Redaction[]>;
  /** The first failure's message, or null when no session failed. */
  scanError: string | null;
  /** Scan the targets; `force` replaces kept entries (the explicit re-scan). */
  runScan: (force?: boolean) => void;
}

/**
 * Scan the targets at the level, reading and writing the given cache. The
 * pipeline scans automatically whatever the cache does not hold (or holds only
 * for older content), and never scans a target whose scan is in flight.
 */
export function useRedactionPipeline(
  targets: RedactionScanTarget[],
  redactionLevel: RedactionLevel,
  useMock: boolean,
  cache: RedactionCache,
  onCacheChange: RedactionCacheUpdate,
): RedactionPipeline {
  const allSettled =
    targets.length === 0 ||
    targets.every((target) => {
      const entry = cache.get(redactionCacheKey(redactionLevel, target.id));
      return entry != null && entry.status !== 'scanning' && entryIsCurrent(entry, target);
    });
  const hasInFlight = targets.some(
    (target) => cache.get(redactionCacheKey(redactionLevel, target.id))?.status === 'scanning',
  );
  const scannedCount = targets.filter(
    (target) => cache.get(redactionCacheKey(redactionLevel, target.id))?.status === 'success',
  ).length;
  const failureCount = targets.filter(
    (target) => cache.get(redactionCacheKey(redactionLevel, target.id))?.status === 'failure',
  ).length;
  const scanProgress = targets.length === 0 ? 1 : scannedCount / targets.length;

  const sessionRedactions = useMemo(() => {
    const map = new Map<string, Redaction[]>();
    for (const target of targets) {
      const cached = cache.get(redactionCacheKey(redactionLevel, target.id));
      if (cached?.status === 'success') map.set(target.id, cached.redactions);
    }
    return map;
  }, [targets, redactionLevel, cache]);

  const scanError = useMemo(() => {
    for (const target of targets) {
      const cached = cache.get(redactionCacheKey(redactionLevel, target.id));
      if (cached?.status === 'failure') return cached.error;
    }
    return null;
  }, [targets, redactionLevel, cache]);

  // Latest values for use inside the async scan loop without re-creating it.
  const cacheRef = useRef(cache);
  cacheRef.current = cache;
  const activeScanRef = useRef<string | null>(null);

  const runScan = useCallback(
    (force = false) => {
      if (targets.length === 0) return;

      const contextKey = `${redactionLevel}:${targets.map((target) => `${target.id}@${target.version ?? ''}`).join(',')}`;
      if (activeScanRef.current === contextKey) return;
      activeScanRef.current = contextKey;

      const pending = targets.filter((target) => {
        if (force) return true;
        const entry = cacheRef.current.get(redactionCacheKey(redactionLevel, target.id));
        return !entry || (entry.status !== 'scanning' && !entryIsCurrent(entry, target));
      });
      if (pending.length === 0) {
        activeScanRef.current = null;
        return;
      }

      onCacheChange((prev) => {
        const next = new Map(prev);
        for (const target of pending) {
          next.set(redactionCacheKey(redactionLevel, target.id), { status: 'scanning', version: target.version });
        }
        return next;
      });

      (async () => {
        for (const target of pending) {
          const key = redactionCacheKey(redactionLevel, target.id);
          try {
            const items = useMock
              ? filterMockByLevel(
                  fetchMockRedactionPreview(target.id).map((r) => ({ ...r, status: 'pending' as const })),
                  redactionLevel,
                )
              : await fetchRedactionPreview(target.id, redactionLevel);
            onCacheChange((prev) =>
              new Map(prev).set(key, { status: 'success', redactions: items, version: target.version }),
            );
          } catch (e: unknown) {
            const error = e instanceof Error ? e.message : String(e);
            onCacheChange((prev) =>
              new Map(prev).set(key, { status: 'failure', error, version: target.version }),
            );
          }
        }
        if (activeScanRef.current === contextKey) activeScanRef.current = null;
      })();
    },
    [targets, redactionLevel, useMock, onCacheChange],
  );

  // The review has no truthful idle state. Auto-scan what the cache does not
  // hold, then reuse the cache on revisit; this never shows a false empty result
  // while waiting for work that has not run.
  useEffect(() => {
    if (allSettled) return;
    if (hasInFlight) return;
    runScan(false);
  }, [allSettled, hasInFlight, runScan]);

  return {
    phase: allSettled ? 'ready' : 'scanning',
    scanProgress,
    scannedCount,
    failureCount,
    sessionRedactions,
    scanError,
    runScan,
  };
}
