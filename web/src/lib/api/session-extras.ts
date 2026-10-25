import {
  decodeLocalAnswer,
  localRefusal,
  readLocalAnswer,
  type LocalContract,
} from './validated-local';

/**
 * The two additive reads beside a stored session:
 *
 *   - GET /api/v1/sessions/{id}/commits        the commits the session produced
 *   - GET /api/v1/sessions/{id}/pull-requests  pull requests a recorded
 *                                              `gh pr create` command created
 *
 * Both are display-only. Neither widens who may read a transcript, and pull
 * request detection reads stored tool calls — it never contacts GitHub. A
 * caller that cannot get an answer simply renders without these extras.
 */

/** One commit recorded for a session (`session_commits`, incl. linked worktrees). */
export interface SessionCommit {
  hash: string;
  message: string;
  authorName: string;
  authorEmail: string;
  /** committer date, Unix milliseconds */
  commitTime: number;
  /** author date, Unix milliseconds */
  authorTime: number;
}

/** One pull request detected in a session's stored tool calls. */
export interface DetectedPullRequest {
  owner: string;
  repo: string;
  number: number;
  /** the GitHub pull request URL the recorded command result carried */
  url: string;
  /** the command that supplied the evidence; the server always sends `gh_pr_create` */
  source: string;
}

function sessionCommitsPath(sessionId: string): string {
  return `/api/v1/sessions/${encodeURIComponent(sessionId)}/commits`;
}

function sessionPullRequestsPath(sessionId: string): string {
  return `/api/v1/sessions/${encodeURIComponent(sessionId)}/pull-requests`;
}

/** A refusal, an outage, or a response outside these shapes; never a partial row. */
function malformed(issue: string) {
  return { success: false as const, error: { issues: [{ message: issue }] } };
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function isField(value: unknown): value is string {
  return typeof value === 'string';
}

/**
 * A commit's timestamps arrive as JSON numbers but are int64 millis on the
 * contract. Only safe integers are accepted, so widening them to the
 * transcript's bigint times can never lose precision.
 */
function isSafeInteger(value: unknown): value is number {
  return typeof value === 'number' && Number.isSafeInteger(value);
}

function isSessionCommit(value: unknown): value is SessionCommit {
  return (
    isRecord(value) &&
    isField(value.hash) &&
    isField(value.message) &&
    isField(value.authorName) &&
    isField(value.authorEmail) &&
    isSafeInteger(value.commitTime) &&
    isSafeInteger(value.authorTime)
  );
}

/**
 * The row renders this URL as an external link, so it has to be a pull request
 * on GitHub: a row that says "detected pull request" must never carry an
 * insecure or non-GitHub destination. The path shape is the one GitHub serves
 * (`/owner/repo/pull/number`); anything else is outside the contract.
 */
function isGithubPullRequestUrl(value: unknown): value is string {
  if (typeof value !== 'string') return false;
  let url: URL;
  try {
    url = new URL(value);
  } catch {
    return false;
  }
  return (
    url.protocol === 'https:' &&
    url.hostname === 'github.com' &&
    /^\/[^/]+\/[^/]+\/pull\/[0-9]+$/.test(url.pathname)
  );
}

function isDetectedPullRequest(value: unknown): value is DetectedPullRequest {
  return (
    isRecord(value) &&
    isField(value.owner) &&
    isField(value.repo) &&
    isSafeInteger(value.number) &&
    value.number > 0 &&
    isGithubPullRequestUrl(value.url) &&
    isField(value.source)
  );
}

const sessionCommitsContract: LocalContract<{ commits: SessionCommit[] }> = {
  safeParse(value) {
    if (!isRecord(value) || !Array.isArray(value.commits)) {
      return malformed('the answer has no "commits" array');
    }
    const invalidRow = value.commits.findIndex((row) => !isSessionCommit(row));
    if (invalidRow !== -1) {
      return malformed(`commits[${invalidRow}] is not a commit with hash, message, author, and millisecond timestamps`);
    }
    return { success: true, data: { commits: value.commits } };
  },
};

const sessionPullRequestsContract: LocalContract<{ pullRequests: DetectedPullRequest[] }> = {
  safeParse(value) {
    if (!isRecord(value) || !Array.isArray(value.pullRequests)) {
      return malformed('the answer has no "pullRequests" array');
    }
    const invalidRow = value.pullRequests.findIndex((row) => !isDetectedPullRequest(row));
    if (invalidRow !== -1) {
      return malformed(
        `pullRequests[${invalidRow}] is not a pull request with owner, repo, a positive number, `
        + 'a GitHub pull request https URL, and source',
      );
    }
    return { success: true, data: { pullRequests: value.pullRequests } };
  },
};

/** The commits stored for one session; empty when it produced none. */
export async function fetchSessionCommits(sessionId: string, signal?: AbortSignal): Promise<SessionCommit[]> {
  const path = sessionCommitsPath(sessionId);
  const answer = await readLocalAnswer(path, { signal });
  if (!answer.ok) throw localRefusal(path, answer);
  return decodeLocalAnswer(path, answer, sessionCommitsContract).commits;
}

/** The pull requests detected in one session; empty when none were detected. */
export async function fetchSessionPullRequests(
  sessionId: string,
  signal?: AbortSignal,
): Promise<DetectedPullRequest[]> {
  const path = sessionPullRequestsPath(sessionId);
  const answer = await readLocalAnswer(path, { signal });
  if (!answer.ok) throw localRefusal(path, answer);
  return decodeLocalAnswer(path, answer, sessionPullRequestsContract).pullRequests;
}
