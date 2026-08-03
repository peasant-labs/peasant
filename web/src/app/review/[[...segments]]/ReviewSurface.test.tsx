import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, cleanup, fireEvent, waitFor } from '@testing-library/react';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import {
  parseStrictYAML,
  requireExactRequiredFields,
  requireRecord,
  requireUniqueNames,
} from '@/test/strictYaml';
import { DiscoveryRequestError } from '@/lib/api/errors';
import { ReviewSurface } from './ReviewSurface';
import { timelineNavigationFixture } from './timelineNavigation.fixture';
import { graphAdapterContractFixture } from '@/test/fixtures/graphAdapterContract';
import type { Changes } from '@peasant-labs/fairtrade/graph';

/* ReviewSurface is the DATA layer (project resolution + REST client + loading/error/ready
   orchestration). Keep the real packed <Changes> mounted so route tests exercise its actual
   controls and action objects; stub only <ChangeDetail> where these tests need to inspect the
   host-owned share callback. */

const routerPush = vi.fn();
const PROJECT_HASH = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa';
vi.mock('next/navigation', () => ({ useRouter: () => ({ push: routerPush }) }));

type ChangeDetailProps = {
  payload: {
    work: Array<{ sessionId: string }>;
    insights?: unknown;
  };
  getDiff?: (file: { path: string }) => { error?: string } | null | undefined;
  onShare?: () => void;
};
type ChangesProps = Parameters<typeof Changes>[0];

/** The one file path the mocked <ChangeDetail> asks getDiff() for, mirroring how the
    real lifted component calls getDiff(file) for each open file during its own render.
    Only the diff-fetch test below opts in (requestDiffInMock = true); every other test
    keeps the mock's pre-existing no-getDiff-call behavior so fetchChangeDiff, which most
    tests never mock a resolution for, is never invoked outside that one test. */
const DIFF_TEST_FILE = 'src/example.ts';
let requestDiffInMock = false;

let lastChangeDetailProps: ChangeDetailProps | null = null;
let lastChangesProps: ChangesProps | null = null;
let sessionsError: Error | null = null;
let sessionsErrorCode: 'selection_visibility' | undefined;

vi.mock('@/contexts/WebSocketContext', () => ({
  useChannel: () => ({
    data: { sessions: [] },
    connected: true,
    error: sessionsError,
    errorCode: sessionsErrorCode,
  }),
}));

vi.mock('./sessions', () => ({
  projectNames: () => ['proj'],
  resolveProjectHash: () => 'hash123',
}));

const fetchReviewChanges = vi.fn();
const fetchChangeDetail = vi.fn();
const fetchChangeDiff = vi.fn();
const fetchProjectResolution = vi.fn();
vi.mock('@/lib/api/map', () => ({
  fetchProjectResolution: (...a: unknown[]) => fetchProjectResolution(...a),
  fetchReviewChanges: (...a: unknown[]) => fetchReviewChanges(...a),
  fetchChangeDetail: (...a: unknown[]) => fetchChangeDetail(...a),
  fetchChangeDiff: (...a: unknown[]) => fetchChangeDiff(...a),
}));

vi.mock('@peasant-labs/fairtrade/graph', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@peasant-labs/fairtrade/graph')>();
  return {
    ...actual,
    Changes: (p: Parameters<typeof actual.Changes>[0]) => (
      <div data-testid="changes" data-project={p.projectLabel} ref={() => { lastChangesProps = p; }}>
        <actual.Changes {...p} />
      </div>
    ),
    ChangeDetail: (p: ChangeDetailProps) => {
      lastChangeDetailProps = p;
      const diffResult = requestDiffInMock ? p.getDiff?.({ path: DIFF_TEST_FILE }) : null;
      return (
        <>
          <button type="button" onClick={() => p.onShare?.()}>
            share evidence conversations
          </button>
          {diffResult?.error && <p data-testid="diff-error">{diffResult.error}</p>}
        </>
      );
    },
  };
});

const REVIEW_OK = { repoFound: true, defaultBranch: 'main', changes: [], recentCommits: [], sessions: [] };
const DETAIL_OK = {
  branch: 'feat/x', baseRef: 'abc', defaultBranch: 'main', files: [],
  slice: { nodes: [], structureEdges: [], activityEdges: [] },
  newEdges: [], removedEdges: [], newNodes: [], removedNodes: [], violations: [],
  work: [], unrecordedCommits: [], unusual: [], frictions: [], linesAdded: 0,
  linesRemoved: 0, outputTokens: 0, costUsd: null,
};

const identityFailureSource = readFileSync(
  resolve(process.cwd(), 'src/app/review/[[...segments]]/testdata/project_identity_failures.yaml'),
  'utf8',
);
const requiredIdentityFailureNames = [
  'canonical list renders a missing project',
  'canonical detail renders a missing project',
  'canonical list retries a transient resolution failure',
  'canonical detail retries a transient resolution failure',
] as const;
type IdentityFailureCase = { name: string; view: 'list' | 'detail'; outcome: 'missing' | 'transient'; message: string };

function loadIdentityFailureFixture(source: string): IdentityFailureCase[] {
  const root = requireRecord(parseStrictYAML(source, 'project identity failure fixture'), 'project identity failure fixture');
  requireExactRequiredFields(root, ['expectedCaseCount', 'requiredNames', 'cases'], 'project identity failure fixture');
  if (root.expectedCaseCount !== requiredIdentityFailureNames.length || !Array.isArray(root.requiredNames) || !Array.isArray(root.cases)) {
    throw new Error('project identity failure fixture has an invalid semantic inventory');
  }
  const requiredNames = root.requiredNames;
  if (requiredNames.length !== requiredIdentityFailureNames.length || new Set(requiredNames).size !== requiredNames.length || requiredIdentityFailureNames.some((name) => !requiredNames.includes(name))) {
    throw new Error('project identity failure fixture requiredNames must have exact set equality');
  }
  const cases = root.cases.map((value, index) => requireRecord(value, `project identity failure fixture.cases[${index}]`));
  requireUniqueNames(cases, 'project identity failure fixture.cases');
  cases.forEach((testCase, index) => {
    requireExactRequiredFields(testCase, ['name', 'view', 'outcome', 'message'], `project identity failure fixture.cases[${index}]`);
    if (!requiredNames.includes(testCase.name) || !['list', 'detail'].includes(String(testCase.view)) || !['missing', 'transient'].includes(String(testCase.outcome)) || typeof testCase.message !== 'string' || testCase.message.length === 0) {
      throw new Error(`project identity failure fixture case ${index} is invalid`);
    }
  });
  if (cases.length !== requiredNames.length || requiredNames.some((name) => !cases.some((testCase) => testCase.name === name))) {
    throw new Error('project identity failure fixture cases must have exact set equality');
  }
  return cases as IdentityFailureCase[];
}

const identityFailureCases = loadIdentityFailureFixture(identityFailureSource);

describe('project identity failure fixture contract', () => {
  it('rejects structural and semantic mutations', () => {
    expect(() => loadIdentityFailureFixture(identityFailureSource.replace('expectedCaseCount: 4', 'unknown: true\nexpectedCaseCount: 4'))).toThrow();
    expect(() => loadIdentityFailureFixture(`${identityFailureSource}\n---\n{}`)).toThrow();
    expect(() => loadIdentityFailureFixture(identityFailureSource.replace('canonical detail renders a missing project', 'renamed detail behavior'))).toThrow(/exact set equality/);
    expect(() => loadIdentityFailureFixture(identityFailureSource.replace(/  - \{name: canonical detail renders a missing project[^\n]+\n/, ''))).toThrow(/exact set equality/);
  });
});

beforeEach(() => {
  routerPush.mockClear();
  fetchReviewChanges.mockReset();
  fetchChangeDetail.mockReset();
  fetchChangeDiff.mockReset();
  fetchProjectResolution.mockReset();
  fetchProjectResolution.mockResolvedValue({ project: 'proj', projectHash: PROJECT_HASH });
  lastChangeDetailProps = null;
  lastChangesProps = null;
  sessionsError = null;
  sessionsErrorCode = undefined;
  requestDiffInMock = false;
});
afterEach(() => cleanup());

describe('ReviewSurface — fetch states', () => {
  for (const testCase of identityFailureCases) {
    it(`identity: ${testCase.name}`, async () => {
      const branch = testCase.view === 'detail' ? 'feat/x' : null;
      if (testCase.outcome === 'missing') {
        fetchProjectResolution.mockRejectedValueOnce(new DiscoveryRequestError('/api/v1/projects/resolve', 404, testCase.message));
      } else {
        fetchProjectResolution.mockRejectedValueOnce(new Error(testCase.message));
      }
      fetchReviewChanges.mockResolvedValue(REVIEW_OK);
      fetchChangeDetail.mockResolvedValue(DETAIL_OK);
      render(<ReviewSurface projectHash={PROJECT_HASH as never} branch={branch} />);

      expect(await screen.findByRole('alert')).toHaveTextContent(testCase.message);
      expect(fetchReviewChanges).not.toHaveBeenCalled();
      expect(fetchChangeDetail).not.toHaveBeenCalled();
      expect(fetchChangeDiff).not.toHaveBeenCalled();
      expect(screen.queryByText('proj')).not.toBeInTheDocument();

      if (testCase.outcome === 'transient') {
        fetchProjectResolution.mockResolvedValueOnce({ project: 'resolved project label', projectHash: PROJECT_HASH });
        fireEvent.click(screen.getByRole('button', { name: /retry/i }));
        if (testCase.view === 'list') {
          const changes = await screen.findByTestId('changes');
          expect(changes).toHaveAttribute('data-project', 'resolved project label');
          expect(fetchReviewChanges).toHaveBeenCalledTimes(1);
        } else {
          await waitFor(() => expect(fetchChangeDetail).toHaveBeenCalledTimes(1));
        }
        expect(fetchProjectResolution).toHaveBeenCalledTimes(2);
      }
    });
  }

  it('list: a failed review-list fetch renders an actionable error (not a spinner) + retry', async () => {
    fetchReviewChanges.mockRejectedValueOnce(new Error(`GET /api/v1/review/${PROJECT_HASH} failed (500): boom`));
    render(<ReviewSurface projectHash={PROJECT_HASH as never} branch={null} />);
    expect(await screen.findByText(/couldn.t load this project.s changes/i)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /retry/i })).toBeInTheDocument();
    // it must NOT be sitting on the loading spinner
    expect(screen.queryByText(/loading changes/i)).not.toBeInTheDocument();
  });

  // Pins ReviewSurface to the SAME shared web/src/lib/selectionGuidance.ts
  // discoveryErrorMessage() renderer Home (page.test.tsx) and Map
  // (MapRouter.tsx) use, not a locally re-implemented message fallback. The
  // server error message here deliberately does NOT already contain
  // "peasant kickstart" (unlike the real production Go error text), so the
  // guidance can only appear in the rendered output if discoveryErrorMessage
  // itself appended it for the selection_visibility code — proving the
  // shared renderer actually ran, not just that the server happened to say
  // the same thing. A future regression that reverts either call site back
  // to a local `error.message` fallback would fail this test even though the
  // real backend's baked-in text would currently still "look right".
  it('list: a selection-visibility failure renders through the shared discoveryErrorMessage renderer', async () => {
    fetchReviewChanges.mockRejectedValueOnce(new DiscoveryRequestError(
      `/api/v1/review/${PROJECT_HASH}`,
      500,
      'project discovery failed while applying saved selection',
      'selection_visibility',
    ));
    render(<ReviewSurface projectHash={PROJECT_HASH as never} branch={null} />);
    const alert = await screen.findByText(/couldn.t load this project.s changes/i);
    expect(alert.parentElement).toHaveTextContent(/peasant kickstart/);
  });

  it('detail: a selection-visibility failure renders through the shared discoveryErrorMessage renderer', async () => {
    fetchReviewChanges.mockResolvedValue(REVIEW_OK);
    fetchChangeDetail.mockRejectedValueOnce(new DiscoveryRequestError(
      `/api/v1/review/${PROJECT_HASH}/change`,
      500,
      'project discovery failed while applying saved selection',
      'selection_visibility',
    ));
    render(<ReviewSurface projectHash={PROJECT_HASH as never} branch="feat/x" />);
    const alert = await screen.findByText(/couldn.t load this change/i);
    expect(alert.parentElement).toHaveTextContent(/peasant kickstart/);
  });

  // Pins the third (and last) discoveryErrorMessage() call site — the lazy
  // per-file diff fetch (ReviewSurface.tsx's getDiff callback) — separately
  // from the list/detail cases above: each catch handler is an independent
  // call site a future edit could revert on its own, and neither of the
  // other two tests exercises this one. The mocked <ChangeDetail> calls the
  // real getDiff(file) it was handed (mirroring the lifted package), so the
  // error sentinel it renders comes from ReviewSurface's actual diffErrors
  // state, not a test-only stand-in.
  it('diff: a selection-visibility failure renders through the shared discoveryErrorMessage renderer', async () => {
    requestDiffInMock = true;
    fetchReviewChanges.mockResolvedValue(REVIEW_OK);
    fetchChangeDetail.mockResolvedValueOnce(DETAIL_OK);
    fetchChangeDiff.mockRejectedValueOnce(new DiscoveryRequestError(
      `/api/v1/review/${PROJECT_HASH}/diff`,
      500,
      'project discovery failed while applying saved selection',
      'selection_visibility',
    ));
    render(<ReviewSurface projectHash={PROJECT_HASH as never} branch="feat/x" />);
    const diffError = await screen.findByTestId('diff-error');
    expect(diffError).toHaveTextContent(/peasant kickstart/);
    expect(fetchChangeDiff).toHaveBeenCalledWith(PROJECT_HASH, 'feat/x', DIFF_TEST_FILE);
  });

  it('list: retry re-fetches and recovers to the changes surface', async () => {
    fetchReviewChanges.mockRejectedValueOnce(new Error('boom'));
    render(<ReviewSurface projectHash={PROJECT_HASH as never} branch={null} />);
    const retry = await screen.findByRole('button', { name: /retry/i });
    fetchReviewChanges.mockResolvedValueOnce(REVIEW_OK);
    fireEvent.click(retry);
    expect(await screen.findByTestId('changes')).toBeInTheDocument();
    expect(fetchReviewChanges).toHaveBeenCalledTimes(2);
  });

  it('list: a successful fetch renders the lifted <Changes>', async () => {
    fetchReviewChanges.mockResolvedValueOnce(REVIEW_OK);
    render(<ReviewSurface projectHash={PROJECT_HASH as never} branch={null} />);
    const changes = await screen.findByTestId('changes');
    expect(changes).toHaveAttribute('data-project', 'proj');
    expect(screen.queryByText(PROJECT_HASH)).not.toBeInTheDocument();
  });

  it('mounted ReviewSurface forwards non-empty rewrite and insight evidence to Fairtrade props', async () => {
    const rewrittenCommit = graphAdapterContractFixture.rewrittenCommits[0];
    fetchReviewChanges.mockResolvedValueOnce({
      ...REVIEW_OK,
      recentCommits: [{
        hash: rewrittenCommit.successorHash,
        subject: 'resolved rewrite successor',
        timeMs: rewrittenCommit.authorTimeMs,
        hasSession: true,
        sessionIds: rewrittenCommit.sessionIds,
        associations: rewrittenCommit.associations,
      }],
      sessions: [{
        sessionId: rewrittenCommit.sessionIds[0],
        title: 'rewrite evidence session',
        harness: 'claude-code',
        startMs: rewrittenCommit.authorTimeMs,
        hasCommitBinding: true,
      }],
      rewrittenCommits: graphAdapterContractFixture.rewrittenCommits,
    });
    render(<ReviewSurface projectHash={PROJECT_HASH as never} branch={null} />);
    await screen.findByTestId('changes');
    expect(lastChangesProps?.payload.rewrittenCommits).toEqual(graphAdapterContractFixture.rewrittenCommits);

    cleanup();
    fetchReviewChanges.mockResolvedValueOnce({
      ...REVIEW_OK,
      changes: [{ branch: 'feat/x', filesChanged: 2 }],
    });
    fetchChangeDetail.mockResolvedValueOnce({ ...DETAIL_OK, insights: graphAdapterContractFixture.insights });
    render(<ReviewSurface projectHash={PROJECT_HASH as never} branch="feat/x" />);
    await screen.findByRole('button', { name: /share evidence conversations/i });
    expect(lastChangeDetailProps?.payload.insights).toEqual(graphAdapterContractFixture.insights);
  });

  it('list: a sessions-channel policy error does not block an explicit canonical project route', async () => {
    sessionsError = new Error('selection policy failed');
    sessionsErrorCode = 'selection_visibility';
    fetchReviewChanges.mockResolvedValue(REVIEW_OK);
    render(<ReviewSurface projectHash={PROJECT_HASH as never} branch={null} />);
    expect(await screen.findByTestId('changes')).toBeInTheDocument();
    expect(fetchReviewChanges).toHaveBeenCalledTimes(1);
  });

  for (const testCase of timelineNavigationFixture.cases.filter(
    (candidate) => candidate.mountedReview !== null,
  )) {
    it(`list: ${testCase.name}`, async () => {
      fetchReviewChanges.mockResolvedValueOnce(testCase.mountedReview);
      render(
        <ReviewSurface
          projectHash={PROJECT_HASH as never}
          branch={null}
          returnLocation={testCase.context.returnLocation}
        />,
      );
      fireEvent.click(await screen.findByRole('button', { name: testCase.controlName! }));
      expect(routerPush.mock.calls).toEqual(testCase.expectedRouterCalls);
    });
  }

  it('detail: a failed change-detail fetch renders an actionable error + retry', async () => {
    fetchReviewChanges.mockResolvedValue(REVIEW_OK);
    fetchChangeDetail.mockRejectedValueOnce(new Error('GET .../change?branch=feat/x failed (404)'));
    render(<ReviewSurface projectHash={PROJECT_HASH as never} branch="feat/x" />);
    expect(await screen.findByText(/couldn.t load this change/i)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /retry/i })).toBeInTheDocument();
    expect(screen.queryByText(/loading the change/i)).not.toBeInTheDocument();
  });

  it('detail: share routes to /share with distinct session ids from the adapted work payload', async () => {
    fetchReviewChanges.mockResolvedValueOnce({
      ...REVIEW_OK,
      changes: [{ branch: 'feat/x', filesChanged: 2 }],
    });
    fetchChangeDetail.mockResolvedValueOnce({
      branch: 'feat/x',
      baseRef: 'abc',
      defaultBranch: 'main',
      files: [],
      slice: { nodes: [], structureEdges: [], activityEdges: [] },
      newEdges: [],
      removedEdges: [],
      newNodes: [],
      removedNodes: [],
      violations: [],
      work: [
        { sessionId: 'sess-b', title: 'b', harness: 'claude-code', binding: 'bound', tasks: [] },
        { sessionId: 'sess-a', title: 'a', harness: 'claude-code', binding: 'candidate', tasks: [] },
        { sessionId: 'sess-b', title: 'dup', harness: 'claude-code', binding: 'candidate', tasks: [] },
      ],
      unrecordedCommits: [],
      unusual: [],
      frictions: [],
      linesAdded: 0,
      linesRemoved: 0,
      outputTokens: 0,
      costUsd: null,
    });

    render(<ReviewSurface projectHash={PROJECT_HASH as never} branch="feat/x" />);

    const share = await screen.findByRole('button', { name: /share evidence conversations/i });
    expect(lastChangeDetailProps?.payload.work.map((ws) => ws.sessionId)).toEqual([
      'sess-b',
      'sess-a',
      'sess-b',
    ]);

    fireEvent.click(share);
    expect(routerPush).toHaveBeenCalledWith('/share?sessions=sess-b,sess-a');
  });
});
