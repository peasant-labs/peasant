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
  assertGroupedProjectScope,
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

const traversalManifestSource = readFileSync(
  resolve(process.cwd(), 'src/lib/api/testdata/grouped_search_traversal.manifest.yaml'),
  'utf8',
);
const traversalCasesSource = readFileSync(
  resolve(process.cwd(), 'src/lib/api/testdata/grouped_search_traversal.yaml'),
  'utf8',
);

type TraversalResponse = {
  groupId: string;
  scope: string;
  page: number;
  outcome: 'ok' | 'expired';
  body: unknown;
};

type TraversalMemberRequest = {
  groupId: string;
  scope: string;
  page: number;
};

type TraversalCase = {
  name: string;
  query: string;
  searchLimit: number;
  searchPayload: unknown;
  responses: TraversalResponse[];
  expectedMemberRequests: TraversalMemberRequest[];
  expectedSessionIds: string[];
};

const TRAVERSAL_CASE_FIELDS = [
  'name',
  'query',
  'searchLimit',
  'searchPayload',
  'responses',
  'expectedMemberRequests',
  'expectedSessionIds',
] as const;
const TRAVERSAL_RESPONSE_FIELDS = ['groupId', 'scope', 'page', 'outcome', 'body'] as const;
const TRAVERSAL_REQUEST_FIELDS = ['groupId', 'scope', 'page'] as const;
const TRAVERSAL_MANIFEST_FIELDS = [
  'expectedCount',
  'requiredNames',
  'requiredOutcomes',
  'expectedLoaderMutationCount',
  'loaderMutations',
] as const;

function loadGroupedTraversalFixture(
  manifestValue = traversalManifestSource,
  casesValue = traversalCasesSource,
): TraversalCase[] {
  const manifest = requireRecord(
    parseStrictYAML(manifestValue, 'grouped search traversal manifest'),
    'grouped search traversal manifest',
  );
  requireExactRequiredFields(manifest, TRAVERSAL_MANIFEST_FIELDS, 'grouped search traversal manifest');
  const requiredNames = manifest.requiredNames as unknown[];
  if (
    !Number.isSafeInteger(manifest.expectedCount)
    || !Array.isArray(requiredNames)
    || requiredNames.length !== manifest.expectedCount
    || requiredNames.some((name) => typeof name !== 'string' || name.length === 0)
    || new Set(requiredNames).size !== requiredNames.length
  ) {
    throw new Error('grouped search traversal manifest must carry one independent, unique required-name per expected case');
  }
  if (
    !Array.isArray(manifest.requiredOutcomes)
    || manifest.requiredOutcomes.length === 0
    || manifest.requiredOutcomes.some((outcome) => typeof outcome !== 'string')
  ) {
    throw new Error('grouped search traversal manifest must name at least one required response outcome');
  }
  if (
    !Number.isSafeInteger(manifest.expectedLoaderMutationCount)
    || !Array.isArray(manifest.loaderMutations)
    || manifest.loaderMutations.length !== manifest.expectedLoaderMutationCount
  ) {
    throw new Error('grouped search traversal manifest must carry one loader mutation per expected mutation');
  }
  const outcomes = manifest.requiredOutcomes as readonly string[];

  const root = requireRecord(parseStrictYAML(casesValue, 'grouped search traversal cases'), 'grouped search traversal cases');
  requireExactRequiredFields(root, ['cases'], 'grouped search traversal cases');
  if (!Array.isArray(root.cases)) throw new Error('grouped search traversal cases.cases must be an array');
  const rows = root.cases.map((value, index) => requireRecord(value, `grouped search traversal cases.cases[${index}]`));
  requireUniqueNames(rows, 'grouped search traversal cases.cases');
  rows.forEach((row, index) => {
    const label = `grouped search traversal cases.cases[${index}]`;
    requireExactRequiredFields(row, TRAVERSAL_CASE_FIELDS, label);
    if (typeof row.query !== 'string' || row.query.length === 0) {
      throw new Error(`${label}.query must be a non-empty string`);
    }
    if (!Number.isSafeInteger(row.searchLimit) || (row.searchLimit as number) < 1) {
      throw new Error(`${label}.searchLimit must be a positive integer`);
    }
    if (!Array.isArray(row.responses)) throw new Error(`${label}.responses must be an array`);
    (row.responses as unknown[]).forEach((value, responseIndex) => {
      const response = requireRecord(value, `${label}.responses[${responseIndex}]`);
      requireExactRequiredFields(response, TRAVERSAL_RESPONSE_FIELDS, `${label}.responses[${responseIndex}]`);
      if (typeof response.outcome !== 'string' || !outcomes.includes(response.outcome)) {
        throw new Error(`${label}.responses[${responseIndex}].outcome has an invalid outcome ${String(response.outcome)}`);
      }
      if (!Number.isSafeInteger(response.page) || (response.page as number) < 1) {
        throw new Error(`${label}.responses[${responseIndex}].page must be a positive integer`);
      }
    });
    if (!Array.isArray(row.expectedMemberRequests)) {
      throw new Error(`${label}.expectedMemberRequests must be an array`);
    }
    (row.expectedMemberRequests as unknown[]).forEach((value, requestIndex) => {
      const request = requireRecord(value, `${label}.expectedMemberRequests[${requestIndex}]`);
      requireExactRequiredFields(request, TRAVERSAL_REQUEST_FIELDS, `${label}.expectedMemberRequests[${requestIndex}]`);
      if (!Number.isSafeInteger(request.page) || (request.page as number) < 1) {
        throw new Error(`${label}.expectedMemberRequests[${requestIndex}].page must be a positive integer`);
      }
    });
    if (!Array.isArray(row.expectedSessionIds) || row.expectedSessionIds.some((id) => typeof id !== 'string')) {
      throw new Error(`${label}.expectedSessionIds must be a string array`);
    }
  });
  const names = rows.map((row) => row.name);
  if (rows.length !== manifest.expectedCount || requiredNames.some((name) => !names.includes(name))) {
    throw new Error('grouped search traversal manifest must name exactly the required cases; a case is missing or renamed');
  }
  return rows as unknown as TraversalCase[];
}

const traversalFixture = loadGroupedTraversalFixture();

/** The (groupId, scope, page) a member URL names, or null for a non-member URL. */
function memberRequestFromUrl(raw: string): TraversalMemberRequest | null {
  const url = new URL(raw);
  const match = url.pathname.match(/^\/api\/v1\/session-groups\/([^/]+)\/members$/);
  if (!match) return null;
  return {
    groupId: decodeURIComponent(match[1]),
    scope: url.searchParams.get('scope') ?? '',
    page: Number(url.searchParams.get('page')),
  };
}

function requestKey(request: TraversalMemberRequest): string {
  return `${request.groupId}\u0000${request.scope}\u0000${request.page}`;
}

describe('grouped search navigation', () => {
  it('rejects every traversal loader mutation', () => {
    const manifest = requireRecord(
      parseStrictYAML(traversalManifestSource, 'grouped search traversal manifest'),
      'grouped search traversal manifest',
    );
    for (const mutationValue of manifest.loaderMutations as unknown[]) {
      const mutation = requireRecord(mutationValue, 'traversal loader mutation');
      const target = mutation.target === 'manifest' ? traversalManifestSource : traversalCasesSource;
      const mutated = replaceExactlyOnce(target, String(mutation.find), String(mutation.replace), String(mutation.name));
      expect(
        () => loadGroupedTraversalFixture(
          mutation.target === 'manifest' ? mutated : traversalManifestSource,
          mutation.target === 'cases' ? mutated : traversalCasesSource,
        ),
        String(mutation.name),
      ).toThrow(new RegExp(String(mutation.expectedError)));
    }
  });

  for (const testCase of traversalFixture) {
    it(`navigates grouped search: ${testCase.name}`, async () => {
      const responses = new Map(testCase.responses.map((response) => [requestKey(response), response]));
      const memberRequests: TraversalMemberRequest[] = [];
      const fetchMock = vi.fn((input: unknown) => {
        const url = String(input);
        if (url.includes('/api/v1/search')) return okResponse(testCase.searchPayload);
        const request = memberRequestFromUrl(url);
        if (!request) throw new Error(`unexpected request ${url}`);
        memberRequests.push(request);
        const response = responses.get(requestKey(request));
        if (!response) throw new Error(`no fixture response for ${url}`);
        if (response.outcome === 'expired') return errorResponse(409, response.body);
        return okResponse(response.body);
      });
      vi.stubGlobal('fetch', fetchMock);
      try {
        const rows = await fetchGroupedSearchMatches(testCase.query, testCase.searchLimit);
        expect(rows.map((row) => row.sessionId)).toEqual(testCase.expectedSessionIds);
        expect(memberRequests).toEqual(testCase.expectedMemberRequests);

        // Every member request replays the issued scope and paging only: the
        // query is never widened with an all-helper or cross-project filter.
        for (const call of fetchMock.mock.calls) {
          const request = memberRequestFromUrl(String(call[0]));
          if (!request) continue;
          const keys = [...new URL(String(call[0])).searchParams.keys()].sort();
          expect(keys).toEqual(['limit', 'page', 'scope']);
        }
      } finally {
        vi.unstubAllGlobals();
        vi.restoreAllMocks();
      }
    });
  }
});

describe('grouped project scope', () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn(() => okResponse(listPayload));
    vi.stubGlobal('fetch', fetchMock);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it('sends the project filter and no other narrowing parameter', async () => {
    await fetchGroupedLocalSessions({ projectHash: 'a'.repeat(64) });
    const url = new URL(String(fetchMock.mock.calls[0][0]));
    expect(url.pathname).toBe('/api/v1/sessions');
    expect(url.searchParams.get('view')).toBe('grouped');
    expect(url.searchParams.get('project')).toBe('a'.repeat(64));
    expect([...url.searchParams.keys()].sort()).toEqual(['project', 'view']);
  });

  it('accepts a response whose transcript rows all belong to the requested project', () => {
    const payload = decodeGroupedLocalList({
      ...listPayload,
      totalItems: 1,
      ordinarySessionTotal: 1,
      items: [
        {
          kind: 'transcript',
          transcript: {
            session: { ...sessionSummary('agent-a1'), projectHash: 'a'.repeat(64) },
          },
        },
      ],
    });
    expect(() => assertGroupedProjectScope(payload, 'a'.repeat(64))).not.toThrow();
  });

  it('refuses a response that carries another project under a project-scoped request', () => {
    const payload = decodeGroupedLocalList({
      ...listPayload,
      totalItems: 1,
      ordinarySessionTotal: 1,
      items: [
        {
          kind: 'transcript',
          transcript: {
            session: { ...sessionSummary('agent-a1'), projectHash: 'b'.repeat(64) },
          },
        },
      ],
    });
    expect(() => assertGroupedProjectScope(payload, 'a'.repeat(64))).toThrow(
      /carries other projects' sessions/i,
    );
  });
});

function sessionSummary(id: string) {
  return {
    id,
    harness: 'codex' as const,
    startTime: '2026-01-01T00:00:00Z',
    durationMins: 1,
    turnCount: 1,
    totalTokens: 1,
    toolCallCount: 0,
  };
}
