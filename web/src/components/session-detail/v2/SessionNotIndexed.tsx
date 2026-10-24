'use client';

import { useEffect, useRef, useState } from 'react';
import { FileClock } from 'lucide-react';
import { EmptyState } from '@/lib/ft-ui';
import { Breadcrumbs } from '@/components/Breadcrumbs';
import { reindexSession, SessionReindexError } from '@/lib/api/ingest';
import { reindexReason, reindexRetryable, type ReindexFailureCode } from '@/lib/share/scanFailure';
import type { CrumbItem } from './lib/scopeTurns';

type Run =
  | { status: 'idle' }
  | { status: 'running' }
  | { status: 'done' }
  | { status: 'failed'; code: ReindexFailureCode; message: string };

/**
 * A session this computer recorded but holds no indexed turns for: its turn
 * count comes from the recording, while the index has nothing to show (an
 * earlier build refused the recording, or indexing stopped partway). A count
 * above an empty transcript reads as a broken viewer, so the page says what is
 * missing and offers the one fix, a re-index of this session, here. When the
 * re-index lands, `onReindexed` reads the session again and the transcript
 * replaces this state.
 */
export function SessionNotIndexed({
  sessionId,
  turnCount,
  crumbs,
  onReindexed,
}: {
  sessionId: string;
  turnCount: number;
  crumbs: CrumbItem[];
  onReindexed: () => void;
}) {
  const [run, setRun] = useState<Run>({ status: 'idle' });
  const controller = useRef<AbortController | null>(null);
  useEffect(() => () => controller.current?.abort(), []);

  const reindex = () => {
    if (controller.current) return;
    const current = new AbortController();
    controller.current = current;
    setRun({ status: 'running' });
    reindexSession(sessionId, { signal: current.signal })
      .then(() => {
        if (current.signal.aborted) return;
        setRun({ status: 'done' });
        onReindexed();
      })
      .catch((error: unknown) => {
        if (current.signal.aborted) return;
        setRun({
          status: 'failed',
          code: error instanceof SessionReindexError ? error.code : 'other',
          message: error instanceof Error ? error.message : String(error),
        });
      })
      .finally(() => { if (controller.current === current) controller.current = null; });
  };

  const turns = `${turnCount} recorded ${turnCount === 1 ? 'turn' : 'turns'}`;
  const message = run.status === 'failed'
    ? `the re-index did not finish. ${reindexReason(run.code)}`
    : run.status === 'done'
      ? `the re-index ran, but peasant still can’t read the ${turns} of this session.`
      : `this session has ${turns}, but peasant hasn’t indexed them yet, so there is nothing to show.`;
  const canRun = run.status !== 'failed' || reindexRetryable(run.code);

  return (
    <div className="asb-page" data-testid="session-not-indexed">
      <Breadcrumbs items={crumbs} />
      <EmptyState
        as="h1"
        icon={FileClock}
        title="transcript not indexed yet"
        message={message}
        action={canRun ? (
          <button type="button" className="btn btn-primary btn-sm" onClick={reindex} disabled={run.status === 'running'}>
            {run.status === 'running' ? 're-indexing…' : run.status === 'idle' ? 're-index this session' : 're-index again'}
          </button>
        ) : undefined}
      />
      {run.status === 'failed' && (
        <details className="project-problem-details">
          <summary>details</summary>
          <p>{run.message}</p>
        </details>
      )}
    </div>
  );
}
