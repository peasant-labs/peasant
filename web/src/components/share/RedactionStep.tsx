'use client';

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { RedactionReview, Button } from '@/lib/ft-ui';
import { LoaderIcon } from 'lucide-react';
import type { ShareSession } from '@/lib/share/types';
import { fetchMockRedactionPreview } from '@/lib/share/mock-data';
import type { MockRedaction } from '@/lib/session-detail/mock-redactions';
import {
  fetchRedactionPreview,
  isSelectableRedactionLevel,
  unselectableRedactionLevelReason,
  SELECTABLE_REDACTION_LEVELS,
  type RedactionLevel,
  type SelectableRedactionLevel,
} from '@/lib/share/redactions';
import type { Redaction } from '@/types/messages';
import type { SetWizardFooterActions } from '@/app/share/ShareWizardClient';

// ---------------------------------------------------------------------------
// Redaction pipeline — REAL local scan per session (GET /sync/redactions), or
// the deterministic mock when the wizard runs in mock mode. Successful results
// and honest failures are cached by `${level}:${sessionId}` in the wizard, so
// leaving and returning to this step — or switching back to a scanned level —
// does not repeat the expensive work or turn a failed scan into an all-clear.
// ---------------------------------------------------------------------------

// The engine's rule-activation order, used to decide which mock rules a level
// would fire. It is NOT a menu: SELECTABLE_REDACTION_LEVELS is.
const REDACTION_LEVEL_ORDER = {
  minimal: 0,
  standard: 1,
  maximum: 2,
} as const satisfies Record<RedactionLevel, number>;

// The buttons this component renders. Derived from the offered set rather than
// from the order table above, which is why it is now one button: the local API
// answers 400 for the other two levels, so rendering them here handed the user a
// control that produced an opaque failure after they had committed to sharing.
const REDACTION_LEVELS: readonly RedactionLevel[] = SELECTABLE_REDACTION_LEVELS;

/** Apply the fixture's rule-level minimum (the real endpoint filters server-side). */
function filterMockByLevel(items: MockRedaction[], level: RedactionLevel): Redaction[] {
  return items.filter(
    (item) => REDACTION_LEVEL_ORDER[item.minimumLevel] <= REDACTION_LEVEL_ORDER[level],
  );
}

/** Cache key: a session's scan result is unique per (level, session). */
function cacheKey(level: RedactionLevel, sessionId: string): string {
  return `${level}:${sessionId}`;
}

export type RedactionCacheEntry =
  | { status: 'scanning' }
  | { status: 'success'; redactions: Redaction[] }
  | { status: 'failure'; error: string };

export type RedactionCache = Map<string, RedactionCacheEntry>;

function useRedactionPipeline(
  selectedSessions: ShareSession[],
  redactionLevel: RedactionLevel,
  useMock: boolean,
  cache: RedactionCache,
  onCacheChange: (updater: (prev: RedactionCache) => RedactionCache) => void,
) {
  // In-flight entries live above the conditionally mounted step too, preventing
  // duplicate work when a user navigates away during a scan and then returns.
  const allSettled =
    selectedSessions.length === 0 ||
    selectedSessions.every((session) => {
      const entry = cache.get(cacheKey(redactionLevel, session.id));
      return entry != null && entry.status !== 'scanning';
    });
  const hasInFlight = selectedSessions.some(
    (session) => cache.get(cacheKey(redactionLevel, session.id))?.status === 'scanning',
  );
  const scannedCount = selectedSessions.filter(
    (session) => cache.get(cacheKey(redactionLevel, session.id))?.status === 'success',
  ).length;
  const failureCount = selectedSessions.filter(
    (session) => cache.get(cacheKey(redactionLevel, session.id))?.status === 'failure',
  ).length;
  const scanProgress = selectedSessions.length === 0
    ? 1
    : scannedCount / selectedSessions.length;

  // Results for the current level, read straight from the lifted cache.
  const sessionRedactions = useMemo(() => {
    const map = new Map<string, Redaction[]>();
    for (const s of selectedSessions) {
      const cached = cache.get(cacheKey(redactionLevel, s.id));
      if (cached?.status === 'success') map.set(s.id, cached.redactions);
    }
    return map;
  }, [selectedSessions, redactionLevel, cache]);

  const scanError = useMemo(() => {
    for (const session of selectedSessions) {
      const cached = cache.get(cacheKey(redactionLevel, session.id));
      if (cached?.status === 'failure') return cached.error;
    }
    return null;
  }, [selectedSessions, redactionLevel, cache]);

  // Latest values for use inside the async scan loop without re-creating it.
  const cacheRef = useRef(cache);
  cacheRef.current = cache;
  const activeScanRef = useRef<string | null>(null);

  // Run the scan. `force` refreshes cached sessions; otherwise only missing
  // entries are fetched. Results are stored as success/failure discriminants so
  // the review remains honest across step unmounts.
  const runScan = useCallback(
    (force = false) => {
      const total = selectedSessions.length;
      if (total === 0) {
        return;
      }

      const contextKey = `${redactionLevel}:${selectedSessions.map((session) => session.id).join(',')}`;
      if (activeScanRef.current === contextKey) return;
      activeScanRef.current = contextKey;

      const targetKeys = selectedSessions
        .map((session) => cacheKey(redactionLevel, session.id))
        .filter((key) => force || !cacheRef.current.has(key));
      if (targetKeys.length === 0) {
        activeScanRef.current = null;
        return;
      }
      const targetKeySet = new Set(targetKeys);

      onCacheChange((prev) => {
        const next = new Map(prev);
        for (const key of targetKeys) next.set(key, { status: 'scanning' });
        return next;
      });

      (async () => {
        for (const session of selectedSessions) {
          const key = cacheKey(redactionLevel, session.id);
          if (!targetKeySet.has(key)) {
            continue;
          }
          try {
            const items = useMock
              ? filterMockByLevel(
                  fetchMockRedactionPreview(session.id).map((r) => ({ ...r, status: 'pending' as const })),
                  redactionLevel,
                )
              : await fetchRedactionPreview(session.id, redactionLevel);
            onCacheChange((prev) =>
              new Map(prev).set(key, { status: 'success', redactions: items }),
            );
          } catch (e: unknown) {
            const error = e instanceof Error ? e.message : String(e);
            onCacheChange((prev) =>
              new Map(prev).set(key, { status: 'failure', error }),
            );
          }
        }
        if (activeScanRef.current === contextKey) activeScanRef.current = null;
      })();
    },
    [selectedSessions, redactionLevel, useMock, onCacheChange],
  );

  // The review flow has no truthful idle state. Auto-scan an uncached selection,
  // then reuse the lifted cache on revisit; this preserves the canonical surface
  // without showing a false empty result while waiting for work that has not run.
  useEffect(() => {
    if (allSettled) return;
    if (hasInFlight) return;
    runScan(false);
  }, [allSettled, hasInFlight, runScan]);

  return {
    phase: allSettled ? 'ready' as const : 'scanning' as const,
    scanProgress,
    scannedCount,
    failureCount,
    sessionRedactions,
    scanError,
    runScan,
  };
}

// ---------------------------------------------------------------------------
// Map a peasant Redaction into a fairtrade RedactionReview match. The match id
// is namespaced by session so the flattened list (every selected session's
// findings, one surface) stays unique even when two sessions redact the same
// secret on the same line. Confidence is rescaled 0–100 → 0–1 (fairtrade's
// scale; its low-confidence caution fires below 0.70 — same threshold the old
// per-card badge used at < 70).
// ---------------------------------------------------------------------------

interface ReviewMatch {
  id: string;
  category: string;
  confidence: number;
  before: string;
  after: string;
  kept: boolean;
}

function matchId(sessionId: string, redactionId: string): string {
  return `${sessionId}::${redactionId}`;
}

// ---------------------------------------------------------------------------
// Main component
// ---------------------------------------------------------------------------

interface RedactionStepProps {
  sessions: ShareSession[];
  selectedIds: Set<string>;
  redactionLevel: SelectableRedactionLevel;
  /**
   * Called ONLY with a level this version offers. The design system's selector
   * still renders the other two; this step filters them out and explains the
   * refusal rather than forwarding a level the local API answers 400 for.
   */
  onLevelChange: (level: SelectableRedactionLevel) => void;
  onNext: () => void;
  onFooterActionsChange?: SetWizardFooterActions;
  /**
   * Redaction-result cache, lifted to the wizard so it survives this step's
   * mount/unmount. Keyed by `${level}:${sessionId}`.
   */
  cache: RedactionCache;
  onCacheChange: (updater: (prev: RedactionCache) => RedactionCache) => void;
  /** When true, use deterministic mock data instead of the real local scan. */
  useMock?: boolean;
}

export function RedactionStep({
  sessions,
  selectedIds,
  redactionLevel,
  onLevelChange,
  onNext,
  onFooterActionsChange,
  cache,
  onCacheChange,
  useMock = false,
}: RedactionStepProps) {
  const selectedSessions = useMemo(
    () => sessions.filter((s) => selectedIds.has(s.id)),
    [sessions, selectedIds],
  );

  // The design system's RedactionReview renders its own three-button level
  // selector from a list it holds internally, so this step cannot remove the two
  // levels the local API refuses. Narrowing it there is a design-system change and
  // a published bump; until that lands, pressing one of them must not reach the
  // scan endpoint, where it produces a 400 the wizard has no room to explain.
  //
  // So the level is filtered here and the refusal is stated in the user's own
  // terms. Failing closed with a reason is not as good as not offering the
  // control, and it is much better than an opaque failure or - worse - silently
  // scanning at a different level than the button the user just pressed shows as
  // active.
  const [refusedLevel, setRefusedLevel] = useState<string | null>(null);
  const handleLevelChange = useCallback(
    (level: string) => {
      if (!isSelectableRedactionLevel(level)) {
        setRefusedLevel(level);
        return;
      }
      setRefusedLevel(null);
      onLevelChange(level);
    },
    [onLevelChange],
  );

  // Real per-session redaction scan (or the mock in mock mode). Trigger-driven
  // and cache-backed — see useRedactionPipeline.
  const {
    phase,
    scanProgress,
    scannedCount,
    failureCount,
    sessionRedactions,
    scanError,
    runScan,
  } = useRedactionPipeline(selectedSessions, redactionLevel, useMock, cache, onCacheChange);

  // Per-match opt-out state. `kept` = the user opted this match OUT of
  // redaction, so the secret would leave the machine as-is (the loud,
  // safe-by-default warning case). This is local UI state — the same as the
  // prior surface, the web push sends the chosen level, not per-match edits.
  const [kept, setKept] = useState<Set<string>>(() => new Set());

  // Reset opt-outs when the level changes — the match set is rescanned, so a
  // stale opt-out would no longer point at a shown match.
  useEffect(() => {
    setKept(new Set());
  }, [redactionLevel]);

  const handleToggle = useCallback((id: string, keptNext: boolean) => {
    setKept((prev) => {
      const next = new Set(prev);
      if (keptNext) next.add(id);
      else next.delete(id);
      return next;
    });
  }, []);

  // Flatten every selected session's findings into one match list — the
  // composite is a single safe-by-default review surface.
  const matches = useMemo<ReviewMatch[]>(() => {
    const out: ReviewMatch[] = [];
    for (const session of selectedSessions) {
      const items = sessionRedactions.get(session.id) ?? [];
      for (const r of items) {
        const id = matchId(session.id, r.id);
        out.push({
          id,
          category: r.category,
          confidence: r.confidence / 100,
          before: r.originalText,
          after: r.redactedReplacement,
          kept: kept.has(id),
        });
      }
    }
    return out;
  }, [selectedSessions, sessionRedactions, kept]);

  const isScanning = phase === 'scanning';
  const isReady = phase === 'ready';
  const sessionCount = selectedSessions.length;
  const hasScanFailure = failureCount > 0;
  const hasOnlyFailedEmptyResults = isReady && hasScanFailure && matches.length === 0;
  const continueDisabled = isScanning || hasScanFailure;

  useEffect(() => {
    if (!onFooterActionsChange) return;
    onFooterActionsChange({
      primary: {
        label: 'Continue',
        onClick: onNext,
        disabled: continueDisabled,
        title: isScanning
          ? 'Scanning in progress…'
          : hasScanFailure
            ? 'Re-scan the failed sessions before continuing'
            : undefined,
      },
      secondary: isReady
        ? { label: 'Re-scan', onClick: () => runScan(true), title: 'Re-run the scan at this level' }
        : undefined,
    });
    return () => onFooterActionsChange(null);
  }, [continueDisabled, hasScanFailure, isReady, isScanning, onFooterActionsChange, onNext, runScan]);

  return (
    <div className="flex flex-col gap-5">
      {refusedLevel != null && (
        <div
          className="flex flex-col gap-1 border border-rule-strong bg-surface-raised p-4"
          role="alert"
        >
          <p className="text-sm font-medium text-ink">
            redaction level unchanged
          </p>
          <p className="whitespace-pre-line text-sm text-ink-2">
            {unselectableRedactionLevelReason(refusedLevel)}
          </p>
        </div>
      )}
      {/* The Fairtrade review owns the normal level selector; the bounded failure
          bridge mirrors it with Fairtrade buttons so users can recover. Cache
          reuse keeps revisits instant; Re-scan explicitly refreshes this level. */}
      <div className="flex items-center justify-between gap-3 px-5 py-3 bg-surface border border-rule">
        <span className="v2-eyebrow">redact</span>
        {!onFooterActionsChange && <div className="flex items-center gap-2">
          {isReady && (
            <Button
              variant="secondary"
              size="sm"
              onClick={() => runScan(true)}
              title="Re-run the scan at this level"
            >
              Re-scan
            </Button>
          )}
          <Button
            variant="primary"
            size="sm"
            onClick={onNext}
            disabled={continueDisabled}
            title={
              isScanning
                ? 'Scanning in progress…'
                : hasScanFailure
                  ? 'Re-scan the failed sessions before continuing'
                  : undefined
            }
          >
            Continue
          </Button>
        </div>}
      </div>

      {isScanning ? (
        // Scanning state — kept as step chrome so the review surface never shows
        // its "no sensitive content — safe to share" empty message mid-scan.
        <div className="flex flex-col items-center justify-center py-16 text-center border border-rule bg-surface">
          <LoaderIcon className="size-8 text-ink-3 animate-spin mb-4" />
          <p className="text-sm font-medium text-ink">Scanning your work for sensitive content</p>
          <p className="text-xs text-ink-3 mt-1 tabular-nums">
            {sessionCount} session{sessionCount !== 1 ? 's' : ''} · nothing has left your machine
          </p>
          <div className="mt-4 h-1.5 w-48 bg-surface-hover overflow-hidden">
            <div
              className="h-full bg-rule-strong transition-all duration-300"
              style={{ width: `${scanProgress * 100}%` }}
            />
          </div>
        </div>
      ) : hasOnlyFailedEmptyResults ? (
        <section
          className="flex flex-col gap-4 border border-rule bg-surface p-5"
          aria-label="redaction review"
        >
          <div className="flex items-start gap-3 border border-rule-strong bg-surface-raised p-4" role="alert">
            <div>
              <p className="text-sm font-medium text-ink">redaction scan incomplete</p>
              <p className="mt-1 text-sm text-ink-2">{scanError}</p>
            </div>
          </div>
          <div>
            <div
              className="mb-4 flex flex-wrap items-center gap-2"
              role="group"
              aria-label="redaction level"
            >
              {REDACTION_LEVELS.map((level) => (
                <Button
                  key={level}
                  variant={level === redactionLevel ? 'primary' : 'secondary'}
                  size="sm"
                  pressed={level === redactionLevel}
                  onClick={() => handleLevelChange(level)}
                >
                  {level}
                </Button>
              ))}
            </div>
            <p className="text-xs text-ink-3 tabular-nums">
              {scannedCount} / {sessionCount} sessions scanned successfully
            </p>
            <div
              className="mt-2 h-1.5 bg-surface-hover overflow-hidden"
              role="progressbar"
              aria-valuenow={scannedCount}
              aria-valuemin={0}
              aria-valuemax={sessionCount}
              aria-label={`scanned ${scannedCount} of ${sessionCount}`}
            >
              <div
                className="h-full bg-rule-strong"
                style={{ width: `${scanProgress * 100}%` }}
              />
            </div>
          </div>
          <p className="text-sm text-ink-2">
            sharing status is unknown until every selected session is scanned successfully.
          </p>
        </section>
      ) : (
        <RedactionReview
          level={redactionLevel}
          onLevel={handleLevelChange}
          matches={matches}
          onToggle={handleToggle}
          scanned={scannedCount}
          total={sessionCount}
          failure={scanError ?? false}
        />
      )}
    </div>
  );
}
