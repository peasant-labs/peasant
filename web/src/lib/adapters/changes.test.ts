import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { adaptChanges, adaptChangeDetail, adaptChangeDiff } from './changes';
import type { DecodedChangeDetailPayload, DecodedChangeDiffPayload, DecodedReviewListPayload } from '@/lib/api/map';
import { parseStrictYAML, requireExactRequiredFields, requireRecord, requireUniqueNames } from '@/test/strictYaml';
import { graphAdapterContractFixture } from '@/test/fixtures/graphAdapterContract';

type DiffCase = { name: string; payload: DecodedChangeDiffPayload };
type ReviewAdapterFixture = {
  reviewList: DecodedReviewListPayload;
  emptyReviewList: DecodedReviewListPayload;
  longCommitCount: number;
  detail: DecodedChangeDetailPayload;
  filesChanged: number;
  diffCases: DiffCase[];
};

function loadReviewAdapterFixture(): ReviewAdapterFixture {
  const manifest = requireRecord(parseStrictYAML(
    readFileSync(resolve(process.cwd(), 'src/lib/adapters/testdata/review_adapter.manifest.yaml'), 'utf8'),
    'review adapter manifest',
  ), 'review adapter manifest');
  requireExactRequiredFields(manifest, ['expectedRootFields', 'expectedDiffCount', 'requiredDiffNames', 'expectedReviewListCounts'], 'review adapter manifest');
  const root = requireRecord(parseStrictYAML(
    readFileSync(resolve(process.cwd(), 'src/lib/adapters/testdata/review_adapter.yaml'), 'utf8'),
    'review adapter fixture',
  ), 'review adapter fixture');
  if (!Array.isArray(manifest.expectedRootFields) || JSON.stringify(Object.keys(root)) !== JSON.stringify(manifest.expectedRootFields)) throw new Error('review adapter fixture root fields do not match their independent manifest');
  requireExactRequiredFields(root, manifest.expectedRootFields as string[], 'review adapter fixture');
  const reviewList = requireRecord(root.reviewList, 'review adapter fixture.reviewList');
  const counts = requireRecord(manifest.expectedReviewListCounts, 'review adapter manifest.expectedReviewListCounts');
  for (const field of ['changes', 'recentCommits', 'sessions']) {
    if (!Array.isArray(reviewList[field]) || reviewList[field].length !== counts[field]) throw new Error(`review adapter fixture ${field} count does not match its independent manifest`);
  }
  if (!Array.isArray(root.diffCases)) throw new Error('review adapter fixture diffCases must be an array');
  const diffCases = root.diffCases.map((row, index) => requireRecord(row, `review adapter fixture.diffCases[${index}]`));
  requireUniqueNames(diffCases, 'review adapter fixture.diffCases');
  diffCases.forEach((row, index) => requireExactRequiredFields(row, ['name', 'payload'], `review adapter fixture.diffCases[${index}]`));
  if (diffCases.length !== manifest.expectedDiffCount || JSON.stringify(diffCases.map((row) => row.name)) !== JSON.stringify(manifest.requiredDiffNames)) throw new Error('review adapter diff inventory does not match its independent manifest');
  if (!Number.isSafeInteger(root.longCommitCount) || !Number.isSafeInteger(root.filesChanged)) throw new Error('review adapter fixture counts must be safe integers');
  return root as unknown as ReviewAdapterFixture;
}

const fixture = loadReviewAdapterFixture();

describe('review adapters', () => {
  it('passes the complete review list through while omitting routing-only projectHash', () => {
    const adapted = adaptChanges(fixture.reviewList);
    expect(adapted).toEqual({
      repoFound: fixture.reviewList.repoFound,
      defaultBranch: fixture.reviewList.defaultBranch,
      changes: fixture.reviewList.changes,
      recentCommits: fixture.reviewList.recentCommits,
      sessions: fixture.reviewList.sessions,
      rewrittenCommits: fixture.reviewList.rewrittenCommits,
    });
  });

  it('preserves the exact empty non-repository shape', () => {
    expect(adaptChanges(fixture.emptyReviewList)).toEqual({ repoFound: false, defaultBranch: undefined, changes: [], recentCommits: [], sessions: [], rewrittenCommits: [] });
  });

  it('preserves the fixture-sized long commit strip', () => {
    const input = {
      ...fixture.reviewList,
      recentCommits: Array.from({ length: fixture.longCommitCount }, (_, index) => ({ hash: `c${index}`, subject: `commit ${index}`, timeMs: 1_700_000_000_000 - index * 3_600_000, hasSession: false, sessionIds: [], associations: [] })),
    };
    expect(adaptChanges(input).recentCommits).toEqual(input.recentCommits);
  });

  it('forwards non-empty rewrite evidence from the shared contract fixture unchanged', () => {
    const review = {
      ...fixture.reviewList,
      rewrittenCommits: graphAdapterContractFixture.rewrittenCommits,
    };
    expect(adaptChanges(review).rewrittenCommits).toEqual(graphAdapterContractFixture.rewrittenCommits);
  });

  it('forwards non-empty insight evidence from the shared contract fixture unchanged', () => {
    const detail = {
      ...fixture.detail,
      insights: graphAdapterContractFixture.insights,
    };
    expect(adaptChangeDetail(detail, fixture.filesChanged).insights).toEqual(graphAdapterContractFixture.insights);
  });

  it('adds only the independently supplied filesChanged count to change detail', () => {
    expect(adaptChangeDetail(fixture.detail, fixture.filesChanged)).toEqual({ ...fixture.detail, filesChanged: fixture.filesChanged });
    expect(fixture.detail).not.toHaveProperty('filesChanged');
  });

  for (const row of fixture.diffCases) {
    it(row.name, () => {
      expect(adaptChangeDiff(row.payload)).toEqual({
        ...row.payload,
        oldPath: row.payload.oldPath ?? null,
        hunks: row.payload.hunks.map((hunk) => ({ ...hunk })),
      });
    });
  }
});
