'use client';

import { useCallback, useEffect, useMemo, useState } from 'react';
import { WhereDoesThisGo, Button } from '@/lib/ft-ui';
import { displayProject } from '@/lib/quality/utils';
import { runPush } from '@/lib/share/push';
import type { ShareSession, LabelSelection } from '@/lib/share/types';
import {
  DEFAULT_REDACTION_LEVEL,
  type SelectableRedactionLevel,
} from '@/lib/share/redactions';
import {
  CheckCircle2Icon,
  CircleAlertIcon,
  LoaderIcon,
  ExternalLinkIcon,
  ShieldCheckIcon,
} from 'lucide-react';
import type { SetWizardFooterActions } from '@/app/share/ShareWizardClient';

const COMMONS_URL =
  process.env.NEXT_PUBLIC_COMMONS_URL ?? 'https://village.peasantlabs.org';

// Where the local copy stays after a push under the default XDG data directory.
const LOCAL_SYNC_PATH = '~/.local/share/peasant/peasant-sync/';

/**
 * Rough byte estimate for a transcript from its token count. Transcripts are
 * JSONL; ~4 bytes/token is the usual ballpark, and the JSON envelope roughly
 * doubles it. Used only for the "what gets sent" panel and clearly labelled
 * approximate — the real size is computed server-side at push time.
 */
function estimateTranscriptBytes(totalTokens: number): number {
  return Math.round(totalTokens * 4 * 2);
}

function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

// ---------------------------------------------------------------------------
// Per-session push state
// ---------------------------------------------------------------------------

type PushState = 'pending' | 'uploading' | 'done' | 'skipped' | 'error';

interface SessionPushState {
  sessionId: string;
  state: PushState;
  error?: string;
}

interface PushSummary {
  done: number; // new + updated
  skipped: number;
  errors: number;
  total: number;
}

// ---------------------------------------------------------------------------
// Real push (POST /sync/push) — or an honest no-op simulation in mock mode.
// ---------------------------------------------------------------------------

function usePush(sessionIds: string[], redactionLevel: SelectableRedactionLevel, useMock: boolean) {
  const [states, setStates] = useState<SessionPushState[]>(() =>
    sessionIds.map((id) => ({ sessionId: id, state: 'pending' })),
  );
  const [phase, setPhase] = useState<'idle' | 'pushing' | 'done' | 'error'>('idle');
  const [topError, setTopError] = useState<string | null>(null);
  const [summary, setSummary] = useState<PushSummary>({
    done: 0,
    skipped: 0,
    errors: 0,
    total: sessionIds.length,
  });

  // Reset whenever the selection changes.
  useEffect(() => {
    setStates(sessionIds.map((id) => ({ sessionId: id, state: 'pending' })));
    setPhase('idle');
    setTopError(null);
    setSummary({ done: 0, skipped: 0, errors: 0, total: sessionIds.length });
  }, [sessionIds]);

  const start = useCallback(async () => {
    setPhase('pushing');
    setTopError(null);
    setStates((prev) => prev.map((s) => ({ ...s, state: 'uploading' })));

    if (useMock) {
      // Mock mode has no village + fake session ids; don't pretend to push.
      setStates((prev) => prev.map((s) => ({ ...s, state: 'skipped' })));
      setSummary({ done: 0, skipped: sessionIds.length, errors: 0, total: sessionIds.length });
      setTopError(
        'mock mode · the push is not run. Disable mock data to contribute for real.',
      );
      setPhase('done');
      return;
    }

    try {
      const result = await runPush(sessionIds, redactionLevel);
      const byId = new Map(result.sessions.map((s) => [s.sessionId, s]));
      setStates(
        sessionIds.map((id) => {
          const r = byId.get(id);
          if (!r) return { sessionId: id, state: 'skipped' };
          if (r.status === 'error') return { sessionId: id, state: 'error', error: r.error };
          if (r.status === 'skipped') return { sessionId: id, state: 'skipped' };
          return { sessionId: id, state: 'done' };
        }),
      );
      setSummary({
        done: result.new + result.updated,
        skipped: result.skipped,
        errors: result.errors,
        total: sessionIds.length,
      });
      setPhase('done');
    } catch (e: unknown) {
      // Honest failure (e.g. 401 "run 'peasant village login' first").
      setTopError(e instanceof Error ? e.message : String(e));
      setStates((prev) => prev.map((s) => ({ ...s, state: 'pending' })));
      setPhase('error');
    }
  }, [sessionIds, redactionLevel, useMock]);

  return { states, phase, topError, start, summary };
}

// ---------------------------------------------------------------------------
// Session push row
// ---------------------------------------------------------------------------

function SessionPushRow({
  session,
  pushState,
}: {
  session: ShareSession;
  pushState: SessionPushState;
}) {
  return (
    <div className="flex items-center gap-3 px-3 py-2.5">
      {/* Status slot — reserved so rows stay aligned. Pending shows nothing:
          an empty box reads as an unchecked checkbox. The spinner appears only
          while a transcript is uploading. */}
      <div className="size-5 flex-shrink-0">
        {pushState.state === 'uploading' && (
          <LoaderIcon className="size-5 text-ink-2 animate-spin" />
        )}
        {pushState.state === 'done' && (
          <CheckCircle2Icon className="size-5 text-success" />
        )}
        {pushState.state === 'error' && (
          <CircleAlertIcon className="size-5 text-danger" />
        )}
      </div>

      <div className="flex-1 min-w-0">
        <div className="flex items-center gap-2">
          <span className="text-sm font-medium font-mono tabular-nums truncate text-ink">
            {session.id.slice(5, 13)}...
          </span>
          <span className="text-xs text-ink-3" title={session.projectName}>{displayProject(session.projectName)}</span>
        </div>
        {pushState.state === 'error' && pushState.error && (
          <p className="text-xs text-danger mt-0.5">{pushState.error}</p>
        )}
      </div>

      {pushState.state === 'error' && (
        <span className="inline-flex items-center border border-danger/30 bg-danger-soft text-danger px-1.5 py-0.5 text-[10px] font-medium">
          Failed
        </span>
      )}
      {pushState.state === 'skipped' && (
        <span className="inline-flex items-center border border-rule bg-surface-hover text-ink-3 px-1.5 py-0.5 text-[10px] font-medium">
          Skipped
        </span>
      )}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Main component
// ---------------------------------------------------------------------------

interface PushStepProps {
  sessions: ShareSession[];
  selectedIds: Set<string>;
  /**
   * Labels (annotations) chosen on the Labels step, grouped auto/manual.
   * Surfaced in the transparency panel for the count only — the web push sends
   * the session's annotations as ingested and does not filter by this
   * selection. Label-level filtering is CLI-only (`peasant push --annotation-id`).
   */
  labels: LabelSelection;
  /** Redaction level chosen on the Redact step — sent with the push. */
  redactionLevel?: SelectableRedactionLevel;
  /** When true, the push is not run (mock mode has no village). */
  useMock?: boolean;
  onFooterActionsChange?: SetWizardFooterActions;
}

export function PushStep({
  sessions,
  selectedIds,
  labels,
  redactionLevel = DEFAULT_REDACTION_LEVEL,
  useMock = false,
  onFooterActionsChange,
}: PushStepProps) {
  const selectedSessions = useMemo(
    () => sessions.filter((s) => selectedIds.has(s.id)),
    [sessions, selectedIds],
  );

  const sessionIds = useMemo(
    () => selectedSessions.map((s) => s.id),
    [selectedSessions],
  );

  // Aggregate the "what gets sent" figures for the transparency panel.
  const totalTokens = useMemo(
    () => selectedSessions.reduce((sum, s) => sum + s.totalTokens, 0),
    [selectedSessions],
  );
  const transcriptBytes = useMemo(
    () => selectedSessions.reduce((sum, s) => sum + estimateTranscriptBytes(s.totalTokens), 0),
    [selectedSessions],
  );

  // Label totals for the transparency panel: how many annotations were
  // discovered vs. how many the user kept. Display-only — see the `labels` prop
  // note (the web push does not filter by label).
  const labelStats = useMemo(() => {
    let discovered = 0;
    let auto = 0;
    let manual = 0;
    for (const labelsForSession of labels.bySession.values()) {
      for (const label of labelsForSession) {
        discovered++;
        if (!labels.includedIds.has(label.id)) continue;
        if (label.origin === 'manual') manual++;
        else auto++;
      }
    }
    return { discovered, included: labels.includedIds.size, auto, manual };
  }, [labels]);

  // The two-column transparency split, encoded for the fairtrade
  // `WhereDoesThisGo` composite: each line names a thing + its measure.
  const sentItems = useMemo(
    () => [
      `redacted transcripts (~${formatBytes(transcriptBytes)})`,
      `session metadata · ${totalTokens.toLocaleString()} tokens`,
      `selected labels · ${labelStats.included} of ${labelStats.discovered}` +
        (labelStats.included > 0
          ? ` (${labelStats.auto} automatic · ${labelStats.manual} manual)`
          : ''),
    ],
    [transcriptBytes, totalTokens, labelStats],
  );

  const privateItems = useMemo(
    () => [
      'raw file paths · hashed before upload',
      // "matching redaction patterns" is the load-bearing qualifier. Pattern
      // matching is best effort, so this line describes what the ${redactionLevel}
      // level FINDS rather than promising it found everything.
      `PII matching known redaction patterns · rewritten at the ${redactionLevel} level, best effort`,
      `excluded labels · ${labelStats.discovered - labelStats.included} opted out`,
    ],
    [redactionLevel, labelStats],
  );

  const { states, phase, topError, start, summary } = usePush(sessionIds, redactionLevel, useMock);

  useEffect(() => {
    if (!onFooterActionsChange) return;
    onFooterActionsChange(
      phase === 'idle' || phase === 'error'
        ? { primary: { label: phase === 'error' ? 'Try again' : 'Submit', onClick: start } }
        : null,
    );
    return () => onFooterActionsChange(null);
  }, [onFooterActionsChange, phase, start]);

  return (
    <div className="flex flex-col gap-5">
      {/* Transparency panel — "where does this go?" Shown before push begins so
          the user sees the destination, payload, redactions, and post-publish
          state up front. Collapses once the push starts to keep focus on
          progress. */}
      {phase === 'idle' && (
        <div className="flex flex-col gap-3">
          <WhereDoesThisGo
            destination={COMMONS_URL}
            sent={sentItems}
            private={privateItems}
          />

          {/* Destination context + the context-travels line — kept as
              app chrome; the composite carries the two-column split. */}
          <p className="px-1 text-xs text-ink-3">
            The public Peasant commons — {selectedSessions.length} session
            {selectedSessions.length === 1 ? '' : 's'} will be uploaded.
          </p>
          <p className="px-1 text-xs text-ink-3">
            Annotations and commit links travel with the transcript — minus whatever
            redaction removed.
          </p>

          {/* Post-publish note — kept as app chrome; the local copy is untouched. */}
          <div className="flex items-start gap-2 border border-rule bg-surface-hover px-5 py-3">
            <ShieldCheckIcon className="mt-0.5 size-3.5 flex-shrink-0 text-ink-2" />
            <p className="text-xs text-ink-2">
              After publishing, sessions are{' '}
              <span className="font-medium text-ink">public</span> on the commons.
              Your local copy is untouched and stays in{' '}
              <span className="font-mono text-ink">{LOCAL_SYNC_PATH}</span>.
            </p>
          </div>
        </div>
      )}

      <div className="px-5 py-3 bg-surface border border-rule-strong">
        <div className="flex items-center justify-between gap-3">
          <div className="flex items-center gap-3">
            {phase === 'pushing' && (
              <span className="inline-flex items-center gap-2 text-sm text-ink-3">
                <LoaderIcon className="size-4 animate-spin" /> Contributing…
              </span>
            )}
            {phase === 'done' && (
              <span className="text-sm text-ink-3 tabular-nums">
                {summary.done} shared
                {summary.skipped > 0 ? `, ${summary.skipped} skipped` : ''}
                {summary.errors > 0 ? `, ${summary.errors} failed` : ''}
              </span>
            )}
          </div>
          <div className="flex items-center gap-3">
            {!onFooterActionsChange && (phase === 'idle' || phase === 'error') && (
              <Button variant="primary" size="sm" onClick={start}>
                {phase === 'error' ? 'Try again' : 'Submit'}
              </Button>
            )}
          </div>
        </div>
      </div>

      {/* Honest failure — the push didn't run (e.g. not signed in). The server's
          message is shown verbatim; for the common "log in first" case it tells
          the user exactly what to do. No fabricated success. */}
      {phase === 'error' && topError && (
        <div className="border border-danger/30 bg-danger-soft px-5 py-4" role="alert">
          <p className="text-sm font-medium text-ink">Nothing was sent.</p>
          <p className="mt-1 text-[13px] text-danger">{topError}</p>
        </div>
      )}

      {/* Completion — only claim success when something actually published. */}
      {phase === 'done' && summary.done > 0 && (
        <div className="border border-success/30 bg-success-soft">
          <div className="flex flex-col items-center text-center px-6 py-8">
            <CheckCircle2Icon className="size-10 text-success mb-3" />
            <h3 className="font-[family-name:var(--font-display)] text-lg font-semibold text-ink">
              {summary.done} contributed to the commons
              {summary.errors > 0 ? ` · ${summary.errors} failed` : ''}
            </h3>
            <Button
              as="a"
              size="sm"
              variant="secondary"
              className="mt-4"
              href={COMMONS_URL}
              target="_blank"
              rel="noopener noreferrer"
              iconRight={ExternalLinkIcon}
            >
              View in the commons
            </Button>
          </div>
        </div>
      )}

      {/* Done but nothing published. Distinguish a genuine no-op (all skipped /
          mock) from a total failure (every session rejected, e.g. a server-side
          secret-scan 422 which returns 200 with errors>0) — the latter must read
          as a failure, not "already up to date". */}
      {phase === 'done' && summary.done === 0 && (
        summary.errors > 0 ? (
          <div className="border border-danger/30 bg-danger-soft px-5 py-4" role="alert">
            <p className="text-sm font-medium text-ink">Nothing was sent.</p>
            <p className="mt-1 text-[13px] text-danger">
              {topError ??
                `${summary.errors} session${summary.errors === 1 ? '' : 's'} failed; see the rows below.`}
            </p>
          </div>
        ) : (
          <div className="border border-rule bg-surface px-5 py-4">
            <p className="text-sm text-ink-2">
              {topError ??
                `nothing new to send; ${summary.skipped} session${summary.skipped === 1 ? ' was' : 's were'} already up to date.`}
            </p>
          </div>
        )
      )}

      {/* Per-session rows */}
      <div className="border border-rule bg-surface">
        <div className="px-5 py-4">
          <div className="space-y-1">
            {selectedSessions.map((session) => {
              const pushState = states.find((s) => s.sessionId === session.id) ?? {
                sessionId: session.id,
                state: 'pending' as const,
              };
              return (
                <SessionPushRow
                  key={session.id}
                  session={session}
                  pushState={pushState}
                />
              );
            })}
          </div>
        </div>
      </div>
    </div>
  );
}
