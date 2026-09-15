import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { LocalSessionListPayload } from '@/lib/api/grouped';
import { GroupedLocalSessions } from './GroupedLocalSessions';

const PROJECT_HASH = 'a'.repeat(64);

function session(id: string, overrides: Record<string, unknown> = {}) {
  return {
    id,
    harness: 'codex',
    startTime: '2026-01-01T00:00:00Z',
    durationMins: 1,
    turnCount: 2,
    totalTokens: 10,
    toolCallCount: 0,
    projectHash: PROJECT_HASH,
    ...overrides,
  };
}

function group(groupId: string, helperThreadCount: number, memberScope: string) {
  return { groupId, purpose: 'helper_review', helperThreadCount, memberScope };
}

/** P1 with two saved helpers, P2 with one, plus an ordinary session O1. */
function orderedPayload(): LocalSessionListPayload {
  return {
    page: 1,
    limit: 20,
    totalItems: 3,
    ordinarySessionTotal: 3,
    helperThreadTotal: 3,
    items: [
      { kind: 'transcript', transcript: { session: session('agent-a1') }, helperGroups: [group('hg_p1', 2, 'scope-p1')] },
      { kind: 'transcript', transcript: { session: session('agent-a2') }, helperGroups: [group('hg_p2', 1, 'scope-p2')] },
      { kind: 'transcript', transcript: { session: session('agent-a3') }, helperGroups: [] },
    ],
  } as unknown as LocalSessionListPayload;
}

/** A helper-only search result: an explicit owner context container. */
function contextPayload(): LocalSessionListPayload {
  return {
    page: 1,
    limit: 20,
    totalItems: 1,
    ordinarySessionTotal: 0,
    helperThreadTotal: 2,
    items: [
      {
        kind: 'context_container',
        context: { groupId: 'hg_p1', ownerStatus: 'known_unavailable' },
        helperGroups: [group('hg_p1', 2, 'scope-p1')],
      },
    ],
  } as unknown as LocalSessionListPayload;
}

function okResponse(payload: unknown) {
  return Promise.resolve({ ok: true, json: async () => payload } as Response);
}

function errorResponse(status: number, body: unknown) {
  return Promise.resolve({ ok: false, status, text: async () => JSON.stringify(body) } as Response);
}

function membersResponse(ids: string[], page: number, total: number, limit = 20) {
  return {
    members: ids.map((id) => ({
      kind: 'transcript',
      transcript: { session: session(id) },
    })),
    page,
    limit,
    total,
  };
}

describe('GroupedLocalSessions', () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn(() => okResponse(membersResponse([], 1, 0)));
    vi.stubGlobal('fetch', fetchMock);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it('renders ordinary rows and keeps saved helpers collapsed with their counts', () => {
    render(<GroupedLocalSessions payload={orderedPayload()} onRefreshList={() => {}} />);
    expect(screen.getByText('agent-a1')).toBeInTheDocument();
    expect(screen.getByText('agent-a2')).toBeInTheDocument();
    expect(screen.getByText('agent-a3')).toBeInTheDocument();
    expect(screen.getByText('2 helper threads')).toBeInTheDocument();
    expect(screen.getByText('1 helper thread')).toBeInTheDocument();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('renders an owner context container without a fake ordinary title', () => {
    render(<GroupedLocalSessions payload={contextPayload()} onRefreshList={() => {}} />);
    expect(screen.getByText('owner is unavailable')).toBeInTheDocument();
    expect(screen.queryByText('agent-a1')).not.toBeInTheDocument();
    expect(screen.getByText('2 helper threads')).toBeInTheDocument();
  });

  it('expands a group with exactly the issued scope and page, and links each member', async () => {
    fetchMock.mockReturnValueOnce(
      okResponse(membersResponse(['agent-b1', 'agent-b2'], 1, 2)),
    );
    render(<GroupedLocalSessions payload={orderedPayload()} onRefreshList={() => {}} />);

    await userEvent.click(screen.getByText('2 helper threads'));

    await waitFor(() => expect(screen.getByText('agent-b1')).toBeInTheDocument());
    const call = new URL(String(fetchMock.mock.calls[0][0]));
    expect(call.pathname).toBe('/api/v1/session-groups/hg_p1/members');
    expect(call.searchParams.get('scope')).toBe('scope-p1');
    expect(call.searchParams.get('page')).toBe('1');
    expect(call.searchParams.get('limit')).toBe('20');
    expect([...call.searchParams.keys()].sort()).toEqual(['limit', 'page', 'scope']);

    const memberLink = screen.getByRole('link', { name: /agent-b1/ });
    expect(memberLink).toHaveAttribute('href', `/projects/${PROJECT_HASH}/agent-b1`);
    expect(screen.getByText('agent-b2')).toBeInTheDocument();
  });

  it('keeps one independent paging state per expanded group', async () => {
    fetchMock
      .mockReturnValueOnce(okResponse(membersResponse(['agent-b1', 'agent-b2'], 1, 21)))
      .mockReturnValueOnce(okResponse(membersResponse(['agent-c1'], 1, 1)))
      .mockReturnValueOnce(okResponse(membersResponse(['agent-b3'], 2, 21)));
    render(<GroupedLocalSessions payload={orderedPayload()} onRefreshList={() => {}} />);

    await userEvent.click(screen.getByText('2 helper threads'));
    await waitFor(() => expect(screen.getByText('agent-b1')).toBeInTheDocument());
    await userEvent.click(screen.getByText('1 helper thread'));
    await waitFor(() => expect(screen.getByText('agent-c1')).toBeInTheDocument());

    const p1Footer = document.querySelector('[data-group-id="hg_p1"] [data-helper-member-paging]') as HTMLElement;
    const p2Footer = document.querySelector('[data-group-id="hg_p2"] [data-helper-member-paging]') as HTMLElement;
    expect(p1Footer.textContent).toContain('page 1 of 2');
    expect(p2Footer.textContent).toContain('page 1 of 1');
    expect(within(p2Footer).getByRole('button', { name: 'next' })).toBeDisabled();

    await userEvent.click(within(p1Footer).getByRole('button', { name: 'next' }));

    await waitFor(() => expect(screen.getByText('agent-b3')).toBeInTheDocument());
    const nextCall = new URL(String(fetchMock.mock.calls[2][0]));
    expect(nextCall.searchParams.get('scope')).toBe('scope-p1');
    expect(nextCall.searchParams.get('page')).toBe('2');
    expect(fetchMock.mock.calls.filter((call) => String(call[0]).includes('scope-p2'))).toHaveLength(1);
  });

  it('selects exactly the chosen member and never the owner or a sibling', async () => {
    const onSelect = vi.fn();
    fetchMock.mockReturnValueOnce(okResponse(membersResponse(['agent-b1', 'agent-b2'], 1, 2)));
    render(
      <GroupedLocalSessions
        payload={orderedPayload()}
        onRefreshList={() => {}}
        selection={{ selectedIds: new Set<string>(), onSelect }}
      />,
    );

    await userEvent.click(screen.getByText('2 helper threads'));
    await waitFor(() => expect(screen.getByText('agent-b1')).toBeInTheDocument());

    await userEvent.click(screen.getByLabelText('select agent-b1 (agent-b1)'));

    expect(onSelect).toHaveBeenCalledTimes(1);
    expect(onSelect).toHaveBeenCalledWith('agent-b1', true);
    expect(onSelect).not.toHaveBeenCalledWith('agent-a1', expect.anything());
    expect(onSelect).not.toHaveBeenCalledWith('agent-b2', expect.anything());
  });

  it('fails closed on an expired scope: hides members and refreshes the originating list only', async () => {
    const onRefreshList = vi.fn();
    fetchMock.mockReturnValueOnce(errorResponse(409, { error: 'expired', code: 'group_scope_expired' }));
    render(<GroupedLocalSessions payload={orderedPayload()} onRefreshList={onRefreshList} />);

    await userEvent.click(screen.getByText('2 helper threads'));

    await waitFor(() =>
      expect(screen.getByText(/the saved helper query expired/)).toBeInTheDocument(),
    );
    expect(screen.queryByText('agent-b1')).not.toBeInTheDocument();
    expect(screen.getByText('hide')).toBeInTheDocument();

    const refresh = screen.getByRole('button', { name: /refresh list/ });
    await userEvent.click(refresh);
    expect(onRefreshList).toHaveBeenCalledTimes(1);

    // The expired request was never widened: exactly one member request, for the issued scope.
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const call = new URL(String(fetchMock.mock.calls[0][0]));
    expect(call.searchParams.get('scope')).toBe('scope-p1');
  });

  it('shows the explicit empty notice when a group has no surviving members', async () => {
    fetchMock.mockReturnValueOnce(okResponse(membersResponse([], 1, 0)));
    render(<GroupedLocalSessions payload={orderedPayload()} onRefreshList={() => {}} />);

    await userEvent.click(screen.getByText('1 helper thread'));
    await waitFor(() =>
      expect(screen.getByText('no saved helpers match the current query and access.')).toBeInTheDocument(),
    );
  });
});
