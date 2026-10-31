'use client';

import { useId } from 'react';
import type { SessionPullRequest } from '@/lib/api/session-extras';

interface DetectedPullRequestsProps {
  /**
   * The pull requests detected in this session's stored tool calls. The caller
   * skips an empty list; one that arrives anyway still renders no evidence
   * sentence.
   */
  pullRequests: SessionPullRequest[];
}

/**
 * The evidence label for one reported source. `gh_pr_create` keeps the wording
 * readers already know — the current value the server sends. Any other source
 * is named exactly as the server reported it, so the page never invents a
 * command name, and never edits one into a shape it did not arrive in.
 */
function evidenceLabel(source: string): string {
  return source === 'gh_pr_create' ? 'gh pr create' : source;
}

/** The distinct sources behind these rows, in the order they appear. */
function evidenceSources(pullRequests: SessionPullRequest[]): string[] {
  return [...new Set(pullRequests.map((pr) => pr.source))];
}

/**
 * The pull requests detected in this session's stored tool calls, under the
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
  const labels = evidenceSources(pullRequests).map(evidenceLabel);
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
      {/*
        Rows always carry a non-empty source, so an empty label list means an
        empty list: skip the sentence rather than render "detected from ;".
        The caller gates on length today; the prop contract holds either way.
      */}
      {labels.length > 0 && (
        <p className="mt-1 text-[14px] leading-5 text-ink-3">
          detected from <span className="font-mono">{labels.join(', ')}</span>; not verified by village.
        </p>
      )}
    </section>
  );
}
