'use client';

import { useEffect, useState } from 'react';
import {
  fetchSessionCommits,
  fetchSessionPullRequests,
  type DetectedPullRequest,
  type SessionCommit,
} from '@/lib/api/session-extras';

export interface SessionExtras {
  /** The commits recorded for this session; empty when it produced none. */
  commits: SessionCommit[];
  /** The pull requests a recorded `gh pr create` created; empty when none. */
  pullRequests: DetectedPullRequest[];
}

const NO_EXTRAS: SessionExtras = { commits: [], pullRequests: [] };

/**
 * The session's additive extras: its recorded commits and the pull requests
 * detected from its own tool calls.
 *
 * Both reads are display-only additions to the transcript, so a failure leaves
 * that piece empty instead of raising a banner — the transcript is complete
 * without them, and a genuinely dead channel is reported by the session detail
 * channel, not by these. The reads run independently: one endpoint being
 * unavailable never suppresses the other.
 *
 * Switching session discards the previous session's rows before the new reads
 * land, so one transcript's commits or pull requests can never appear under
 * another. Both requests are aborted on unmount and on session change.
 */
export function useSessionExtras(sessionId: string): SessionExtras {
  const [extras, setExtras] = useState<SessionExtras>(NO_EXTRAS);

  useEffect(() => {
    const controller = new AbortController();
    const { signal } = controller;
    setExtras(NO_EXTRAS);
    void fetchSessionCommits(sessionId, signal)
      .then((commits) => {
        if (!signal.aborted) setExtras((current) => ({ ...current, commits }));
      })
      .catch(() => undefined);
    void fetchSessionPullRequests(sessionId, signal)
      .then((pullRequests) => {
        if (!signal.aborted) setExtras((current) => ({ ...current, pullRequests }));
      })
      .catch(() => undefined);
    return () => controller.abort();
  }, [sessionId]);

  return extras;
}
