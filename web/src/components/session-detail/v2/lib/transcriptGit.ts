import type { CommitInfo } from '@peasant-labs/schema';

/**
 * The transcript's git compatibility shape (`payload.gitContext`), which is the
 * one seam `adaptTranscript` still normalizes: it cooks these rows into
 * `session.git.commits`, which is what lights up the viewer's header commit
 * count and its per-turn checkpoint markers.
 *
 * The local commits read decodes to the same `CommitInfo` the contract carries,
 * so the times are already bigint at this boundary; only the unknown-time
 * omission remains.
 *
 * Returns `undefined` — not an empty object — when the session produced no
 * commits, so the viewer renders neither a count of zero nor a checkpoint
 * cluster. A session with no commits is unchanged by this feature.
 */
export function toTranscriptGitContext(
  commits: CommitInfo[],
): { commits: CommitInfo[] } | undefined {
  if (commits.length === 0) return undefined;
  return { commits: commits.map(toTranscriptCommit) };
}

/** A commit row whose times may be absent, which is how "never recorded" travels. */
type UndatedCommit = Omit<CommitInfo, 'commitTime' | 'authorTime'> & {
  commitTime?: bigint;
  authorTime?: bigint;
};

/**
 * One local commit row as the viewer's compatibility seam accepts it.
 *
 * `commit_time` and `author_time` are nullable in `session_commits`, and the
 * store reads a NULL back as the zero value, so a row whose time is 0 has no
 * recorded time — it is not midnight of 1970. The distinction decides where the
 * row lands: the adapter reads a PRESENT `commitTime` as an instant and anchors
 * the commit to the last turn that started before it, which would put an
 * unknown-time commit inside turn 0; an ABSENT `commitTime` leaves the commit
 * unattributed, which the viewer renders as its trailing unanchored group. So
 * both time fields are omitted when they are 0, and the hash, message, and
 * author fields always travel.
 *
 * The published `CommitInfo` type still requires both int64 times, because the
 * durable wire carries them; this read can hold "unknown". The assertion is
 * that deliberate narrowness, kept at the single boundary where the local read
 * meets the viewer's wire input.
 */
function toTranscriptCommit(commit: CommitInfo): CommitInfo {
  const row: UndatedCommit = {
    hash: commit.hash,
    message: commit.message,
    authorName: commit.authorName,
    authorEmail: commit.authorEmail,
  };
  // The project targets a pre-ES2020 lib, so the zero instant is built rather
  // than written as a bigint literal.
  const unrecorded = BigInt(0);
  if (commit.commitTime !== unrecorded) row.commitTime = commit.commitTime;
  if (commit.authorTime !== unrecorded) row.authorTime = commit.authorTime;
  return row as CommitInfo;
}
