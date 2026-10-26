import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import { parseTranscriptRouteQuery, type ProjectHash } from '@/lib/navigation/projectRoutes';
import { PublishProvider } from '@/contexts/PublishContext';
import { SessionDetailV2 } from './SessionDetailV2';

// The session's commits ride into the shared viewer's own git shape, so this
// suite mounts the REAL viewer and asserts what a reader sees: the header commit
// count, a checkpoint marker at the turn the commit belongs to, and the
// detected-pull-request list under the meta row — including when either read
// fails, because these extras are additive.

const PROJECT_HASH = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' as ProjectHash;
const SESSION_ID = 'sess-extras-ui';
const PATHNAME = `/projects/alpha-project/${SESSION_ID}`;

const COMMIT = {
  hash: '9f3c1ad0aa11',
  message: 'feat(canvas): port TurnRow',
  authorName: 'Ada Lovelace',
  authorEmail: 'ada@example.test',
  commitTime: Date.parse('2026-07-16T08:01:00Z'),
  authorTime: Date.parse('2026-07-16T08:01:00Z'),
};

/** The commit whose time the store never recorded, so it reads back as 0. */
const UNDATED_COMMIT = {
  hash: '1b2c3d4e5f60',
  message: 'fix(api): keep the refusal reason',
  authorName: 'Ada Lovelace',
  authorEmail: 'ada@example.test',
  commitTime: 0,
  authorTime: 0,
};

const PULL_REQUEST = {
  owner: 'storytold',
  repo: 'photocraft',
  number: 42,
  url: 'https://github.com/storytold/photocraft/pull/42',
  source: 'gh_pr_create',
};

let sessionDetailData: unknown;
let qualityData: unknown;
let extras: { commits: { status: number; body: unknown }; pullRequests: { status: number; body: unknown } };

vi.mock('@/lib/share/publishing', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/lib/share/publishing')>()),
  ...(await import('@/test/fixtures/publishingIdle')).PUBLISHING_IDLE,
}));

vi.mock('next/navigation', () => ({
  useSearchParams: () => new URLSearchParams(),
  usePathname: () => PATHNAME,
  useRouter: () => ({ replace: vi.fn(), push: vi.fn() }),
}));
vi.mock('@/contexts/WebSocketContext', () => ({
  useChannel: (subscription: { topic: string }) => ({
    data: subscription.topic === 'session_detail' ? sessionDetailData : qualityData,
    connected: true,
    error: null,
  }),
}));
vi.mock('./lib/useEntryLabels', () => ({
  useEntryLabels: () => ({ entryTypes: [], labelsByEntry: new Map(), addLabel: vi.fn() }),
}));
vi.mock('@/hooks/useTheme', () => ({
  useTheme: () => ({ theme: 'dark', setTheme: vi.fn(), toggle: vi.fn() }),
}));
vi.mock('@peasant-labs/fairtrade/graph', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@peasant-labs/fairtrade/graph')>();
  return { ...actual, TrajectoryGraph: () => null };
});
vi.mock('@peasant-labs/fairtrade/ui', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@peasant-labs/fairtrade/ui')>();
  return { ...actual, annotateTranscript: () => [], computePersonalMedians: () => undefined };
});

function TestDetail() {
  const routeQuery = parseTranscriptRouteQuery(new URLSearchParams());
  if (!routeQuery) throw new Error('session extras route query must be valid');
  return (
    <PublishProvider>
      <SessionDetailV2 sessionId={SESSION_ID} projectHash={PROJECT_HASH} projectName="alpha-project" routeQuery={routeQuery} />
    </PublishProvider>
  );
}

/** Answer the two additive reads; nothing else on this page reaches the Local API. */
function stubLocalApi(): void {
  vi.stubGlobal('fetch', vi.fn((url: string) => {
    const piece = url.includes('/pull-requests') ? extras.pullRequests : extras.commits;
    return Promise.resolve(new Response(JSON.stringify(piece.body), { status: piece.status }));
  }));
}

beforeEach(() => {
  window.localStorage.clear();
  if (typeof HTMLElement.prototype.scrollIntoView !== 'function') {
    Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', { configurable: true, value: () => undefined });
  }
  sessionDetailData = {
    id: SESSION_ID,
    project: 'alpha-project',
    harness: 'claude-code',
    startTime: '2026-07-16T08:00:00Z',
    endTime: '2026-07-16T08:02:00Z',
    durationMins: 2,
    totalTokens: 12,
    tokensIn: 5,
    tokensOut: 7,
    turnCount: 2,
    toolCallCount: 0,
    turns: [
      { index: 0, role: 'user', depth: 0, content: 'add the canvas row', timestamp: '2026-07-16T08:00:00Z', toolCalls: [] },
      { index: 1, role: 'assistant', depth: 0, content: 'the row is ported', timestamp: '2026-07-16T08:00:30Z', toolCalls: [] },
    ],
  };
  qualityData = { sessions: [] };
  extras = { commits: { status: 200, body: { commits: [] } }, pullRequests: { status: 200, body: { pullRequests: [] } } };
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  sessionDetailData = undefined;
  qualityData = undefined;
});

/** True when `later` sits after `earlier` in document order. */
function follows(later: Element, earlier: Element): boolean {
  return (earlier.compareDocumentPosition(later) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0;
}

describe('SessionDetailV2 — session commits and detected pull requests', () => {
  it('renders the commit count, a checkpoint marker, and the detected pull request', async () => {
    extras = {
      commits: { status: 200, body: { commits: [COMMIT] } },
      pullRequests: { status: 200, body: { pullRequests: [PULL_REQUEST] } },
    };
    stubLocalApi();

    const view = render(<TestDetail />);
    await waitFor(() => expect(view.container.querySelector('.txn-title')).toBeInTheDocument());

    await waitFor(() => expect(view.container.querySelector('.txn-checkpoint')).toBeInTheDocument());
    const meta = view.container.querySelector('.txn-meta');
    expect(meta?.textContent).toContain('1 commit');
    expect(view.container.querySelector('.txn-checkpoint')?.textContent).toContain('feat(canvas): port TurnRow');
    expect(view.container.querySelector('.txn-checkpoint')?.textContent).toContain('9f3c1ad');

    await waitFor(() => expect(view.container.querySelector('.txn-prs')).toBeInTheDocument());
    const link = screen.getByRole('link', { name: /storytold\/photocraft #42/i });
    expect(link).toHaveAttribute('href', 'https://github.com/storytold/photocraft/pull/42');
    expect(view.container.querySelector('.txn-prs')).toHaveTextContent('detected from gh pr create; not verified by village.');
  });

  it('shows a commit with no recorded time outside the turns instead of inside turn 0', async () => {
    // A recorded instant between the two turns, so the adapter has an unambiguous
    // anchor for the dated row; the other row has no instant at all.
    const dated = {
      ...COMMIT,
      hash: '9f3c1ad0aa11',
      commitTime: Date.parse('2026-07-16T08:00:10Z'),
      authorTime: Date.parse('2026-07-16T08:00:10Z'),
    };
    extras = {
      commits: { status: 200, body: { commits: [dated, UNDATED_COMMIT] } },
      pullRequests: { status: 200, body: { pullRequests: [] } },
    };
    stubLocalApi();

    const view = render(<TestDetail />);
    await waitFor(() => expect(view.container.querySelector('.txn-title')).toBeInTheDocument());
    await waitFor(() => expect(view.container.querySelectorAll('.txn-checkpoint')).toHaveLength(2));

    // Both commits are still counted: an unknown time hides the marker's anchor,
    // not the commit.
    expect(view.container.querySelector('.txn-meta')?.textContent).toContain('2 commits');

    const turns = [...view.container.querySelectorAll('.txn-turn')];
    expect(turns.length).toBeGreaterThanOrEqual(2);
    const lastTurn = turns[turns.length - 1];
    const checkpoints = [...view.container.querySelectorAll('.txn-checkpoint')];
    const datedRow = checkpoints.find((row) => row.textContent?.includes('9f3c1ad'));
    const undatedRow = checkpoints.find((row) => row.textContent?.includes('1b2c3d4'));
    expect(datedRow).toBeDefined();
    expect(undatedRow).toBeDefined();

    // The dated commit is anchored to the first turn: its marker follows that
    // turn and precedes the next one.
    expect(follows(datedRow!, turns[0])).toBe(true);
    expect(follows(turns[1], datedRow!)).toBe(true);
    // The undated commit is not anchored to any turn: turn 0 would be the
    // adapter's fallback, so a marker after the last turn is the unattributed
    // group, and one before it would mean the bug.
    expect(follows(undatedRow!, lastTurn)).toBe(true);
  });

  it('leaves a session with no extras exactly as it was', async () => {
    stubLocalApi();

    const view = render(<TestDetail />);
    await waitFor(() => expect(view.container.querySelector('.txn-title')).toBeInTheDocument());

    expect(view.container.querySelector('.txn-checkpoint')).toBeNull();
    expect(view.container.querySelector('.txn-prs')).toBeNull();
    expect(view.container.querySelector('.txn-meta')?.textContent).not.toContain('commit');
  });

  it('keeps the transcript when both additive reads are unavailable', async () => {
    extras = {
      commits: { status: 503, body: { error: 'store not available' } },
      pullRequests: { status: 503, body: { error: 'store not available' } },
    };
    stubLocalApi();

    const view = render(<TestDetail />);
    await waitFor(() => expect(view.container.querySelector('.txn-title')).toBeInTheDocument());

    expect(view.container.querySelector('.txn-stream')).toBeInTheDocument();
    expect(view.container.querySelector('.txn-checkpoint')).toBeNull();
    expect(view.container.querySelector('.txn-prs')).toBeNull();
    expect(screen.queryByText(/could not load/i)).toBeNull();
  });
});
