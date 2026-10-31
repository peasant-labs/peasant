import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { parseStrictYAML, requireExactRequiredFields, requireRecord, requireUniqueNames } from '@/test/strictYaml';
import {
  fetchSessionCommits,
  fetchSessionPullRequestsResponse,
  type CommitInfo,
} from './session-extras';
import type { SessionPullRequestsResponse } from '@peasant-labs/schema';

// The two additive session reads are display-only, so the boundary that matters
// is what the page does with an answer it cannot use: decode the rows, or refuse
// the whole answer rather than render a partial commit or pull request.
const SESSION_ID = 'sess-extras-1';
const caseFields = ['name', 'endpoint', 'status', 'body', 'expect'] as const;
const endpoints = ['commits', 'pull-requests'] as const;
const outcomes = ['rows', 'refusal', 'contract'] as const;

type SessionExtrasCase = {
  name: string;
  endpoint: (typeof endpoints)[number];
  status: number;
  body: string;
  expect: (typeof outcomes)[number];
};

const fixtures = parseStrictYAML(
  readFileSync(resolve('src/lib/api/testdata/session-extras.yaml'), 'utf8'),
  'session extras cases',
);
const root = requireRecord(fixtures, 'session extras cases');
requireExactRequiredFields(root, ['requiredNames', 'cases'], 'session extras cases');
if (!Array.isArray(root.requiredNames) || !Array.isArray(root.cases)) {
  throw new Error('session extras cases requires a requiredNames list and a cases list');
}
const cases = root.cases.map((row, index) => {
  const entry = requireRecord(row, `session extras cases[${index}]`);
  requireExactRequiredFields(entry, caseFields, `session extras cases[${index}]`);
  if (!Number.isSafeInteger(entry.status)) throw new Error(`session extras cases[${index}].status must be an integer`);
  if (typeof entry.body !== 'string') throw new Error(`session extras cases[${index}].body must be a string`);
  if (!endpoints.includes(entry.endpoint as (typeof endpoints)[number])) {
    throw new Error(`session extras cases[${index}].endpoint is outside the supported reads`);
  }
  if (!outcomes.includes(entry.expect as (typeof outcomes)[number])) {
    throw new Error(`session extras cases[${index}].expect is outside the supported outcomes`);
  }
  return entry as SessionExtrasCase;
});
requireUniqueNames(cases, 'session extras cases');

function answer(caseRow: SessionExtrasCase): Promise<CommitInfo[] | SessionPullRequestsResponse> {
  return caseRow.endpoint === 'commits'
    ? fetchSessionCommits(SESSION_ID)
    : fetchSessionPullRequestsResponse(SESSION_ID);
}

/** One raw commit as the local read carries it: millisecond times are JSON numbers. */
type RawCommit = Omit<CommitInfo, 'commitTime' | 'authorTime'> & {
  commitTime: number;
  authorTime: number;
};

/** The decoded rows a `rows` case must produce: commit times are the contract's bigint. */
function expectedRows(caseRow: SessionExtrasCase): CommitInfo[] | SessionPullRequestsResponse {
  if (caseRow.endpoint === 'commits') {
    const { commits } = JSON.parse(caseRow.body) as { commits: RawCommit[] };
    return commits.map((commit) => ({
      ...commit,
      commitTime: BigInt(commit.commitTime),
      authorTime: BigInt(commit.authorTime),
    }));
  }
  return JSON.parse(caseRow.body) as SessionPullRequestsResponse;
}

afterEach(() => vi.unstubAllGlobals());

describe('session extra reads', () => {
  it('covers every named case', () => {
    expect(new Set(cases.map((caseRow) => caseRow.name))).toEqual(new Set(root.requiredNames as unknown[]));
  });

  for (const caseRow of cases) {
    it(caseRow.name, async () => {
      const fetcher = vi.fn().mockResolvedValue(new Response(caseRow.body, { status: caseRow.status }));
      vi.stubGlobal('fetch', fetcher);

      if (caseRow.expect === 'rows') {
        const rows = await answer(caseRow);
        expect(rows).toEqual(expectedRows(caseRow));
      } else if (caseRow.expect === 'refusal') {
        await expect(answer(caseRow)).rejects.toMatchObject({
          name: 'LocalRequestError',
          status: caseRow.status,
          message: expect.stringContaining('session not found'),
        });
      } else {
        // A shape refusal names either the offending row or the missing array.
        const field = caseRow.endpoint === 'commits' ? 'commits' : '(pullRequests|unverified_pull_requests)';
        await expect(answer(caseRow)).rejects.toMatchObject({
          name: 'LocalRequestError',
          status: caseRow.status,
          message: expect.stringMatching(new RegExp(`${field}(\\[0\\]|" array|:)`)),
        });
      }

      expect(fetcher).toHaveBeenCalledWith(
        expect.stringContaining(`/api/v1/sessions/${SESSION_ID}/${caseRow.endpoint}`),
        expect.objectContaining({ cache: 'no-store' }),
      );
    });
  }
});
