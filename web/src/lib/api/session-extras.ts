import {
  zSessionCommitsResponse,
  zSessionPullRequestsResponse,
  type CommitInfo,
  type SessionPullRequest,
} from '@peasant-labs/schema';
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
 *
 * The rows decode through the generated Local API contract. Two local
 * hardening checks stay here because the contract does not carry them: a pull
 * request URL must be a GitHub pull request over https (the row renders it as
 * an external link), and a commit's millisecond times must arrive as JSON safe
 * integers (the contract's int64 fields would otherwise coerce a string or
 * fraction into an instant the local read never sent).
 */

export type { CommitInfo, SessionPullRequest };

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

type LocalParseResult<T> = ReturnType<LocalContract<T>['safeParse']>;

/** Render a schema issue path as `pullRequests[0].owner`. */
function formatIssuePath(path: readonly PropertyKey[]): string {
  return path.reduce<string>((text, segment) => {
    if (typeof segment === 'number') return `${text}[${segment}]`;
    const name = String(segment);
    return text ? `${text}.${name}` : name;
  }, '');
}

/**
 * Prefix each generated issue with its location. The contract's own message
 * says what is wrong with a value; without the path a reader cannot tell which
 * row or field the page refused.
 */
function locateIssues<T>(
  result: { success: true; data: T }
    | { success: false; error: { issues: readonly { path: readonly PropertyKey[]; message: string }[] } },
): LocalParseResult<T> {
  if (result.success) return result;
  return {
    success: false,
    error: {
      issues: result.error.issues.map((issue) => {
        const location = formatIssuePath(issue.path);
        return { message: location ? `${location}: ${issue.message}` : issue.message };
      }),
    },
  };
}

/**
 * A commit's timestamps arrive as JSON numbers but are int64 millis on the
 * contract. Only safe integers are accepted, so the generated bigint coercion
 * can never turn a value the local read did not send into an instant.
 */
function isSafeInteger(value: unknown): value is number {
  return typeof value === 'number' && Number.isSafeInteger(value);
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

const sessionCommitsContract: LocalContract<{ commits: CommitInfo[] }> = {
  safeParse(value) {
    if (!isRecord(value) || !Array.isArray(value.commits)) {
      return malformed('the answer has no "commits" array');
    }
    const unsafeTimeRow = value.commits.findIndex((row) =>
      isRecord(row) && (!isSafeInteger(row.commitTime) || !isSafeInteger(row.authorTime)));
    if (unsafeTimeRow !== -1) {
      return malformed(
        `commits[${unsafeTimeRow}] is not a commit with hash, message, author, and millisecond timestamps`,
      );
    }
    return locateIssues(zSessionCommitsResponse.safeParse(value));
  },
};

const sessionPullRequestsContract: LocalContract<{ pullRequests: SessionPullRequest[] }> = {
  safeParse(value) {
    if (!isRecord(value) || !Array.isArray(value.pullRequests)) {
      return malformed('the answer has no "pullRequests" array');
    }
    const invalidUrlRow = value.pullRequests.findIndex((row) =>
      isRecord(row) && !isGithubPullRequestUrl(row.url));
    if (invalidUrlRow !== -1) {
      return malformed(
        `pullRequests[${invalidUrlRow}] is not a pull request with owner, repo, a positive number, `
        + 'a GitHub pull request https URL, and source',
      );
    }
    return locateIssues(zSessionPullRequestsResponse.safeParse(value));
  },
};

/** The commits stored for one session; empty when it produced none. */
export async function fetchSessionCommits(sessionId: string, signal?: AbortSignal): Promise<CommitInfo[]> {
  const path = sessionCommitsPath(sessionId);
  const answer = await readLocalAnswer(path, { signal });
  if (!answer.ok) throw localRefusal(path, answer);
  return decodeLocalAnswer(path, answer, sessionCommitsContract).commits;
}

/** The pull requests detected in one session; empty when none were detected. */
export async function fetchSessionPullRequests(
  sessionId: string,
  signal?: AbortSignal,
): Promise<SessionPullRequest[]> {
  const path = sessionPullRequestsPath(sessionId);
  const answer = await readLocalAnswer(path, { signal });
  if (!answer.ok) throw localRefusal(path, answer);
  return decodeLocalAnswer(path, answer, sessionPullRequestsContract).pullRequests;
}
