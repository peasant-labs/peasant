import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  parseStrictYAML,
  requireExactRequiredFields,
  requireRecord,
  requireUniqueNames,
} from '@/test/strictYaml';
import {
  GroupScopeExpiredError,
  decodeGroupedLocalList,
  decodeGroupedMembers,
  fetchGroupedLocalSearch,
  fetchGroupedLocalSessions,
  fetchGroupedSearchMatches,
  fetchHelperGroupMembers,
  isGroupScopeExpired,
} from './grouped';

const manifestSource = readFileSync(
  resolve(process.cwd(), 'src/lib/api/testdata/grouped_local_lists.manifest.yaml'),
  'utf8',
);
const casesSource = readFileSync(
  resolve(process.cwd(), 'src/lib/api/testdata/grouped_local_lists.yaml'),
  'utf8',
);

type GroupedCase = {
  name: string;
  kind: 'list' | 'members';
  payload: unknown;
  expected: {
    valid: boolean;
    itemKinds?: string[];
    groupIds?: string[];
    memberIds?: string[];
    total?: number;
    totalItems?: number;
    ordinarySessionTotal?: number;
    helperThreadTotal?: number;
  };
};

const CASE_FIELDS = ['name', 'kind', 'payload', 'expected'] as const;
const MANIFEST_FIELDS = [
  'expectedCount',
  'requiredNames',
  'requiredKinds',
  'expectedLoaderMutationCount',
  'loaderMutations',
] as const;
const KINDS: readonly GroupedCase['kind'][] = ['list', 'members'];

function replaceExactlyOnce(source: string, find: string, replace: string, label: string): string {
  const count = source.split(find).length - 1;
  if (count !== 1) throw new Error(`${label} mutation anchor must occur exactly once, received ${count}`);
  return source.replace(find, replace);
}

function loadGroupedFixture(manifestValue = manifestSource, casesValue = casesSource): GroupedCase[] {
  const manifest = requireRecord(
    parseStrictYAML(manifestValue, 'grouped local lists manifest'),
    'grouped local lists manifest',
  );
  requireExactRequiredFields(manifest, MANIFEST_FIELDS, 'grouped local lists manifest');
  const requiredNames = manifest.requiredNames as unknown[];
  if (
    !Number.isSafeInteger(manifest.expectedCount)
    || !Array.isArray(requiredNames)
    || requiredNames.length !== manifest.expectedCount
    || requiredNames.some((name) => typeof name !== 'string' || name.length === 0)
    || new Set(requiredNames).size !== requiredNames.length
  ) {
    throw new Error('grouped local lists manifest must carry one independent, unique required-name per expected case');
  }
  if (
    !Array.isArray(manifest.requiredKinds)
    || JSON.stringify(manifest.requiredKinds) !== JSON.stringify(KINDS)
  ) {
    throw new Error('grouped local lists manifest must name every closed decode kind exactly once');
  }
  if (
    !Number.isSafeInteger(manifest.expectedLoaderMutationCount)
    || !Array.isArray(manifest.loaderMutations)
    || manifest.loaderMutations.length !== manifest.expectedLoaderMutationCount
  ) {
    throw new Error('grouped local lists manifest must carry one loader mutation per expected mutation');
  }

  const root = requireRecord(parseStrictYAML(casesValue, 'grouped local lists cases'), 'grouped local lists cases');
  requireExactRequiredFields(root, ['cases'], 'grouped local lists cases');
  if (!Array.isArray(root.cases)) throw new Error('grouped local lists cases.cases must be an array');
  const rows = root.cases.map((value, index) => requireRecord(value, `grouped local lists cases.cases[${index}]`));
  requireUniqueNames(rows, 'grouped local lists cases.cases');
  rows.forEach((row, index) => {
    requireExactRequiredFields(row, CASE_FIELDS, `grouped local lists cases.cases[${index}]`);
    if (!KINDS.includes(row.kind as GroupedCase['kind'])) {
      throw new Error(`grouped local lists cases.cases[${index}].kind must be list or members`);
    }
    requireRecord(row.expected, `grouped local lists cases.cases[${index}].expected`);
    if (typeof row.expected !== 'object' || (row.expected as Record<string, unknown>).valid !== true
      && (row.expected as Record<string, unknown>).valid !== false) {
      throw new Error(`grouped local lists cases.cases[${index}].expected.valid must be a boolean`);
    }
  });
  const names = rows.map((row) => row.name);
  if (rows.length !== manifest.expectedCount || requiredNames.some((name) => !names.includes(name))) {
    throw new Error('grouped local lists manifest must name exactly the required cases; a case is missing or renamed');
  }
  return rows as unknown as GroupedCase[];
}

const fixture = loadGroupedFixture();

function projectList(payload: ReturnType<typeof decodeGroupedLocalList>) {
  return {
    itemKinds: payload.items.map((item) => item.kind),
    groupIds: payload.items.flatMap((item) => (item.helperGroups ?? []).map((group) => group.groupId)),
    totalItems: payload.totalItems,
    ordinarySessionTotal: payload.ordinarySessionTotal,
    helperThreadTotal: payload.helperThreadTotal,
  };
}

function projectMembers(payload: ReturnType<typeof decodeGroupedMembers>) {
  return {
    memberIds: payload.members.flatMap((item) => (item.transcript ? [item.transcript.session.id] : [])),
    total: payload.total,
  };
}

describe('grouped local decode fixture', () => {
  it('rejects every loader mutation', () => {
    const manifest = requireRecord(parseStrictYAML(manifestSource, 'grouped local lists manifest'), 'grouped local lists manifest');
    for (const mutationValue of manifest.loaderMutations as unknown[]) {
      const mutation = requireRecord(mutationValue, 'grouped loader mutation');
      const target = mutation.target === 'manifest' ? manifestSource : casesSource;
      const mutated = replaceExactlyOnce(target, String(mutation.find), String(mutation.replace), String(mutation.name));
      expect(
        () => loadGroupedFixture(
          mutation.target === 'manifest' ? mutated : manifestSource,
          mutation.target === 'cases' ? mutated : casesSource,
        ),
        String(mutation.name),
      ).toThrow(new RegExp(String(mutation.expectedError)));
    }
  });

  for (const testCase of fixture) {
    it(`${testCase.kind}: ${testCase.name}`, () => {
      if (!testCase.expected.valid) {
        expect(() =>
          testCase.kind === 'list' ? decodeGroupedLocalList(testCase.payload) : decodeGroupedMembers(testCase.payload),
        ).toThrow(/could not be decoded/i);
        return;
      }
      if (testCase.kind === 'list') {
        const { valid: _valid, ...expected } = testCase.expected;
        expect(projectList(decodeGroupedLocalList(testCase.payload))).toEqual(expected);
      } else {
        const { valid: _valid, ...expected } = testCase.expected;
        expect(projectMembers(decodeGroupedMembers(testCase.payload))).toEqual(expected);
      }
    });
  }
});

function okResponse(payload: unknown) {
  return Promise.resolve({ ok: true, json: async () => payload } as Response);
}

function errorResponse(status: number, body: unknown) {
  return Promise.resolve({
    ok: false,
    status,
    text: async () => JSON.stringify(body),
  } as Response);
}

const listPayload = {
  items: [],
  page: 1,
  limit: 20,
  totalItems: 0,
  ordinarySessionTotal: 0,
  helperThreadTotal: 0,
};

describe('grouped local REST client', () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn(() => okResponse(listPayload));
    vi.stubGlobal('fetch', fetchMock);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it('fetchGroupedLocalSessions asks for the opt-in grouped view and decodes the payload', async () => {
    const payload = await fetchGroupedLocalSessions();
    const url = String(fetchMock.mock.calls[0][0]);
    expect(url).toContain('/api/v1/sessions?');
    expect(new URL(url).searchParams.get('view')).toBe('grouped');
    expect(payload.items).toEqual([]);
  });

  it('fetchGroupedLocalSearch carries the query, limit and grouped view, and never a narrowing filter', async () => {
    await fetchGroupedLocalSearch('needle', 7);
    const url = new URL(String(fetchMock.mock.calls[0][0]));
    expect(url.pathname).toBe('/api/v1/search');
    expect(url.searchParams.get('q')).toBe('needle');
    expect(url.searchParams.get('limit')).toBe('7');
    expect(url.searchParams.get('view')).toBe('grouped');
    expect([...url.searchParams.keys()].sort()).toEqual(['limit', 'q', 'view']);
  });

  it('fetchHelperGroupMembers sends only the opaque scope and paging', async () => {
    fetchMock.mockReturnValueOnce(okResponse({ members: [], page: 2, limit: 1, total: 4 }));
    const payload = await fetchHelperGroupMembers({ groupId: 'hg_p1', scope: 'scope-p1', page: 2, limit: 1 });
    const url = new URL(String(fetchMock.mock.calls[0][0]));
    expect(url.pathname).toBe('/api/v1/session-groups/hg_p1/members');
    expect(url.searchParams.get('scope')).toBe('scope-p1');
    expect(url.searchParams.get('page')).toBe('2');
    expect(url.searchParams.get('limit')).toBe('1');
    expect([...url.searchParams.keys()].sort()).toEqual(['limit', 'page', 'scope']);
    expect(payload.total).toBe(4);
  });

  it('turns a 409 group_scope_expired into the typed fail-closed refusal', async () => {
    fetchMock.mockReturnValueOnce(errorResponse(409, { error: 'scope expired', code: 'group_scope_expired' }));
    const failure = await fetchHelperGroupMembers({ groupId: 'hg_p1', scope: 'stale' }).catch((error) => error);
    expect(isGroupScopeExpired(failure)).toBe(true);
    expect(failure).toBeInstanceOf(GroupScopeExpiredError);
    expect((failure as GroupScopeExpiredError).groupId).toBe('hg_p1');
  });

  it('surfaces a non-scope failure as a decode-safe discovery error', async () => {
    fetchMock.mockReturnValueOnce(errorResponse(500, { error: 'boom' }));
    const failure = await fetchHelperGroupMembers({ groupId: 'hg_p1', scope: 'scope-p1' }).catch((error) => error);
    expect(isGroupScopeExpired(failure)).toBe(false);
    expect(String(failure)).toContain('500');
  });
});

function match(sessionId: string, entryIndex: number) {
  return {
    sessionId,
    project: '/work/alpha-project',
    projectHash: 'a'.repeat(64),
    entryIndex,
    role: 'user' as const,
    snippet: `${sessionId} hit`,
    score: 1,
  };
}

describe('grouped search navigation', () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  const helperOnlyPayload = {
    items: [
      {
        kind: 'transcript',
        transcript: { session: sessionSummaryFor('agent-a1'), matches: [match('agent-a1', 0)] },
      },
      {
        kind: 'context_container',
        context: { groupId: 'hg_h1', ownerStatus: 'known_unavailable' },
        helperGroups: [{ groupId: 'hg_h1', purpose: 'helper_review', helperThreadCount: 1, memberScope: 'scope-h1' }],
      },
    ],
    page: 1,
    limit: 20,
    totalItems: 2,
    ordinarySessionTotal: 1,
    helperThreadTotal: 1,
  };

  function sessionSummaryFor(id: string) {
    return {
      id,
      harness: 'codex',
      startTime: '2026-01-01T00:00:00Z',
      durationMins: 1,
      turnCount: 1,
      totalTokens: 1,
      toolCallCount: 0,
    };
  }

  it('expands a helper-only search container from its issued scope, keeping ordinary hits', async () => {
    fetchMock
      .mockReturnValueOnce(okResponse(helperOnlyPayload))
      .mockReturnValueOnce(
        okResponse({
          members: [{ kind: 'transcript', transcript: { session: sessionSummaryFor('agent-b1'), matches: [match('agent-b1', 3)] } }],
          page: 1,
          limit: 20,
          total: 1,
        }),
      );

    const rows = await fetchGroupedSearchMatches('needle', 20);
    expect(rows.map((row) => row.sessionId)).toEqual(['agent-a1', 'agent-b1']);

    const searchUrl = new URL(String(fetchMock.mock.calls[0][0]));
    expect(searchUrl.searchParams.get('view')).toBe('grouped');
    const memberUrl = new URL(String(fetchMock.mock.calls[1][0]));
    expect(memberUrl.pathname).toBe('/api/v1/session-groups/hg_h1/members');
    expect(memberUrl.searchParams.get('scope')).toBe('scope-h1');
    expect([...memberUrl.searchParams.keys()].sort()).toEqual(['limit', 'page', 'scope']);
  });

  it('omits helper hits when the helper scope expired instead of widening the query', async () => {
    fetchMock
      .mockReturnValueOnce(okResponse(helperOnlyPayload))
      .mockReturnValueOnce(errorResponse(409, { error: 'expired', code: 'group_scope_expired' }));

    const rows = await fetchGroupedSearchMatches('needle', 20);
    expect(rows.map((row) => row.sessionId)).toEqual(['agent-a1']);
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });
});
