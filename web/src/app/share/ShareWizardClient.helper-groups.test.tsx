import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import fixtureSource from './testdata/mounted-share-helper-groups.yaml?raw';
import {
  buildGroupedMembersResponse,
  buildGroupedSyncResponse,
  type GroupedSyncSessionSpec,
} from './testdata/grouped-sync';
import {
  HELPER_GROUP_REQUIRED_ROLES,
  loadHelperGroupFixture,
  type FixtureMember,
} from './testdata/helper-groups-fixture';
import { ShareWizardClient } from './ShareWizardClient';
import * as useMockConfig from '@/hooks/useMockConfig';

const fixture = loadHelperGroupFixture(fixtureSource, HELPER_GROUP_REQUIRED_ROLES);

// Mockable search params: the deep-link arm sets ?sessionId=, every other arm
// leaves them empty. All arms mount the same production surface.
let currentSearchParams = new URLSearchParams();
vi.mock('@/hooks/useMockConfig');
vi.mock('next/navigation', () => ({ useSearchParams: () => currentSearchParams }));

const response = (body: unknown, status = 200) => ({ ok: status < 400, status, json: async () => body, text: async () => '' });

function ownerSpecs(owners = fixture.owners): GroupedSyncSessionSpec[] {
  return owners.map((owner) => ({
    id: owner.id,
    harness: 'codex',
    startTime: owner.startTime,
    durationMins: 1,
    totalTokens: 100,
    turnCount: 1,
    project: owner.project,
    projectHash: owner.projectHash,
    preview: owner.preview,
    syncStatus: owner.syncStatus,
    helperGroups: owner.groups,
  }));
}

function contextSpecs() {
  return fixture.contexts.map((context) => ({
    groupId: context.groupId,
    ownerStatus: context.ownerStatus,
    helperGroups: context.helperGroups,
  }));
}

function memberSpecsFor(groupId: string): GroupedSyncSessionSpec[] {
  const rows: FixtureMember[] = fixture.contexts.some((context) => context.groupId === groupId)
    ? fixture.contextMembers
    : fixture.members.filter((member) => member.groupId === groupId);
  return rows.map((member) => ({
    id: member.id,
    harness: 'codex',
    startTime: '2026-08-12T07:00:00Z',
    durationMins: 1,
    totalTokens: 10,
    turnCount: member.turnCount,
    toolCallCount: 0,
    project: 'alpha',
    projectHash: 'hash-alpha',
    preview: member.preview,
    syncStatus: member.syncStatus,
    helperGroups: member.helperGroups,
  }));
}

function discoveryItems() {
  const ids = [
    ...fixture.owners.map((owner) => owner.id),
    ...fixture.members.map((member) => member.id),
    ...fixture.contextMembers.map((member) => member.id),
  ];
  return ids.map((sessionId) => ({ sessionId, locationLabel: 'same label', repositoryLocationId: 'location-a', branch: 'main', selectionStatus: 'selected' }));
}

interface FetchOptions {
  memberStatus?: number;
  /** Refuse only this group's scope (409 group_scope_expired). */
  expireGroup?: (groupId: string) => boolean;
  onSyncList?: () => void;
  /** Replace the grouped list payload (e.g. a changed list revision). */
  syncList?: () => unknown;
}

function installFetch(options: FetchOptions = {}) {
  const fetchMock = vi.fn(async (input: RequestInfo | URL, _init?: RequestInit) => {
    const requested = new URL(String(input), 'http://mounted.test');
    switch (requested.pathname) {
      case '/api/v1/sync/sessions':
        options.onSyncList?.();
        return response(options.syncList ? options.syncList() : buildGroupedSyncResponse(ownerSpecs(), contextSpecs()));
      case '/api/v1/web/discovery':
        return response({ items: discoveryItems() });
      case '/api/v1/annotations':
        return response({ annotations: [] });
      case '/api/v1/sync/redactions':
        return response({ categories: [] });
      case '/api/v1/sync/push':
        return response({ new: 1, updated: 0, skipped: 0, errors: 0, sessions: [] });
      default:
        if (requested.pathname.startsWith('/api/v1/session-groups/')) {
          const groupId = decodeURIComponent(requested.pathname.split('/')[4] ?? '');
          if (options.expireGroup?.(groupId) || (options.memberStatus && options.memberStatus >= 400)) {
            return response({ code: 'group_scope_expired', error: 'scope expired' }, options.memberStatus ?? 409);
          }
          const members = memberSpecsFor(groupId);
          const pageSize = fixture.paging[groupId] ?? (members.length || 1);
          const page = Number(requested.searchParams.get('page') ?? '1');
          const start = (page - 1) * pageSize;
          return response(buildGroupedMembersResponse(members.slice(start, start + pageSize), {
            page,
            limit: pageSize,
            total: members.length,
          }));
        }
        throw new Error(`unexpected helper-group mounted fetch: ${String(input)}`);
    }
  });
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

/** The rendered member row's checkbox, keyed by its stable transcript id. */
function memberCheckbox(id: string): HTMLInputElement {
  const row = document.querySelector(`[data-thread-id="${id}"]`);
  if (!row) throw new Error(`helper member row ${id} is not rendered`);
  const box = row.querySelector('input[type="checkbox"]');
  if (!box) throw new Error(`helper member row ${id} has no checkbox`);
  return box as HTMLInputElement;
}

function groupRoot(groupId: string): HTMLElement {
  const group = document.querySelector(`[data-group-id="${groupId}"]`);
  if (!group) throw new Error(`helper group ${groupId} is not rendered`);
  return group as HTMLElement;
}

function sessionCheckbox(id: string): HTMLInputElement {
  const box = screen.getByRole('checkbox', { name: `select session ${id}` });
  return box as HTMLInputElement;
}

async function goToSubmit(user: ReturnType<typeof userEvent.setup>) {
  const footer = document.querySelector('.swz-foot') as HTMLElement;
  await user.click(within(footer).getByRole('button', { name: 'Continue' }));
  await user.click(await within(footer).findByRole('button', { name: 'Skip' }));
  const continueRedaction = await waitFor(() => {
    const action = within(footer).getByRole('button', { name: 'Continue' });
    expect(action).toBeEnabled();
    return action;
  });
  await user.click(continueRedaction);
  await user.click(await screen.findByRole('button', { name: 'Submit' }));
}

async function expectPushedIds(fetchMock: ReturnType<typeof installFetch>, expected: string[]) {
  await waitFor(() => {
    const call = fetchMock.mock.calls.find(([input]) => String(input).includes('/api/v1/sync/push'));
    expect(call, 'the submit step must push the selection').toBeTruthy();
    const body = JSON.parse(String((call?.[1] as RequestInit | undefined)?.body)) as { sessionIds: string[] };
    expect([...body.sessionIds].sort()).toEqual([...expected].sort());
  });
}

describe('mounted Share helper-group chooser', () => {
  beforeEach(() => {
    currentSearchParams = new URLSearchParams();
    vi.mocked(useMockConfig.useMockConfig).mockReturnValue({ config: { enabled: false, web: [], tui: [] }, loading: false, error: null, refetch: vi.fn() });
  });
  afterEach(() => { vi.unstubAllGlobals(); vi.clearAllMocks(); });

  it('selects exactly one eligible helper member and pushes only its transcript id', async () => {
    const fetchMock = installFetch();
    const user = userEvent.setup();
    render(<ShareWizardClient />);

    // The owner row anchors a collapsed group whose count is the saved-thread
    // count, never a message or review total.
    const ownerRegion = await screen.findByRole('region', { name: 'project alpha' });
    expect(within(ownerRegion).getByRole('checkbox', { name: `select session ${fixture.owners[0].id}` })).toBeInTheDocument();
    await user.click(await screen.findByRole('button', { name: /3 helper threads/ }));

    const expectedMembersURL = `/api/v1/session-groups/hg_owner1/members?scope=scope-owner1`;
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith(expect.stringContaining(expectedMembersURL)));

    await waitFor(() => expect(document.querySelector('[data-thread-id="eligible-member"]')).not.toBeNull());
    // The ineligible member is rendered but disabled: held is not contributable.
    expect(memberCheckbox('ineligible-member')).toBeDisabled();
    expect(memberCheckbox('other-member')).not.toBeChecked();
    await user.click(memberCheckbox('eligible-member'));

    const tally = document.querySelector('.gms-tally') as HTMLElement;
    expect(tally.textContent).toContain('1 selected');

    await goToSubmit(user);
    await expectPushedIds(fetchMock, ['eligible-member']);
  });

  it('keeps an earlier helper pick when a second helper group is expanded', async () => {
    const fetchMock = installFetch();
    const user = userEvent.setup();
    render(<ShareWizardClient />);

    await user.click(await screen.findByRole('button', { name: /3 helper threads/ }));
    await waitFor(() => expect(memberCheckbox('eligible-member')).toBeVisible());
    await user.click(memberCheckbox('eligible-member'));
    expect(memberCheckbox('eligible-member')).toBeChecked();

    // Expanding a second, independently scoped group must not clear the first
    // group's explicit pick.
    await user.click(screen.getByRole('button', { name: /2 helper threads/ }));
    await waitFor(() => expect(document.querySelector('[data-thread-id="second-group-member"]')).not.toBeNull());
    expect(memberCheckbox('eligible-member')).toBeChecked();
    await user.click(memberCheckbox('second-group-member'));

    const tally = document.querySelector('.gms-tally') as HTMLElement;
    expect(tally.textContent).toContain('2 selected');

    await goToSubmit(user);
    await expectPushedIds(fetchMock, ['eligible-member', 'second-group-member']);
  });

  it('keeps a pick on one page of a group while an independent group keeps its own paging state', async () => {
    const fetchMock = installFetch();
    const user = userEvent.setup();
    render(<ShareWizardClient />);

    // The first group's three members all sit on one page.
    await user.click(await screen.findByRole('button', { name: /3 helper threads/ }));
    await waitFor(() => expect(document.querySelector('[data-thread-id="eligible-member"]')).not.toBeNull());

    // The second group is paged one member at a time. Select on page one.
    await user.click(screen.getByRole('button', { name: /2 helper threads/ }));
    await waitFor(() => expect(document.querySelector('[data-thread-id="second-group-member"]')).not.toBeNull());
    await user.click(memberCheckbox('second-group-member'));

    const second = groupRoot('hg_owner2');
    await user.click(within(second).getByRole('button', { name: 'next' }));
    await waitFor(() => expect(document.querySelector('[data-thread-id="page-two-member"]')).not.toBeNull());
    // Page two replaced page one's rows, but not the selection made on page one.
    expect(document.querySelector('[data-thread-id="second-group-member"]')).toBeNull();
    await user.click(memberCheckbox('page-two-member'));

    // The first group never paged: its rows and its own selection are untouched.
    expect(groupRoot('hg_owner1')).toBeInTheDocument();
    expect(memberCheckbox('eligible-member')).not.toBeChecked();
    expect(document.querySelector('[data-thread-id="other-member"]')).not.toBeNull();

    const tally = document.querySelector('.gms-tally') as HTMLElement;
    expect(tally.textContent).toContain('2 selected');

    await goToSubmit(user);
    await expectPushedIds(fetchMock, ['second-group-member', 'page-two-member']);
  });

  it('selects a nested helper member without widening to its owner or the parent group', async () => {
    const fetchMock = installFetch();
    const user = userEvent.setup();
    render(<ShareWizardClient />);

    await screen.findByRole('checkbox', { name: 'select session owner-nested' });
    await user.click(within(groupRoot('hg_nested_owner')).getByRole('button', { name: /1 helper thread/ }));

    // The nested owner's own member anchors a further group; open it from inside
    // that member's row, not from the top-level group list.
    await waitFor(() => expect(document.querySelector('[data-thread-id="nested-owner-member"]')).not.toBeNull());
    const nestedOwnerRow = document.querySelector('[data-thread-id="nested-owner-member"]') as HTMLElement;
    await user.click(within(nestedOwnerRow).getByRole('button', { name: /1 helper thread/ }));

    await waitFor(() => expect(document.querySelector('[data-thread-id="nested-member"]')).not.toBeNull());
    await user.click(memberCheckbox('nested-member'));

    // No parent/owner widening: only the nested member is checked.
    expect(memberCheckbox('nested-owner-member')).not.toBeChecked();
    expect(sessionCheckbox('owner-nested')).not.toBeChecked();

    const tally = document.querySelector('.gms-tally') as HTMLElement;
    expect(tally.textContent).toContain('1 selected');

    await goToSubmit(user);
    await expectPushedIds(fetchMock, ['nested-member']);
  });

  it('preselects a deep-linked session once and keeps it while helper pages load', async () => {
    currentSearchParams = new URLSearchParams({ sessionId: 'owner-1' });
    const fetchMock = installFetch();
    const user = userEvent.setup();
    render(<ShareWizardClient />);

    await waitFor(() => expect(sessionCheckbox('owner-1')).toBeChecked());

    await user.click(screen.getByRole('button', { name: /3 helper threads/ }));
    await waitFor(() => expect(document.querySelector('[data-thread-id="eligible-member"]')).not.toBeNull());
    await user.click(memberCheckbox('eligible-member'));

    // Loading a second group's page must not reset the chooser back to the
    // deep-linked session, and must not drop the helper pick.
    await user.click(screen.getByRole('button', { name: /2 helper threads/ }));
    await waitFor(() => expect(document.querySelector('[data-thread-id="second-group-member"]')).not.toBeNull());
    expect(sessionCheckbox('owner-1')).toBeChecked();
    expect(memberCheckbox('eligible-member')).toBeChecked();

    await goToSubmit(user);
    await expectPushedIds(fetchMock, ['owner-1', 'eligible-member']);
  });

  it('fails closed on an expired scope, offers only a list refresh, and refreshes the originating list', async () => {
    let syncListCalls = 0;
    installFetch({ memberStatus: 409, onSyncList: () => { syncListCalls += 1; } });
    const user = userEvent.setup();
    render(<ShareWizardClient />);

    await user.click(await screen.findByRole('button', { name: /3 helper threads/ }));

    // The refused scope hides members and offers ONLY a refresh of the
    // originating list; it never loads a broader set.
    const refresh = await screen.findByRole('button', { name: /refresh list/i });
    await waitFor(() => expect(document.querySelector('[data-thread-id="eligible-member"]')).toBeNull());
    await user.click(refresh);
    await waitFor(() => expect(syncListCalls).toBeGreaterThan(1));
  });

  it('refreshes the changed originating list on scope expiry and prunes the stale member pick', async () => {
    let syncListCalls = 0;
    const fetchMock = installFetch({
      expireGroup: (groupId) => groupId === 'hg_owner2',
      onSyncList: () => { syncListCalls += 1; },
      syncList: () => {
        // The refreshed revision no longer offers the second owner, its helpers,
        // or the first owner's helper group; the still-confirmed first owner
        // remains in the list.
        const owners = syncListCalls > 1
          ? fixture.owners
            .filter((owner) => owner.id !== 'owner-2')
            .map((owner) => (owner.id === 'owner-1' ? { ...owner, groups: [] } : owner))
          : fixture.owners;
        const contexts = syncListCalls > 1 ? [] : contextSpecs();
        return buildGroupedSyncResponse(ownerSpecs(owners), contexts);
      },
    });
    const user = userEvent.setup();
    render(<ShareWizardClient />);

    // A confirmed ordinary session plus an explicit helper member.
    await waitFor(() => expect(sessionCheckbox('owner-1')).toBeVisible());
    await user.click(sessionCheckbox('owner-1'));
    await user.click(screen.getByRole('button', { name: /3 helper threads/ }));
    await waitFor(() => expect(document.querySelector('[data-thread-id="eligible-member"]')).not.toBeNull());
    await user.click(memberCheckbox('eligible-member'));
    expect((document.querySelector('.gms-tally') as HTMLElement).textContent).toContain('2 selected');

    // Expanding the second group refuses its scope and refreshes the list.
    await user.click(screen.getByRole('button', { name: /2 helper threads/ }));
    await waitFor(() => expect(syncListCalls).toBeGreaterThan(1));

    // The refreshed list confirms owner-1 but cannot confirm the helper member,
    // so only the confirmed session survives to the push.
    await waitFor(() => expect(sessionCheckbox('owner-1')).toBeChecked());
    await waitFor(() => expect(document.querySelector('[data-thread-id="eligible-member"]')).toBeNull());
    await waitFor(() => {
      expect((document.querySelector('.gms-tally') as HTMLElement).textContent).toContain('1 selected');
    });

    await goToSubmit(user);
    await expectPushedIds(fetchMock, ['owner-1']);
  });
});
