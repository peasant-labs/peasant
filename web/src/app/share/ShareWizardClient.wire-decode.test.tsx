import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import helperFixtureSource from './testdata/mounted-share-helper-groups.yaml?raw';
import {
  buildGroupedMembersResponse,
  buildGroupedSyncResponse,
  type GroupedSyncSessionSpec,
} from './testdata/grouped-sync';
import {
  HELPER_GROUP_REQUIRED_ROLES,
  loadHelperGroupFixture,
  type FixtureMember,
  type FixtureOwner,
} from './testdata/helper-groups-fixture';
import { readWireDecodeFixture, type WireDecodeCase } from './testdata/grouped-wire-cases';
import { ShareWizardClient } from './ShareWizardClient';
import * as useMockConfig from '@/hooks/useMockConfig';

vi.mock('@/hooks/useMockConfig');
vi.mock('next/navigation', () => ({ useSearchParams: () => new URLSearchParams() }));

const fixture = loadHelperGroupFixture(helperFixtureSource, HELPER_GROUP_REQUIRED_ROLES);
const decodeCases = readWireDecodeFixture();

const owner = fixture.owners.find((row) => row.role === 'owner-with-helpers')!;
const member = fixture.members.find((row) => row.role === 'eligible-member')!;

function ownerSpec(row: FixtureOwner): GroupedSyncSessionSpec {
  return {
    id: row.id,
    harness: 'codex',
    startTime: row.startTime,
    durationMins: 1,
    totalTokens: 100,
    turnCount: 1,
    project: row.project,
    projectHash: row.projectHash,
    preview: row.preview,
    syncStatus: row.syncStatus,
    helperGroups: row.groups,
  };
}

function memberSpec(row: FixtureMember): GroupedSyncSessionSpec {
  return {
    id: row.id,
    harness: 'codex',
    startTime: '2026-08-12T07:00:00Z',
    durationMins: 1,
    totalTokens: 10,
    turnCount: row.turnCount,
    toolCallCount: 0,
    project: 'alpha',
    projectHash: 'hash-alpha',
    preview: row.preview,
    syncStatus: row.syncStatus,
    helperGroups: row.helperGroups,
  };
}

/** A valid grouped list whose first item is the helper owner. */
function listBaseline(): unknown {
  const contexts = fixture.contexts.map((context) => ({
    groupId: context.groupId,
    ownerStatus: context.ownerStatus,
    helperGroups: context.helperGroups,
  }));
  return buildGroupedSyncResponse([ownerSpec(owner)], contexts);
}

/** A valid member page whose first member is a transcript. */
function membersBaseline(): unknown {
  return buildGroupedMembersResponse([memberSpec(member)]);
}

function parsePath(path: string): Array<string | number> {
  const tokens: Array<string | number> = [];
  for (const part of path.split('.')) {
    const match = /^([^[\]]+)((?:\[\d+\])*)$/.exec(part);
    if (!match) throw new Error(`wire-case mutation path ${JSON.stringify(path)} is not parseable`);
    tokens.push(match[1]);
    for (const index of match[2].matchAll(/\[(\d+)\]/g)) tokens.push(Number(index[1]));
  }
  return tokens;
}

/* The mutation helpers walk parsed JSON test input; the typed loader under test
   re-validates the payload, so the walk below cannot weaken validation. */
function parentOf(root: unknown, tokens: Array<string | number>): Record<string, unknown> | undefined {
  let node: unknown = root;
  for (const token of tokens.slice(0, -1)) {
    node = (node as Record<string, unknown> | undefined)?.[String(token)];
    if (node === undefined || node === null) return undefined;
  }
  return node as Record<string, unknown> | undefined;
}

function applyWireCase(base: unknown, testCase: WireDecodeCase): unknown {
  const document = JSON.parse(JSON.stringify(base)) as unknown;
  for (const path of testCase.drop) {
    const tokens = parsePath(path);
    const parent = parentOf(document, tokens);
    if (parent) delete parent[String(tokens[tokens.length - 1])];
  }
  for (const mutation of testCase.set) {
    const tokens = parsePath(mutation.path);
    const parent = parentOf(document, tokens);
    if (parent) parent[String(tokens[tokens.length - 1])] = mutation.value;
  }
  return document;
}

function response(body: unknown, status = 200) {
  return { ok: status < 400, status, json: async () => body, text: async () => '' };
}

function installFetch(listPayload: unknown, membersPayload: unknown) {
  const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
    const requested = new URL(String(input), 'http://mounted.test');
    switch (requested.pathname) {
      case '/api/v1/sync/sessions':
        return response(listPayload);
      case '/api/v1/web/discovery':
        return response({ items: [{ sessionId: owner.id, locationLabel: 'same label', repositoryLocationId: 'location-a', branch: 'main', selectionStatus: 'selected' }] });
      case '/api/v1/annotations':
        return response({ annotations: [] });
      case '/api/v1/sync/redactions':
        return response({ categories: [] });
      case '/api/v1/sync/push':
        return response({ new: 1, updated: 0, skipped: 0, errors: 0, sessions: [] });
      default:
        if (requested.pathname.startsWith('/api/v1/session-groups/')) return response(membersPayload);
        throw new Error(`unexpected wire-decode mounted fetch: ${String(input)}`);
    }
  });
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

describe('mounted Share chooser wire decoding', () => {
  beforeEach(() => {
    vi.mocked(useMockConfig.useMockConfig).mockReturnValue({ config: { enabled: false, web: [], tui: [] }, loading: false, error: null, refetch: vi.fn() });
  });
  afterEach(() => { vi.unstubAllGlobals(); vi.clearAllMocks(); });

  it('mounts the chooser for the unmutated baseline payloads', async () => {
    installFetch(listBaseline(), membersBaseline());
    render(<ShareWizardClient />);
    const region = await screen.findByRole('region', { name: `project ${owner.project}` });
    expect(region).toBeInTheDocument();
    expect(screen.getByRole('checkbox', { name: `select session ${owner.id}` })).toBeInTheDocument();
    expect(screen.queryByText(/could not read the grouped sessions response/)).not.toBeInTheDocument();
  });

  it.each(decodeCases.filter((testCase) => testCase.base === 'list'))('stops the chooser for the $name list mutation', async (testCase) => {
    installFetch(applyWireCase(listBaseline(), testCase), membersBaseline());
    render(<ShareWizardClient />);

    expect(await screen.findByText(/could not read the grouped sessions response/)).toBeInTheDocument();
    expect(screen.getByText('Retry')).toBeInTheDocument();
    // No guessed rows or counts: the chooser body never mounted.
    expect(document.querySelector('[aria-label="choose sessions to contribute"]')).toBeNull();
    expect(document.querySelector('.gms-tally')).toBeNull();
  });

  it.each(decodeCases.filter((testCase) => testCase.base === 'members'))('stops the member disclosure for the $name member mutation', async (testCase) => {
    const user = userEvent.setup();
    installFetch(listBaseline(), applyWireCase(membersBaseline(), testCase));
    render(<ShareWizardClient />);

    await screen.findByRole('checkbox', { name: `select session ${owner.id}` });
    const group = document.querySelector(`[data-group-id="${owner.groups[0].groupId}"]`) as HTMLElement;
    expect(group).not.toBeNull();
    await user.click(within(group).getByRole('button', { name: new RegExp(`${owner.groups[0].helperThreadCount} helper thread`) }));

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toMatch(/could not read the grouped sessions response/);
    // No guessed member rows or counts.
    await waitFor(() => expect(document.querySelector('[data-thread-id]')).toBeNull());
    expect((document.querySelector('.gms-tally') as HTMLElement).textContent).toContain('0 selected');
  });
});
