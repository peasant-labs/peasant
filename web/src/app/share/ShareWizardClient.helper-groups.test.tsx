import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { parse } from 'yaml';
import { buildGroupedSyncResponse, type GroupedSyncContextSpec, type GroupedSyncSessionSpec } from './testdata/grouped-sync';
import { ShareWizardClient } from './ShareWizardClient';
import * as useMockConfig from '@/hooks/useMockConfig';

vi.mock('@/hooks/useMockConfig');
vi.mock('next/navigation', () => ({ useSearchParams: () => new URLSearchParams() }));

interface HelperGroupSpec { groupId: string; helperThreadCount: number; memberScope: string }
interface OwnerSpec { id: string; project: string; projectHash: string; startTime: string; preview: string; syncStatus: 'new' | 'updated' | 'synced' | 'held'; groups: HelperGroupSpec[] }
interface MemberSpec { groupId: string; id: string; preview: string; turnCount: number; syncStatus: 'new' | 'updated' | 'synced' | 'held' }
interface Fixture {
  requiredNames: string[];
  owners: OwnerSpec[];
  members: MemberSpec[];
  contexts: Array<{ groupId: string; ownerStatus: string; helperGroups: HelperGroupSpec[] }>;
  contextMembers: MemberSpec[];
}

// Required-NAME manifest: every declared role must be present, so a resolver
// that drops a role fails loudly instead of silently shrinking the corpus.
function loadFixture(): Fixture {
  const fixture = parse(readFileSync(resolve(process.cwd(), 'src/app/share/testdata/mounted-share-helper-groups.yaml'), 'utf8')) as Fixture;
  const names = new Set<string>();
  if (fixture.owners.some((owner) => owner.groups.length > 0)) names.add('owner-with-helpers');
  if (fixture.members.some((member) => member.id === 'eligible-member')) names.add('eligible-member');
  if (fixture.members.some((member) => member.id === 'ineligible-member')) names.add('ineligible-member');
  if (fixture.contexts.length > 0) names.add('helper-only-context');
  for (const required of fixture.requiredNames) {
    if (!names.has(required)) {
      throw new Error(`mounted helper-group fixture is missing required role ${required}; restore the role or remove it from the manifest`);
    }
  }
  return fixture;
}

const fixture = loadFixture();
const response = (body: unknown, status = 200) => ({ ok: status < 400, status, json: async () => body, text: async () => '' });

/** The rendered member row's checkbox, keyed by its stable transcript id. */
function memberCheckbox(id: string): HTMLInputElement {
  const row = document.querySelector(`[data-thread-id="${id}"]`);
  if (!row) throw new Error(`helper member row ${id} is not rendered`);
  const box = row.querySelector('input[type="checkbox"]');
  if (!box) throw new Error(`helper member row ${id} has no checkbox`);
  return box as HTMLInputElement;
}

function ownerSessions(): GroupedSyncSessionSpec[] {
  return fixture.owners.map((owner) => ({
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

function contextSpecs(): GroupedSyncContextSpec[] {
  return fixture.contexts.map((context) => ({
    groupId: context.groupId,
    ownerStatus: context.ownerStatus,
    helperGroups: context.helperGroups,
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

function memberItems(groupId: string) {
  const specs = groupId === fixture.contexts[0]?.groupId ? fixture.contextMembers : fixture.members.filter((member) => member.groupId === groupId);
  return specs.map((member) => ({
    kind: 'transcript' as const,
    transcript: {
      session: { id: member.id, harness: 'codex', startTime: '2026-08-12T07:00:00Z', durationMins: 1, totalTokens: 10, turnCount: member.turnCount, toolCallCount: 0, project: 'alpha', projectHash: 'hash-alpha', preview: member.preview },
      sync: { id: member.id, harness: 'codex', projectName: 'alpha', projectHash: 'hash-alpha', hostSlug: 'host', startTime: '2026-08-12T07:00:00Z', durationMs: 60000, totalTokens: 10, turnCount: member.turnCount, model: 'fixture-model', syncStatus: member.syncStatus },
    },
    helperGroups: [],
  }));
}

function installFetch(options: { memberStatus?: number; onSyncList?: () => void } = {}) {
  const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
    const requested = new URL(String(input), 'http://mounted.test');
    switch (requested.pathname) {
      case '/api/v1/sync/sessions':
        options.onSyncList?.();
        return response(buildGroupedSyncResponse(ownerSessions(), contextSpecs()));
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
          if (options.memberStatus && options.memberStatus >= 400) {
            return response({ code: 'group_scope_expired', error: 'scope expired' }, options.memberStatus);
          }
          const groupId = decodeURIComponent(requested.pathname.split('/')[4] ?? '');
          const members = memberItems(groupId);
          return response({ members, page: 1, limit: 20, total: members.length });
        }
        throw new Error(`unexpected helper-group mounted fetch: ${String(input)}`);
    }
  });
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

describe('mounted Share helper-group chooser', () => {
  beforeEach(() => {
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
    const groupTrigger = await screen.findByRole('button', { name: /3 helper threads/ });
    await user.click(groupTrigger);

    const expectedMembersURL = `/api/v1/session-groups/${fixture.owners[0].groups[0].groupId}/members?scope=${fixture.owners[0].groups[0].memberScope}`;
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith(expect.stringContaining(expectedMembersURL)));

    await waitFor(() => expect(document.querySelector('[data-thread-id="eligible-member"]')).not.toBeNull());
    const eligible = memberCheckbox('eligible-member');
    // The ineligible member is rendered but disabled: held is not contributable.
    expect(memberCheckbox('ineligible-member')).toBeDisabled();
    expect(memberCheckbox('other-member')).not.toBeChecked();
    await user.click(eligible);

    const tally = document.querySelector('.gms-tally') as HTMLElement;
    expect(tally.textContent).toContain('1 selected');

    // The push carries exactly the member id: not its owner, its sibling, or
    // another owner's helper.
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
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith(expect.stringContaining('/api/v1/sync/push'), expect.objectContaining({
      body: JSON.stringify({ sessionIds: ['eligible-member'], redactionLevel: 'standard', visibility: 'public' }),
    })));
  });

  it('fails closed on an expired scope, offers only a list refresh, and refreshes the originating list', async () => {
    let syncListCalls = 0;
    installFetch({ memberStatus: 409, onSyncList: () => { syncListCalls += 1; } });
    const user = userEvent.setup();
    render(<ShareWizardClient />);

    const groupTrigger = await screen.findByRole('button', { name: /3 helper threads/ });
    await user.click(groupTrigger);

    // The refused scope hides members and offers ONLY a refresh of the
    // originating list; it never loads a broader set.
    const refresh = await screen.findByRole('button', { name: /refresh list/i });
    await waitFor(() => expect(document.querySelector('[data-thread-id="eligible-member"]')).toBeNull());
    await user.click(refresh);
    await waitFor(() => expect(syncListCalls).toBeGreaterThan(1));
  });
});
