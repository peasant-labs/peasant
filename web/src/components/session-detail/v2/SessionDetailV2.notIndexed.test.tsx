import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { parseTranscriptRouteQuery, type ProjectHash } from '@/lib/navigation/projectRoutes';
import { PublishProvider } from '@/contexts/PublishContext';
import { SessionDetailV2 } from './SessionDetailV2';

// A session this computer recorded but has not indexed: the detail carries the
// recording's turn count and no turns. The page must not show that count over
// an empty transcript; it says what is missing, offers the re-index, and reads
// the session again when the re-index lands.

const PROJECT_HASH = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' as ProjectHash;
const SESSION_ID = 'sess-not-indexed';

let sessionDetailData: unknown;
const refreshDetail = vi.fn();
const reindexSession = vi.fn();

vi.mock('@/lib/share/publishing', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/lib/share/publishing')>()),
  ...(await import('@/test/fixtures/publishingIdle')).PUBLISHING_IDLE,
}));
vi.mock('@/lib/api/ingest', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/lib/api/ingest')>()),
  reindexSession: (...args: unknown[]) => reindexSession(...args),
}));
vi.mock('next/navigation', () => ({
  useSearchParams: () => new URLSearchParams(),
  usePathname: () => `/projects/${PROJECT_HASH}/${SESSION_ID}`,
  useRouter: () => ({ replace: vi.fn() }),
}));
vi.mock('@/contexts/WebSocketContext', () => ({
  useChannel: (subscription: { topic: string }) => ({
    data: subscription.topic === 'session_detail' ? sessionDetailData : { sessions: [] },
    connected: true,
    error: null,
    refresh: subscription.topic === 'session_detail' ? refreshDetail : vi.fn(),
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

function detail(turnCount: number) {
  return {
    id: SESSION_ID,
    project: 'alpha-project',
    harness: 'claude-code',
    startTime: '2026-10-22T09:00:00Z',
    endTime: '2026-10-22T14:00:00Z',
    durationMins: 300,
    totalTokens: 0,
    tokensIn: 0,
    tokensOut: 0,
    turnCount,
    toolCallCount: 0,
    turns: null,
  };
}

function TestDetail() {
  const routeQuery = parseTranscriptRouteQuery(new URLSearchParams());
  if (!routeQuery) throw new Error('route query must be valid');
  return (
    <PublishProvider>
      <SessionDetailV2 sessionId={SESSION_ID} projectHash={PROJECT_HASH} projectName="alpha-project" routeQuery={routeQuery} />
    </PublishProvider>
  );
}

beforeEach(() => {
  refreshDetail.mockReset();
  reindexSession.mockReset();
});

afterEach(() => {
  cleanup();
  sessionDetailData = undefined;
});

describe('SessionDetailV2 — a recorded session with nothing indexed', () => {
  it('says the transcript is not indexed instead of a turn count over an empty viewer', () => {
    sessionDetailData = detail(940);
    render(<TestDetail />);
    const state = screen.getByTestId('session-not-indexed');
    expect(screen.getByRole('heading', { level: 1, name: 'transcript not indexed yet' })).toBeInTheDocument();
    expect(state).toHaveTextContent('940 recorded turns');
    expect(state).not.toHaveTextContent('no turns to display');
    expect(document.querySelector('.txn-meta')).toBeNull();
  });

  it('re-indexes the session here and reads it again when the re-index lands', async () => {
    sessionDetailData = detail(940);
    reindexSession.mockResolvedValue({ notes: [] });
    render(<TestDetail />);
    fireEvent.click(screen.getByRole('button', { name: 're-index this session' }));
    expect(reindexSession).toHaveBeenCalledWith(SESSION_ID, expect.anything());
    await waitFor(() => expect(refreshDetail).toHaveBeenCalledTimes(1));
  });

  it('keeps the viewer for a session that recorded no turns at all', () => {
    sessionDetailData = detail(0);
    render(<TestDetail />);
    expect(screen.queryByTestId('session-not-indexed')).toBeNull();
  });
});
