'use client';

import { useCallback, useEffect, useMemo, useState } from 'react';
import { RedactionReview } from '@/lib/ft-ui';
import { LoaderIcon } from 'lucide-react';
import type { ShareSession } from '@/lib/share/types';
import {
  isSelectableRedactionLevel,
  SELECTABLE_REDACTION_LEVELS,
  type SelectableRedactionLevel,
} from '@/lib/share/redactions';
import {
  useRedactionPipeline,
  type RedactionCache,
  type RedactionCacheUpdate,
} from '@/lib/share/redactionScan';
import type { SetShareFooterActions } from '@/components/share/footer-actions';

// ---------------------------------------------------------------------------
// Redaction pipeline — the shared local scan (lib/share/redactionScan). Its
// (level, session) cache is lifted out of this step, so leaving and returning
// neither repeats the work nor turns a failed scan into an all-clear.
// ---------------------------------------------------------------------------

export type { RedactionCache, RedactionCacheEntry } from '@/lib/share/redactionScan';

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
  /** Called only with a level exposed by the constrained design-system review. */
  onLevelChange: (level: SelectableRedactionLevel) => void;
  onNext: () => void;
  onFooterActionsChange: SetShareFooterActions;
  /**
   * Redaction-result cache, lifted above this step so it survives the step's
   * mount/unmount. Keyed by (level, session).
   */
  cache: RedactionCache;
  onCacheChange: RedactionCacheUpdate;
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

  // Fairtrade renders only the levels the local API offers. Keep the callback
  // boundary narrowed even though RedactionReview already validates the set and
  // guarantees that it emits one of those available levels.
  const handleLevelChange = useCallback(
    (level: string) => {
      if (isSelectableRedactionLevel(level)) onLevelChange(level);
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
          className="w-full"
          level={redactionLevel}
          availableLevels={SELECTABLE_REDACTION_LEVELS}
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
