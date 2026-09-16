import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { GroupedSessionsSection } from './GroupedSessionsSection';
import type { LocalSessionListPayload } from '@/lib/api/grouped';

const PROJECT_HASH = 'a'.repeat(64);

function session(id: string) {
  return {
    id,
    harness: 'codex',
    startTime: '2026-01-01T00:00:00Z',
    durationMins: 1,
    turnCount: 2,
    totalTokens: 10,
    toolCallCount: 0,
    projectHash: PROJECT_HASH,
  };
}

function listResponse(): LocalSessionListPayload {
  return {
    page: 1,
    limit: 20,
    totalItems: 1,
    ordinarySessionTotal: 1,
    helperThreadTotal: 2,
    items: [
      {
        kind: 'transcript',
        transcript: { session: session('agent-a1') },
        helperGroups: [{ groupId: 'hg_p1', purpose: 'helper_review', helperThreadCount: 2, memberScope: 'scope-p1' }],
      },
    ],
  } as unknown as LocalSessionListPayload;
}

function okResponse(payload: unknown) {
  return Promise.resolve({ ok: true, json: async () => payload } as Response);
}

describe('GroupedSessionsSection', () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn((input: RequestInfo | URL) => {
      const url = new URL(String(input));
      if (url.pathname === '/api/v1/sessions') return okResponse(listResponse());
      if (url.pathname === '/api/v1/session-groups/hg_p1/members') {
        return okResponse({ members: [{ kind: 'transcript', transcript: { session: session('agent-b1') } }], page: 1, limit: 20, total: 2 });
      }
      return Promise.resolve({ ok: false, status: 404, text: async () => 'not found' } as Response);
    });
    vi.stubGlobal('fetch', fetchMock);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it('fetches the grouped route and renders its items and counts', async () => {
    render(<GroupedSessionsSection variant="sessions" heading="all sessions" />);
    expect(await screen.findByText('agent-a1')).toBeInTheDocument();
    expect(screen.getByText('1 session · 2 helper threads')).toBeInTheDocument();
    const listCalls = fetchMock.mock.calls.filter((call) => String(call[0]).includes('view=grouped'));
    expect(listCalls).toHaveLength(1);
  });

  it('refetches the grouped read when the live session channel invalidates it', async () => {
    const view = render(<GroupedSessionsSection variant="sessions" invalidationKey="a" />);
    await screen.findByText('agent-a1');
    view.rerender(<GroupedSessionsSection variant="sessions" invalidationKey="b" />);
    await waitFor(() =>
      expect(fetchMock.mock.calls.filter((call) => String(call[0]).includes('view=grouped'))).toHaveLength(2),
    );
  });

  it('scopes the grouped read to one project through the route filter', async () => {
    render(<GroupedSessionsSection variant="sessions" projectHash={PROJECT_HASH} heading="sessions" />);
    await screen.findByText('agent-a1');

    const listCalls = fetchMock.mock.calls.filter(
      (call) => new URL(String(call[0])).pathname === '/api/v1/sessions',
    );
    expect(listCalls).toHaveLength(1);
    const url = new URL(String(listCalls[0][0]));
    expect(url.searchParams.get('view')).toBe('grouped');
    expect(url.searchParams.get('project')).toBe(PROJECT_HASH);
  });

  it('refuses a cross-project grouped response instead of rendering it under the project heading', async () => {
    fetchMock.mockImplementation((input: RequestInfo | URL) => {
      const url = new URL(String(input));
      if (url.pathname === '/api/v1/sessions') {
        return okResponse({
          ...listResponse(),
          items: [
            {
              kind: 'transcript',
              transcript: { session: { ...session('other'), projectHash: 'b'.repeat(64) } },
            },
          ],
        });
      }
      return Promise.resolve({ ok: false, status: 404, text: async () => 'not found' } as Response);
    });
    render(<GroupedSessionsSection variant="sessions" projectHash={PROJECT_HASH} heading="sessions" />);

    expect(await screen.findByText(/carries other projects' sessions/i)).toBeInTheDocument();
    expect(screen.queryByText('other')).not.toBeInTheDocument();
  });

  it('reports a server that did not apply the project filter so the host can fall back', async () => {
    const onScopeUnavailable = vi.fn();
    fetchMock.mockImplementation((input: RequestInfo | URL) => {
      const url = new URL(String(input));
      if (url.pathname === '/api/v1/sessions') {
        return okResponse({
          ...listResponse(),
          items: [
            {
              kind: 'transcript',
              transcript: { session: { ...session('other'), projectHash: 'b'.repeat(64) } },
            },
          ],
        });
      }
      return Promise.resolve({ ok: false, status: 404, text: async () => 'not found' } as Response);
    });
    render(
      <GroupedSessionsSection
        variant="sessions"
        projectHash={PROJECT_HASH}
        heading="sessions"
        onScopeUnavailable={onScopeUnavailable}
      />,
    );

    await waitFor(() => expect(onScopeUnavailable).toHaveBeenCalledTimes(1));
    expect(screen.queryByText('other')).not.toBeInTheDocument();
    expect(screen.queryByText(/carries other projects' sessions/i)).not.toBeInTheDocument();
    expect(screen.queryByText('sessions')).not.toBeInTheDocument();
  });

  it('refreshes the originating list when an expanded group scope expires, never a broader set', async () => {
    fetchMock.mockImplementation((input: RequestInfo | URL) => {
      const url = new URL(String(input));
      if (url.pathname === '/api/v1/sessions') return okResponse(listResponse());
      if (url.pathname === '/api/v1/session-groups/hg_p1/members') {
        return Promise.resolve({ ok: false, status: 409, text: async () => JSON.stringify({ error: 'expired', code: 'group_scope_expired' }) } as Response);
      }
      return Promise.resolve({ ok: false, status: 404, text: async () => 'not found' } as Response);
    });
    render(<GroupedSessionsSection variant="sessions" />);
    await screen.findByText('agent-a1');

    await userEvent.click(screen.getByText('2 helper threads'));
    await waitFor(() => expect(screen.getByText(/the saved helper query expired/)).toBeInTheDocument());

    await userEvent.click(screen.getByRole('button', { name: /refresh list/ }));
    await waitFor(() =>
      expect(fetchMock.mock.calls.filter((call) => String(call[0]).includes('view=grouped'))).toHaveLength(2),
    );
    const memberCalls = fetchMock.mock.calls.filter((call) => String(call[0]).includes('/members'));
    expect(memberCalls).toHaveLength(1);
    expect(String(memberCalls[0][0])).not.toContain('all');
  });
});
