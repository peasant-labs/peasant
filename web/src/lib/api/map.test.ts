import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import {
  fetchMapGraph,
  fetchMapNodeDetail,
  fetchProjectSummaries,
  fetchProjectTasks,
  fetchReviewChanges,
  fetchChangeDetail,
  fetchChangeDiff,
  fetchSearch,
} from './map';
import { DiscoveryErrorCode, DiscoveryRequestError } from './errors';
import { parseProjectHash } from '@/lib/navigation/projectRoutes';
import {
  AllChangeBindings,
  AllDiffLineKinds,
  AllEdgeViolationKinds,
  AllFileChangeStatuses,
  AllMapNodeKinds,
  Harness,
} from '@peasant-labs/schema';
import {
  parseStrictYAML,
  requireExactRequiredFields,
  requireRecord,
  requireUniqueNames,
} from '@/test/strictYaml';

const PROJECT_HASH = parseProjectHash('a'.repeat(64))!;

type NullableArrayCase = {
  name: string;
  operation: 'project-summaries' | 'map-graph' | 'node-detail' | 'project-tasks' | 'change-detail' | 'change-diff' | 'search';
  payload: Record<string, unknown>;
  expected: Record<string, unknown>;
};

const nullableManifestSource = readFileSync(resolve(process.cwd(), 'src/lib/api/testdata/nullable_arrays.manifest.yaml'), 'utf8');
const nullableCasesSource = readFileSync(resolve(process.cwd(), 'src/lib/api/testdata/nullable_arrays.yaml'), 'utf8');

function replaceExactlyOnce(source: string, find: string, replace: string, label: string): string {
  const count = source.split(find).length - 1;
  if (count !== 1) throw new Error(`${label} mutation anchor must occur exactly once, received ${count}`);
  return source.replace(find, replace);
}

function loadNullableArrayCases(manifestSource = nullableManifestSource, casesSource = nullableCasesSource): NullableArrayCase[] {
  const manifest = requireRecord(
    parseStrictYAML(
      manifestSource,
      'nullable REST arrays manifest',
    ),
    'nullable REST arrays manifest',
  );
  requireExactRequiredFields(manifest, ['expectedCount', 'requiredNames', 'requiredOperations', 'expectedNormalizedPaths', 'expectedLoaderMutationCount', 'loaderMutations'], 'nullable REST arrays manifest');
  if (!Number.isSafeInteger(manifest.expectedCount) || !Array.isArray(manifest.requiredNames) || !Array.isArray(manifest.requiredOperations) || !Array.isArray(manifest.expectedNormalizedPaths)) throw new Error('nullable REST arrays manifest requires independent count, name, operation, and normalized-path inventories');
  const requiredNames = manifest.requiredNames as unknown[];
  if (requiredNames.some((name) => typeof name !== 'string' || name.length === 0) || new Set(requiredNames).size !== requiredNames.length) throw new Error('nullable REST arrays manifest names must be unique nonempty strings');

  const root = requireRecord(
    parseStrictYAML(
      casesSource,
      'nullable REST arrays cases',
    ),
    'nullable REST arrays cases',
  );
  requireExactRequiredFields(root, ['cases'], 'nullable REST arrays cases');
  if (!Array.isArray(root.cases)) throw new Error('nullable REST arrays cases must be an array');
  const rows = root.cases.map((row, index) => requireRecord(row, `nullable REST arrays cases[${index}]`));
  requireUniqueNames(rows, 'nullable REST arrays cases');
  const operations = ['project-summaries', 'map-graph', 'node-detail', 'project-tasks', 'change-detail', 'change-diff', 'search'];
  rows.forEach((row, index) => {
    requireExactRequiredFields(row, ['name', 'operation', 'payload', 'expected'], `nullable REST arrays cases[${index}]`);
    if (typeof row.name !== 'string' || !operations.includes(String(row.operation))) throw new Error(`nullable REST arrays cases[${index}] has invalid scalar fields`);
    requireRecord(row.payload, `nullable REST arrays cases[${index}].payload`);
    requireRecord(row.expected, `nullable REST arrays cases[${index}].expected`);
  });
  const names = rows.map((row) => row.name);
  const operationInventory = rows.map((row) => row.operation);
  if (rows.length !== manifest.expectedCount || requiredNames.length !== rows.length || requiredNames.some((name) => !names.includes(name)) || JSON.stringify(operationInventory) !== JSON.stringify(manifest.requiredOperations)) throw new Error('nullable REST arrays cases do not match their independent manifest');
  const paths = new Set<string>();
  const collect = (value: unknown, expected: unknown, path: string): void => {
    if (value === null && Array.isArray(expected)) { paths.add(path); return; }
    if (Array.isArray(value) && Array.isArray(expected)) {
      value.forEach((child, index) => collect(child, expected[index], `${path}[]`));
      return;
    }
    if (value && expected && typeof value === 'object' && typeof expected === 'object') {
      for (const [key, child] of Object.entries(value)) collect(child, (expected as Record<string, unknown>)[key], path ? `${path}.${key}` : key);
    }
  };
  rows.forEach((row) => collect(row.payload, row.expected, ''));
  const actualPaths = [...paths];
  if (JSON.stringify(actualPaths) !== JSON.stringify(manifest.expectedNormalizedPaths)) throw new Error(`nullable REST arrays normalized paths do not match their independent manifest: ${JSON.stringify(actualPaths)}`);
  if (!Array.isArray(manifest.loaderMutations) || manifest.loaderMutations.length !== manifest.expectedLoaderMutationCount) throw new Error('nullable REST arrays loader mutation inventory is incomplete');
  return rows as NullableArrayCase[];
}

const nullableArrayCases = loadNullableArrayCases();

type RuntimeOperation = 'map-graph' | 'node-detail' | 'change-detail' | 'change-diff' | 'review-list';
type RuntimeInvalidCase = {
  name: string;
  operation: RuntimeOperation;
  field: string;
  value: string;
  expectedError: string;
};
type RuntimeContractFixture = {
  validValues: Record<string, string[]>;
  invalidCases: RuntimeInvalidCase[];
};

function loadRuntimeContract(): RuntimeContractFixture {
  const manifest = requireRecord(parseStrictYAML(
    readFileSync(resolve(process.cwd(), 'src/lib/api/testdata/runtime_contract.manifest.yaml'), 'utf8'),
    'REST runtime contract manifest',
  ), 'REST runtime contract manifest');
  requireExactRequiredFields(manifest, ['expectedValidFamilies', 'expectedInvalidCount', 'requiredInvalidFields', 'requiredInvalidNames', 'expectedMutationCount', 'mutations'], 'REST runtime contract manifest');
  const root = requireRecord(parseStrictYAML(
    readFileSync(resolve(process.cwd(), 'src/lib/api/testdata/runtime_contract.yaml'), 'utf8'),
    'REST runtime contract fixture',
  ), 'REST runtime contract fixture');
  requireExactRequiredFields(root, ['validValues', 'invalidCases'], 'REST runtime contract fixture');
  const validValues = requireRecord(root.validValues, 'REST runtime contract fixture.validValues');
  const families = Object.keys(validValues);
  if (!Array.isArray(manifest.expectedValidFamilies) || JSON.stringify(families) !== JSON.stringify(manifest.expectedValidFamilies)) throw new Error('REST runtime contract valid families do not match their independent manifest');
  for (const family of families) {
    const values = validValues[family];
    if (!Array.isArray(values) || values.some((value) => typeof value !== 'string') || new Set(values).size !== values.length) throw new Error(`REST runtime contract ${family} must contain unique strings`);
  }
  if (!Array.isArray(root.invalidCases)) throw new Error('REST runtime contract invalidCases must be an array');
  const invalidCases = root.invalidCases.map((row, index) => requireRecord(row, `REST runtime contract invalidCases[${index}]`));
  requireUniqueNames(invalidCases, 'REST runtime contract invalidCases');
  invalidCases.forEach((row, index) => {
    requireExactRequiredFields(row, ['name', 'operation', 'field', 'value', 'expectedError'], `REST runtime contract invalidCases[${index}]`);
    if (Object.values(row).some((value) => typeof value !== 'string')) throw new Error(`REST runtime contract invalidCases[${index}] requires string fields`);
  });
  if (
    invalidCases.length !== manifest.expectedInvalidCount ||
    JSON.stringify(invalidCases.map((row) => row.name)) !== JSON.stringify(manifest.requiredInvalidNames) ||
    JSON.stringify(invalidCases.map((row) => row.field)) !== JSON.stringify(manifest.requiredInvalidFields)
  ) throw new Error('REST runtime contract invalid inventory does not match its independent manifest');
  if (!Array.isArray(manifest.mutations) || manifest.mutations.length !== manifest.expectedMutationCount) throw new Error('REST runtime contract production mutation inventory is incomplete');
  return { validValues: validValues as Record<string, string[]>, invalidCases: invalidCases as RuntimeInvalidCase[] };
}

const runtimeContract = loadRuntimeContract();

const validMapGraph = (nodeKinds: string[] = [], violationKinds: string[] = []) => ({
  projectHash: PROJECT_HASH,
  repoFound: true,
  generatedAtMs: 1,
  nodes: nodeKinds.map((kind, index) => ({ id: `node-${index}`, kind, name: `node-${index}`, layer: 0, order: index, loc: 1, fileCount: 1, recordedFiles: 1, totalFiles: 1, touchCount: 0, effortDensity: 0 })),
  parsedLanguages: [],
  structureEdges: [],
  activityEdges: [],
  violations: violationKinds.map((kind) => ({ kind, from: 'a', to: 'b' })),
});
const validNodeDetail = (kind: string) => ({ kind, path: 'src/a.ts', loc: 1, recordedFiles: 1, totalFiles: 1, reEdits: 0, retryLoops: 0, sessionCount: 0, taskCount: 0, dependsOn: [], usedBy: [], shapedBy: [], recentCommits: [] });
const validChangeDetail = () => ({
  branch: 'feature', baseRef: 'base', defaultBranch: 'develop', files: [] as Array<Record<string, unknown>>, frictions: [], linesAdded: 0, linesRemoved: 0,
  newEdges: [], newNodes: [], outputTokens: 0, removedEdges: [], removedNodes: [],
  slice: { nodes: [] as Array<Record<string, unknown>>, structureEdges: [], activityEdges: [] }, unrecordedCommits: [], unusual: [], violations: [] as Array<Record<string, unknown>>, work: [] as Array<Record<string, unknown>>,
});
const validDiff = () => ({ branch: 'feature', file: 'src/a.ts', status: 'M', binary: false, truncated: false, hunks: [] as Array<Record<string, unknown>> });
const validReviewList = () => ({ projectHash: PROJECT_HASH, repoFound: true, changes: [], recentCommits: [], sessions: [] as Array<Record<string, unknown>> });

function invalidPayload(fixture: RuntimeInvalidCase): Record<string, unknown> {
  if (fixture.operation === 'map-graph') {
    return fixture.field === 'nodes[].kind' ? validMapGraph([fixture.value]) : validMapGraph([], [fixture.value]);
  }
  if (fixture.operation === 'node-detail') return validNodeDetail(fixture.value);
  if (fixture.operation === 'change-detail') {
    const payload = validChangeDetail();
    if (fixture.field === 'files[].status') payload.files = [{ path: 'a', status: fixture.value, linesAdded: 0, linesRemoved: 0 }];
    if (fixture.field === 'slice.nodes[].kind') payload.slice.nodes = validMapGraph([fixture.value]).nodes;
    if (fixture.field === 'violations[].kind') payload.violations = [{ kind: fixture.value, from: 'a', to: 'b' }];
    if (fixture.field === 'work[].harness' || fixture.field === 'work[].binding') payload.work = [{ sessionId: 's', title: 't', harness: fixture.field.endsWith('harness') ? fixture.value : 'codex', binding: fixture.field.endsWith('binding') ? fixture.value : 'bound', tasks: [] }];
    return payload;
  }
  if (fixture.operation === 'change-diff') {
    const payload = validDiff();
    if (fixture.field === 'status') payload.status = fixture.value;
    else payload.hunks = [{ oldStart: 1, oldLines: 1, newStart: 1, newLines: 1, lines: [{ kind: fixture.value, text: 'x' }] }];
    return payload;
  }
  const payload = validReviewList();
  payload.sessions = [{ sessionId: 's', title: 't', harness: fixture.value, hasCommitBinding: false }];
  return payload;
}

function okResponse(payload: unknown) {
  return Promise.resolve({
    ok: true,
    json: async () => payload,
  } as Response);
}

describe('map REST client', () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn(() => okResponse({}));
    vi.stubGlobal('fetch', fetchMock);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it('rejects every nullable-fixture loader mutation', () => {
    const manifest = requireRecord(parseStrictYAML(nullableManifestSource, 'nullable REST arrays manifest'), 'nullable REST arrays manifest');
    for (const mutationValue of manifest.loaderMutations as unknown[]) {
      const mutation = requireRecord(mutationValue, 'nullable loader mutation');
      const target = mutation.target === 'manifest' ? nullableManifestSource : nullableCasesSource;
      const mutated = replaceExactlyOnce(target, String(mutation.find), String(mutation.replace), String(mutation.name));
      expect(() => loadNullableArrayCases(mutation.target === 'manifest' ? mutated : nullableManifestSource, mutation.target === 'cases' ? mutated : nullableCasesSource), String(mutation.name)).toThrow(new RegExp(String(mutation.expectedError)));
    }
  });

  it('fetchProjectSummaries hits /api/v1/projects/summary and decodes rows', async () => {
    fetchMock.mockReturnValueOnce(
      okResponse({
        projects: [
          {
            projectHash: PROJECT_HASH,
            project: 'alpha-project',
            sessions: 12,
            recordedFiles: 34,
            totalFiles: 37,
            lastWorkMs: 1_770_000_000_000,
            openChanges: 2,
          },
        ],
      }),
    );
    const payload = await fetchProjectSummaries();
    expect(String(fetchMock.mock.calls[0][0])).toContain('/api/v1/projects/summary');
    expect(payload.projects).toHaveLength(1);
    expect(payload.projects[0].projectHash).toBe(PROJECT_HASH);
    expect(payload.projects[0].recordedFiles).toBe(34);
    expect(payload.projects[0].totalFiles).toBe(37);
    expect(payload.projects[0].openChanges).toBe(2);
  });

  // The server's selection-state metadata (a
  // peasant-local addition, not part of the schema module's
  // ProjectSummariesPayload contract) must pass through decoding unchanged.
  it('fetchProjectSummaries decodes the selection field when the server reports an active, narrowing selection', async () => {
    fetchMock.mockReturnValueOnce(
      okResponse({
        projects: [],
        selection: { active: true, hiddenProjects: 2, hiddenSessions: 7593 },
      }),
    );
    const payload = await fetchProjectSummaries();
    expect(payload.selection).toEqual({ active: true, hiddenProjects: 2, hiddenSessions: 7593 });
  });

  it('fetchProjectSummaries defaults selection to inactive/nothing-hidden when the server omits it', async () => {
    fetchMock.mockReturnValueOnce(okResponse({ projects: [] }));
    const payload = await fetchProjectSummaries();
    expect(payload.selection).toEqual({ active: false, hiddenProjects: 0, hiddenSessions: 0 });
  });

  it('fetchMapGraph hits /api/v1/map/{projectHash} without ?commit by default', async () => {
    await fetchMapGraph(PROJECT_HASH);
    const url = String(fetchMock.mock.calls[0][0]);
    expect(url).toContain(`/api/v1/map/${PROJECT_HASH}`);
    expect(url).not.toContain('commit=');
  });

  it('fetchMapGraph appends ?commit=<sha> when given', async () => {
    await fetchMapGraph(PROJECT_HASH, 'deadbeef');
    const url = String(fetchMock.mock.calls[0][0]);
    expect(url).toContain(`/api/v1/map/${PROJECT_HASH}?commit=deadbeef`);
  });

  it('fetchMapNodeDetail hits the node endpoint with an encoded path', async () => {
    fetchMock.mockReturnValueOnce(okResponse({ kind: 'file', dependsOn: [], usedBy: [], shapedBy: [], recentCommits: [] }));
    await fetchMapNodeDetail(PROJECT_HASH, 'web/src/lib/api.ts');
    const url = String(fetchMock.mock.calls[0][0]);
    expect(url).toContain(`/api/v1/map/${PROJECT_HASH}/node?path=web%2Fsrc%2Flib%2Fapi.ts`);
  });

  it('fetchProjectTasks hits the tasks endpoint, with and without ?file=', async () => {
    await fetchProjectTasks(PROJECT_HASH);
    expect(String(fetchMock.mock.calls[0][0])).toContain(
      `/api/v1/map/${PROJECT_HASH}/tasks`,
    );
    expect(String(fetchMock.mock.calls[0][0])).not.toContain('file=');

    await fetchProjectTasks(PROJECT_HASH, 'internal/ingest/pipeline.go');
    expect(String(fetchMock.mock.calls[1][0])).toContain(
      `/api/v1/map/${PROJECT_HASH}/tasks?file=internal%2Fingest%2Fpipeline.go`,
    );
  });

  it('fetchReviewChanges hits /api/v1/review/{projectHash}', async () => {
    fetchMock.mockReturnValueOnce(okResponse({ repoFound: false, projectHash: PROJECT_HASH, changes: [], recentCommits: [], sessions: [] }));
    await fetchReviewChanges(PROJECT_HASH);
    expect(String(fetchMock.mock.calls[0][0])).toContain(
      `/api/v1/review/${PROJECT_HASH}`,
    );
  });

  it('fetchChangeDetail carries the branch (slashes included) in the query string', async () => {
    fetchMock.mockReturnValueOnce(okResponse({
      branch: 'feat/graph-cache',
      baseRef: 'base',
      defaultBranch: 'develop',
      files: [],
      slice: { nodes: [], structureEdges: [], activityEdges: [] },
      newEdges: [],
      removedEdges: [],
      newNodes: [],
      removedNodes: [],
      violations: [],
      work: [],
      unrecordedCommits: [],
      unusual: [],
      frictions: [],
      linesAdded: 0,
      linesRemoved: 0,
      outputTokens: 0,
    }));
    await fetchChangeDetail(PROJECT_HASH, 'feat/graph-cache');
    const url = String(fetchMock.mock.calls[0][0]);
    expect(url).toContain(
      `/api/v1/review/${PROJECT_HASH}/change?branch=feat%2Fgraph-cache`,
    );
  });

  it('fetchSearch builds /api/v1/search?q= and omits limit when not given', async () => {
    await fetchSearch('ingest pipeline');
    const url = String(fetchMock.mock.calls[0][0]);
    expect(url).toContain('/api/v1/search?q=ingest+pipeline');
    expect(url).not.toContain('limit=');
  });

  it('fetchSearch appends &limit=<n> and decodes results', async () => {
    fetchMock.mockReturnValueOnce(
      okResponse({
        query: 'pipeline',
        results: [
          {
            sessionId: 'sess-1',
            project: '/repo/fortuna',
            projectHash: PROJECT_HASH,
            entryIndex: 4,
            role: 'user',
            snippet: 'the [pipeline] here',
            score: 1.5,
          },
        ],
      }),
    );
    const payload = await fetchSearch('pipeline', 20);
    expect(String(fetchMock.mock.calls[0][0])).toContain('/api/v1/search?q=pipeline&limit=20');
    expect(payload.results).toHaveLength(1);
    expect(payload.results[0].sessionId).toBe('sess-1');
    expect(payload.results[0].entryIndex).toBe(4);
  });

  it('matches every schema runtime enum exactly, including every Harness value', () => {
    expect(runtimeContract.validValues.mapNodeKinds).toEqual([...AllMapNodeKinds]);
    expect(runtimeContract.validValues.edgeViolationKinds).toEqual([...AllEdgeViolationKinds]);
    expect(runtimeContract.validValues.fileChangeStatuses).toEqual([...AllFileChangeStatuses]);
    expect(runtimeContract.validValues.diffLineKinds).toEqual([...AllDiffLineKinds]);
    expect(runtimeContract.validValues.changeBindings).toEqual([...AllChangeBindings]);
    expect(runtimeContract.validValues.harnesses).toEqual(Object.values(Harness));
  });

  it('accepts every canonical runtime enum value at its REST trust boundary', async () => {
    fetchMock.mockReturnValueOnce(okResponse(validMapGraph(runtimeContract.validValues.mapNodeKinds, runtimeContract.validValues.edgeViolationKinds)));
    expect((await fetchMapGraph(PROJECT_HASH)).nodes.map((node) => node.kind)).toEqual(runtimeContract.validValues.mapNodeKinds);
    fetchMock.mockReturnValueOnce(okResponse({
      ...validChangeDetail(),
      files: runtimeContract.validValues.fileChangeStatuses.map((status) => ({ path: status, status, linesAdded: 0, linesRemoved: 0 })),
      slice: { ...validChangeDetail().slice, nodes: validMapGraph(runtimeContract.validValues.mapNodeKinds).nodes },
      violations: validMapGraph([], runtimeContract.validValues.edgeViolationKinds).violations,
      work: runtimeContract.validValues.harnesses.flatMap((harness) => runtimeContract.validValues.changeBindings.map((binding) => ({ sessionId: `${harness}-${binding}`, title: 't', harness, binding, tasks: [] }))),
    }));
    const detail = await fetchChangeDetail(PROJECT_HASH, 'feature');
    expect(detail.files.map((file) => file.status)).toEqual(runtimeContract.validValues.fileChangeStatuses);
    expect([...new Set(detail.work.map((session) => session.harness))]).toEqual(runtimeContract.validValues.harnesses);
    fetchMock.mockReturnValueOnce(okResponse({ ...validDiff(), hunks: [{ oldStart: 1, oldLines: 1, newStart: 1, newLines: 1, lines: runtimeContract.validValues.diffLineKinds.map((kind) => ({ kind, text: kind })) }] }));
    expect((await fetchChangeDiff(PROJECT_HASH, 'feature', 'a')).hunks[0].lines.map((line) => line.kind)).toEqual(runtimeContract.validValues.diffLineKinds);
    fetchMock.mockReturnValueOnce(okResponse({ ...validReviewList(), sessions: runtimeContract.validValues.harnesses.map((harness) => ({ sessionId: harness, title: harness, harness, hasCommitBinding: false })) }));
    expect((await fetchReviewChanges(PROJECT_HASH)).sessions.map((session) => session.harness)).toEqual(runtimeContract.validValues.harnesses);
  });

  for (const fixture of runtimeContract.invalidCases) {
    it(fixture.name, async () => {
      fetchMock.mockReturnValueOnce(okResponse(invalidPayload(fixture)));
      let request: Promise<unknown>;
      if (fixture.operation === 'map-graph') request = fetchMapGraph(PROJECT_HASH);
      else if (fixture.operation === 'node-detail') request = fetchMapNodeDetail(PROJECT_HASH, 'src/a.ts');
      else if (fixture.operation === 'change-detail') request = fetchChangeDetail(PROJECT_HASH, 'feature');
      else if (fixture.operation === 'change-diff') request = fetchChangeDiff(PROJECT_HASH, 'feature', 'src/a.ts');
      else request = fetchReviewChanges(PROJECT_HASH);
      await expect(request).rejects.toThrow(new RegExp(fixture.expectedError));
      await expect(request).rejects.toThrow(/unknown value[\s\S]*after the Peasant API response was received[\s\S]*stopped[\s\S]*Regenerate/);
    });
  }

  for (const fixture of nullableArrayCases) {
    it(fixture.name, async () => {
      fetchMock.mockReturnValueOnce(okResponse(fixture.payload));
      let decoded: unknown;
      switch (fixture.operation) {
        case 'project-summaries': decoded = await fetchProjectSummaries(); break;
        case 'map-graph': decoded = await fetchMapGraph(PROJECT_HASH); break;
        case 'node-detail': decoded = await fetchMapNodeDetail(PROJECT_HASH, 'src'); break;
        case 'project-tasks': decoded = await fetchProjectTasks(PROJECT_HASH); break;
        case 'change-detail': decoded = await fetchChangeDetail(PROJECT_HASH, 'feature'); break;
        case 'change-diff': decoded = await fetchChangeDiff(PROJECT_HASH, 'feature', 'file.ts'); break;
        case 'search': decoded = await fetchSearch('query'); break;
      }
      expect(decoded).toEqual(fixture.expected);
    });
  }

  it('decodes the JSON payload of a successful response', async () => {
    fetchMock.mockReturnValueOnce(
      okResponse({ projectHash: PROJECT_HASH, repoFound: true, changes: [], recentCommits: [], sessions: [] }),
    );
    const payload = await fetchReviewChanges(PROJECT_HASH);
    expect(payload.projectHash).toBe(PROJECT_HASH);
    expect(payload.repoFound).toBe(true);
    expect(payload.changes).toEqual([]);
  });

  it('throws with status and body on a non-2xx response', async () => {
    fetchMock.mockReturnValueOnce(
      Promise.resolve({
        ok: false,
        status: 404,
        text: async () => '{"error":"unknown project"}',
      } as Response),
    );
    await expect(fetchMapGraph(PROJECT_HASH)).rejects.toThrow(
      /failed \(404\): unknown project/,
    );
  });

  it('preserves the typed saved-selection failure from a discovery response', async () => {
    fetchMock.mockReturnValueOnce(Promise.resolve({
      ok: false,
      status: 500,
      text: async () => JSON.stringify({
        error: 'project discovery failed while applying saved selection',
        code: 'selection_visibility',
      }),
    } as Response));

    const error = await fetchProjectSummaries().catch((caught) => caught);
    expect(error).toBeInstanceOf(DiscoveryRequestError);
    expect(error).toMatchObject({
      status: 500,
      code: DiscoveryErrorCode.SelectionVisibility,
      path: '/api/v1/projects/summary',
    });
    expect(error.message).toContain('applying saved selection');
  });

  it('does not misclassify an unrelated discovery failure', async () => {
    fetchMock.mockReturnValueOnce(Promise.resolve({
      ok: false,
      status: 503,
      text: async () => JSON.stringify({ error: 'database unavailable' }),
    } as Response));

    const error = await fetchProjectSummaries().catch((caught) => caught);
    expect(error).toBeInstanceOf(DiscoveryRequestError);
    expect(error).toMatchObject({ status: 503, code: undefined });
    expect(error.message).toContain('database unavailable');
  });
});
