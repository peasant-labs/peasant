import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { parseSessionsRoute, SessionsRouter } from './SessionsRouter';
import { ALPHA_HASH, BETA_HASH, makeSession } from '@/app/review/[[...segments]]/test-fixtures';
import type { SessionsPayload } from '@/types/messages';

const PROJECT_HASH = ALPHA_HASH;

const replace = vi.fn();
let pathname = `/sessions/${PROJECT_HASH}`;
vi.mock('next/navigation', () => ({
  usePathname: () => pathname,
  useRouter: () => ({ replace, push: vi.fn() }),
}));

let channelData: SessionsPayload | undefined = {
  sessions: [makeSession({ id: 'agent-a1' })],
};
vi.mock('@/contexts/WebSocketContext', () => ({
  useChannel: () => ({ data: channelData, connected: true, error: null, errorCode: undefined }),
}));

function okResponse(payload: unknown) {
  return Promise.resolve({ ok: true, json: async () => payload } as Response);
}

function groupedPayload(over: Record<string, unknown> = {}) {
  return {
    page: 1,
    limit: 20,
    totalItems: 1,
    ordinarySessionTotal: 1,
    helperThreadTotal: 2,
    items: [
      {
        kind: 'transcript',
        transcript: { session: makeSession({ id: 'agent-a1' }) },
        helperGroups: [
          { groupId: 'hg_p1', purpose: 'helper_review', helperThreadCount: 2, memberScope: 'scope-p1' },
        ],
      },
    ],
    ...over,
  };
}

describe('parseSessionsRoute', () => {
  it('reads the project hash from the path and refuses absent or malformed identities', () => {
    expect(parseSessionsRoute(`/sessions/${PROJECT_HASH}`)).toBe(PROJECT_HASH);
    expect(parseSessionsRoute('/sessions')).toBeNull();
    expect(parseSessionsRoute('/sessions/')).toBeNull();
    expect(parseSessionsRoute('/sessions/%')).toBeNull();
    expect(parseSessionsRoute('/map/thing')).toBeNull();
  });
});

describe('SessionsRouter', () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    replace.mockReset();
    pathname = `/sessions/${PROJECT_HASH}`;
    channelData = { sessions: [makeSession({ id: 'agent-a1' })] };
    fetchMock = vi.fn((input: RequestInfo | URL) => {
      const url = new URL(String(input));
      if (url.pathname === '/api/v1/sessions') return okResponse(groupedPayload());
      if (url.pathname === '/api/v1/session-groups/hg_p1/members') {
        return okResponse({
          members: [
            { kind: 'transcript', transcript: { session: makeSession({ id: 'helper01' }) } },
          ],
          page: 1,
          limit: 20,
          total: 2,
        });
      }
      return Promise.resolve({ ok: false, status: 404, text: async () => 'not found' } as Response);
    });
    vi.stubGlobal('fetch', fetchMock);
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it('mounts the project-scoped grouped list from the grouped REST route', async () => {
    render(<SessionsRouter />);

    expect(await screen.findByRole('heading', { name: 'alpha-project' })).toBeInTheDocument();
    expect(await screen.findByText('agent-a1')).toBeInTheDocument();
    expect(await screen.findByText('2 helper threads')).toBeInTheDocument();

    const listCalls = fetchMock.mock.calls.filter(
      (call) => new URL(String(call[0])).pathname === '/api/v1/sessions',
    );
    expect(listCalls).toHaveLength(1);
    const url = new URL(String(listCalls[0][0]));
    expect(url.searchParams.get('view')).toBe('grouped');
    expect(url.searchParams.get('project')).toBe(PROJECT_HASH);
    // The flat omitted-view route is untouched and never requested here.
    expect(url.searchParams.get('view')).not.toBeNull();
  });

  it('expands a saved helper from the project-scoped member route', async () => {
    render(<SessionsRouter />);
    await screen.findByText('agent-a1');

    await userEvent.click(screen.getByText('2 helper threads'));
    expect(await screen.findByText('helper01')).toBeInTheDocument();
    const memberCall = fetchMock.mock.calls.find((call) => String(call[0]).includes('/members'));
    expect(memberCall).toBeDefined();
    const memberUrl = new URL(String(memberCall?.[0]));
    expect(memberUrl.pathname).toBe('/api/v1/session-groups/hg_p1/members');
    expect(memberUrl.searchParams.get('scope')).toBe('scope-p1');
  });

  it('shows the ingest teaching state when the project has no grouped items', async () => {
    fetchMock.mockImplementation((input: RequestInfo | URL) => {
      const url = new URL(String(input));
      if (url.pathname === '/api/v1/sessions') {
        return okResponse(
          groupedPayload({ items: [], totalItems: 0, ordinarySessionTotal: 0, helperThreadTotal: 0 }),
        );
      }
      return Promise.resolve({ ok: false, status: 404, text: async () => 'not found' } as Response);
    });
    render(<SessionsRouter />);

    expect(await screen.findByText('no sessions recorded for this project yet')).toBeInTheDocument();
    expect(screen.getByText('peasant ingest')).toBeInTheDocument();
  });

  it('refuses a cross-project grouped response rather than misstating the project', async () => {
    fetchMock.mockImplementation((input: RequestInfo | URL) => {
      const url = new URL(String(input));
      if (url.pathname === '/api/v1/sessions') {
        return okResponse(
          groupedPayload({
            items: [
              {
                kind: 'transcript',
                transcript: { session: makeSession({ id: 'agent-other', projectHash: BETA_HASH }) },
              },
            ],
          }),
        );
      }
      return Promise.resolve({ ok: false, status: 404, text: async () => 'not found' } as Response);
    });
    render(<SessionsRouter />);

    expect(await screen.findByText(/carries other projects' sessions/i)).toBeInTheDocument();
    expect(screen.queryByText('agent-other')).not.toBeInTheDocument();
  });

  it('redirects Home for a malformed project identity without a sessions request', async () => {
    pathname = '/sessions/%';
    render(<SessionsRouter />);

    await waitFor(() => expect(replace).toHaveBeenCalledWith('/'));
    expect(fetchMock).not.toHaveBeenCalled();
  });
});
