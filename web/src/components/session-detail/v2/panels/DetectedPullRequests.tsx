'use client';

import { useId } from 'react';
import type { DetectedPullRequest } from '@/lib/api/session-extras';

interface DetectedPullRequestsProps {
  /** The pull requests detected in this session's stored tool calls; the caller skips an empty list. */
  pullRequests: DetectedPullRequest[];
}

/**
 * The pull requests a recorded `gh pr create` command created, under the
 * transcript header's meta row (mounted through the viewer's `pullRequests`
 * slot).
 *
 * This is EVIDENCE, not a binding: detection reads the session's own stored
 * tool call, so the row says where it came from and that nothing was verified
 * with GitHub or village. Nothing here implies access — the transcript is
 * readable whether or not the reader can open the pull request on GitHub.
 *
 * Each row opens GitHub itself (`pr.url`), never a route of ours: peasant holds
 * no pull request record beyond the URL the command printed.
 */
export function DetectedPullRequests({ pullRequests }: DetectedPullRequestsProps) {
  const titleId = useId();
  return (
    <section aria-labelledby={titleId} className="flex min-w-0 flex-col">
      <p className="v2-eyebrow" id={titleId}>
        pull requests <span className="tnum">{pullRequests.length}</span>
      </p>
      <ul className="mt-1 flex min-w-0 flex-col gap-0.5">
        {pullRequests.map((pr) => (
          <li key={`${pr.owner}/${pr.repo}#${pr.number}`} className="min-w-0">
            <a
              href={pr.url}
              target="_blank"
              rel="noreferrer"
              className="font-mono text-[14px] leading-5 text-ink underline underline-offset-2 hover:text-ink-2 focus-mono"
            >
              {pr.owner}/{pr.repo} <span className="tnum">#{pr.number}</span>
            </a>
          </li>
        ))}
      </ul>
      <p
        className="mt-1 text-[14px] leading-5 text-ink-3"
        title="found in a gh pr create command in this session; not verified by village."
      >
        detected from <span className="font-mono">gh pr create</span>; not verified by village.
      </p>
    </section>
  );
}
