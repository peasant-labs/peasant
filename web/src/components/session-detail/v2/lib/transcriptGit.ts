import type { CommitInfo } from '@peasant-labs/fairtrade/ui';
import type { SessionCommit } from '@/lib/api/session-extras';

/**
 * The transcript's git compatibility shape (`payload.gitContext`), which is the
 * one seam `adaptTranscript` still normalizes: it cooks these rows into
 * `session.git.commits`, which is what lights up the viewer's header commit
 * count and its per-turn checkpoint markers.
 *
 * The local commits read carries the same fields as JSON numbers; the contract's
 * int64 times are bigint, so they are widened here at the single boundary
 * between the local read and the viewer's wire input.
 *
 * Returns `undefined` — not an empty object — when the session produced no
 * commits, so the viewer renders neither a count of zero nor a checkpoint
 * cluster. A session with no commits is unchanged by this feature.
 */
export function toTranscriptGitContext(
  commits: SessionCommit[],
): { commits: CommitInfo[] } | undefined {
  if (commits.length === 0) return undefined;
  return {
    commits: commits.map((commit) => ({
      hash: commit.hash,
      message: commit.message,
      authorName: commit.authorName,
      authorEmail: commit.authorEmail,
      commitTime: BigInt(commit.commitTime),
      authorTime: BigInt(commit.authorTime),
    })),
  };
}
