'use client';

import { useEffect, useState } from 'react';
import {
  fetchSessionCommits,
  fetchSessionPullRequestsResponse,
  type CommitInfo,
  type SessionPullRequest,
} from '@/lib/api/session-extras';
import type { UnverifiedPullRequest } from '@peasant-labs/schema';

export interface SessionExtras {
  /** The commits recorded for this session; empty when it produced none. */
  commits: CommitInfo[];
  /** The pull requests a recorded `gh pr create` created; empty when none. */
  pullRequests: SessionPullRequest[];
  unverifiedPullRequests?: UnverifiedPullRequest[];
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
 * The transcript page mounts this under `key={sessionId}`, so changing session
 * is a remount and every mount starts from empty. A consumer that keeps one
 * instance across session changes is covered too: the effect clears the current
 * rows and aborts both requests when the id changes, and an abandoned read's
 * answer is dropped instead of being written into the session now on screen.
 * Both requests are aborted on unmount as well.
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
    void fetchSessionPullRequestsResponse(sessionId, signal)
      .then((response) => {
        if (!signal.aborted) setExtras((current) => ({ ...current, pullRequests: response.pullRequests, unverifiedPullRequests: response.unverified_pull_requests }));
      })
      .catch(() => undefined);
    return () => controller.abort();
  }, [sessionId]);

  return extras;
}
