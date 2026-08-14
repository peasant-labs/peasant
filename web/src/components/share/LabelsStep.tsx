'use client';

import { useCallback, useEffect, useMemo, useState } from 'react';
import { Button } from '@/lib/ft-ui';
import { cn } from '@/lib/utils';
import type {
  ShareSession,
  ShareLabel,
  LabelOrigin,
  LabelSelection,
} from '@/lib/share/types';
import { emptyLabelSelection, originForAnnotatorKind } from '@/lib/share/types';
import { displayProject } from '@/lib/quality/utils';
import { fetchSessionAnnotations, type AnnotationSummary } from '@/lib/api/annotations';
import { getMockAnnotations } from '@/lib/share/mock-data';
import type { SetShareFooterActions } from '@/components/share/footer-actions';

// Re-export so existing importers keep resolving even though the canonical
// definition lives in lib/share/types.
export type { LabelSelection } from '@/lib/share/types';

// Visual treatment per origin. Both are monochrome — authorship (human vs
// automatic) is a distinction, not a semantic outcome, so the human signal
// stands out via a stronger border/ink weight rather than colour (strict
// monochrome; --success is reserved for "done/contributed").
const ORIGIN_CLASS: Record<LabelOrigin, string> = {
  auto: 'bg-surface-hover text-ink-2 border-rule',
  manual: 'bg-surface text-ink border-rule-strong',
};

const ORIGIN_LABEL: Record<LabelOrigin, string> = {
  auto: 'Automatic',
  manual: 'Manual',
};

interface LabelsStepProps {
  sessions: ShareSession[];
  selectedIds: Set<string>;
  /** Reports the SELECTED labels (the subset the user keeps) upward. */
  onLabelsChange: (next: LabelSelection) => void;
  onNext: () => void;
  onFooterActionsChange: SetShareFooterActions;
  /**
   * When true, the step reads deterministic mock annotations instead of calling
   * GET /api/v1/annotations. Wired from the wizard's mock config so mock and
   * real runs exercise the same UI.
   */
  useMock?: boolean;
}

/** Convert a wire annotation into the wizard's distilled, push-bound label. */
function toShareLabel(sessionId: string, a: AnnotationSummary): ShareLabel {
  return {
    id: a.id,
    sessionId,
    origin: originForAnnotatorKind(a.annotatorKind),
    annotatorKind: a.annotatorKind,
    annotatorName: a.annotatorName,
    typeId: a.typeId,
    typeName: a.typeName || a.typeId,
    value: a.value,
  };
}

interface SessionGroup {
  session: ShareSession;
  auto: ShareLabel[];
  manual: ShareLabel[];
}

/**
 * Labels step. Labels are real annotations (GET /api/v1/annotations), grouped
 * per session into Automatic (rule/agent at ingest) vs Manual (human) — the
 * project's `annotatorKind` distinction. Every label is included by default;
 * the user opts individual labels out, or uses the bulk actions. Only the
 * included annotation ids flow into the push (see PushStep / `peasant push
 * --annotation-id`). Sessions with no annotations are not listed.
 */
export function LabelsStep({
  sessions,
  selectedIds,
  onLabelsChange,
  onNext,
  onFooterActionsChange,
  useMock = false,
}: LabelsStepProps) {
  const selectedSessions = useMemo(
    () => sessions.filter((s) => selectedIds.has(s.id)),
    [sessions, selectedIds],
  );

  const [groups, setGroups] = useState<SessionGroup[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  // Annotation ids the user keeps. Starts as every discovered id.
  const [included, setIncluded] = useState<Set<string>>(() => new Set());

  // Discover labels for every selected session (mock or real).
  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setError(null);

    async function discover() {
      try {
        const perSession = await Promise.all(
          selectedSessions.map(async (session) => {
            const annotations = useMock
              ? getMockAnnotations(session.id)
              : await fetchSessionAnnotations(session.id);
            // Only session-level labels are meaningful here; entry-level
            // (per-turn) annotations belong to the transcript itself.
            const labels = annotations
              .filter((a) => a.targetKind === 'session')
              .map((a) => toShareLabel(session.id, a));
            return { session, labels } as const;
          }),
        );

        if (cancelled) return;

        const next: SessionGroup[] = perSession
          .map(({ session, labels }) => ({
            session,
            auto: labels.filter((l) => l.origin === 'auto'),
            manual: labels.filter((l) => l.origin === 'manual'),
          }))
          .filter((g) => g.auto.length > 0 || g.manual.length > 0);

        setGroups(next);
        // Include everything by default.
        setIncluded(
          new Set(
            next.flatMap((g) => [...g.auto, ...g.manual].map((l) => l.id)),
          ),
        );
        setLoading(false);
      } catch (err) {
        if (cancelled) return;
        setError(err instanceof Error ? err.message : 'Failed to load labels');
        setLoading(false);
      }
    }

    void discover();
    return () => {
      cancelled = true;
    };
  }, [selectedSessions, useMock]);

  const allIds = useMemo(
    () => groups.flatMap((g) => [...g.auto, ...g.manual].map((l) => l.id)),
    [groups],
  );

  // Report the selection upward whenever it changes.
  useEffect(() => {
    const bySession = new Map<string, ShareLabel[]>();
    for (const g of groups) {
      bySession.set(g.session.id, [...g.auto, ...g.manual]);
    }
    const selection: LabelSelection = { bySession, includedIds: new Set(included) };
    onLabelsChange(selection);
  }, [included, groups, onLabelsChange]);

  const toggle = useCallback((id: string) => {
    setIncluded((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }, []);

  const hasLabels = groups.length > 0;
  const includedCount = included.size;

  useEffect(() => {
    onFooterActionsChange({
      primary: {
        label: loading || hasLabels ? 'Continue' : 'Skip',
        onClick: onNext,
        disabled: loading,
        title: loading ? 'Labels are still loading' : undefined,
      },
    });
    return () => onFooterActionsChange(null);
  }, [hasLabels, loading, onFooterActionsChange, onNext]);

  if (loading) {
    return (
      <div className="flex items-center justify-center py-16">
        <p className="text-sm text-ink-3">Loading labels…</p>
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-4">
      {/* Toolbar — bulk select across every session + included counter. */}
      <div className="flex items-center justify-between gap-3 border border-rule bg-surface px-5 py-3">
        <div className="flex items-center gap-3 text-xs">
          {hasLabels ? (
            <>
              <button
                type="button"
                onClick={() => setIncluded(new Set(allIds))}
                className="text-ink-3 hover:text-ink transition-colors focus-mono cursor-pointer"
              >
                Select all
              </button>
              <span className="text-ink-4" aria-hidden>
                ·
              </span>
              <button
                type="button"
                onClick={() => setIncluded(new Set())}
                className="text-ink-3 hover:text-ink transition-colors focus-mono cursor-pointer"
              >
                Select none
              </button>
              <span className="text-ink-4" aria-hidden>
                ·
              </span>
              <span className="text-ink-3 tabular-nums">
                {includedCount} of {allIds.length} labels included
              </span>
            </>
          ) : (
            <span className="text-ink-3">
              {error
                ? `Could not load labels: ${error}`
                : 'No labels on the selected sessions.'}
            </span>
          )}
        </div>
      </div>

      {/* One card per session — Automatic and Manual label groups. */}
      {groups.map((g) => (
        <div key={g.session.id} className="border border-rule bg-surface">
          <div className="flex items-center gap-2 border-b border-rule px-4 py-2.5">
            <span className="font-mono tabular-nums text-[13px] text-ink">
              {g.session.id.slice(5, 13)}…
            </span>
            <span className="text-xs text-ink-3" title={g.session.projectName}>
              {displayProject(g.session.projectName)}
            </span>
          </div>

          <div className="flex flex-col divide-y divide-rule">
            {(['auto', 'manual'] as const).map((origin) => {
              const labels = origin === 'auto' ? g.auto : g.manual;
              if (labels.length === 0) return null;
              return (
                <div key={origin} className="px-4 py-3">
                  <div className="mb-2 flex items-center gap-2">
                    <span className="text-[11px] font-medium uppercase tracking-wide text-ink-3">
                      {ORIGIN_LABEL[origin]}
                    </span>
                    <span className="text-[11px] text-ink-4 tabular-nums">
                      {labels.length}
                    </span>
                  </div>
                  <div className="flex flex-wrap items-center gap-1.5">
                    {labels.map((label) => {
                      const on = included.has(label.id);
                      return (
                        <button
                          key={label.id}
                          type="button"
                          onClick={() => toggle(label.id)}
                          aria-pressed={on}
                          title={
                            on
                              ? `included · click to exclude (${label.annotatorName})`
                              : `excluded · click to include (${label.annotatorName})`
                          }
                          className={cn(
                            'inline-flex items-center gap-1 px-2 py-0.5 text-[11px] leading-[1.4] border whitespace-nowrap transition-colors cursor-pointer focus-mono',
                            on
                              ? ORIGIN_CLASS[origin]
                              : 'border-rule bg-transparent text-ink-4 line-through',
                          )}
                        >
                          <span className="font-medium">{label.typeName}</span>
                          <span className="opacity-70">{label.value}</span>
                        </button>
                      );
                    })}
                  </div>
                </div>
              );
            })}
          </div>
        </div>
      ))}
    </div>
  );
}

// Re-export the empty-selection helper so the wizard can seed state without a
// second import.
export { emptyLabelSelection };
