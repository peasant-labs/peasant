import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import type { Role, ToolCallDetail, TurnDetail } from '@/types/messages';
import { ToolCallKind } from '@/types/messages';
import { parseStrictYAML, requireExactRequiredFields, requireRecord, requireUniqueNames } from '@/test/strictYaml';
import { TranscriptScope, type ProjectHash } from '@/lib/navigation/projectRoutes';
import {
  clearScopeQuery,
  collectFileTouches,
  turnsToMarkdown,
  isEditTouch,
  isReadTouch,
  originCrumb,
  pathsMatch,
  prefilterTurns,
  relativizePath,
  scopeChipLabel,
  scopeTurns,
} from './scopeTurns';

// -- Shared fixtures ------------------------------------------------------------

const FILE_A = 'web/src/lib/api.ts';
const FILE_B = 'internal/ingest/pipeline.go';
const PROJECT_HASH = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' as ProjectHash;
const TS = '2026-06-09T12:00:00Z';

const scopeFixtureSource = readFileSync(
  resolve(process.cwd(), 'src/components/session-detail/v2/lib/testdata/scope_turns.yaml'),
  'utf8',
);
const scopeManifestSource = readFileSync(
  resolve(process.cwd(), 'src/components/session-detail/v2/lib/testdata/scope_turns_manifest.yaml'),
  'utf8',
);
const scopeProductionSource = readFileSync(
  resolve(process.cwd(), 'src/components/session-detail/v2/lib/scopeTurns.ts'),
  'utf8',
);

type ScopeFixture = {
  taskScopes: Array<{ name: string; scopeVal: string; wantIndexes: number[] }>;
  fileScopes: Array<{ name: string; scopeVal: string; wantIndexes: number[] }>;
  changeScopes: Array<{ name: string; scopeVal: string; wantIndexes: number[] }>;
  relativizePaths: Array<{ name: string; path: string; workingDirectory: string; want: string }>;
  pathMatches: Array<{ name: string; left: string; right: string; want: boolean }>;
  touchKinds: Array<{ name: string; id: string; toolName: string; filePath: string; toolKind: ToolCallKind; wantEdit: boolean; wantRead: boolean }>;
  originCrumbs: Array<{ name: string; params: Parameters<typeof originCrumb>[0]; want: ReturnType<typeof originCrumb> }>;
  scopeChipLabels: Array<{ name: string; scope: TranscriptScope; value: string; want: string }>;
};
type ScopeFixtureFamily = keyof ScopeFixture;

const scopeFixtureFields: Record<ScopeFixtureFamily, readonly string[]> = {
  taskScopes: ['name', 'scopeVal', 'wantIndexes'],
  fileScopes: ['name', 'scopeVal', 'wantIndexes'],
  changeScopes: ['name', 'scopeVal', 'wantIndexes'],
  relativizePaths: ['name', 'path', 'workingDirectory', 'want'],
  pathMatches: ['name', 'left', 'right', 'want'],
  touchKinds: ['name', 'id', 'toolName', 'filePath', 'toolKind', 'wantEdit', 'wantRead'],
  originCrumbs: ['name', 'params', 'want'],
  scopeChipLabels: ['name', 'scope', 'value', 'want'],
};

type ScopeManifest = {
  expectedFamilyCount: number;
  expectedMutationCount: number;
  mutations: Array<{ name: string; find: string; replace: string; expectedError: string }>;
  families: Array<{ name: ScopeFixtureFamily; requiredNames: string[] }>;
};

function loadScopeManifest(source: string): ScopeManifest {
  const value = requireRecord(parseStrictYAML(source, 'scope turns semantic manifest'), 'scope turns semantic manifest');
  requireExactRequiredFields(value, ['expectedFamilyCount', 'expectedMutationCount', 'mutations', 'families'], 'scope turns semantic manifest');
  if (!Number.isInteger(value.expectedMutationCount) || !Array.isArray(value.mutations) || !Array.isArray(value.families)) throw new Error('scope turns semantic manifest requires family and mutation inventories');
  const mutations = value.mutations.map((entry, index) => {
    const mutation = requireRecord(entry, `scope turns semantic manifest.mutations[${index}]`);
    requireExactRequiredFields(mutation, ['name', 'find', 'replace', 'expectedError'], `scope turns semantic manifest.mutations[${index}]`);
    if (['name', 'find', 'replace', 'expectedError'].some((field) => typeof mutation[field] !== 'string' || mutation[field].length === 0)) throw new Error(`scope turns semantic manifest mutation ${index} must use nonempty strings`);
    return mutation as ScopeManifest['mutations'][number];
  });
  if (mutations.length !== value.expectedMutationCount || new Set(mutations.map((mutation) => mutation.name)).size !== mutations.length) throw new Error('scope turns semantic manifest mutation inventory count or names are invalid');
  const families = value.families.map((entry, index) => {
    const family = requireRecord(entry, `scope turns semantic manifest.families[${index}]`);
    requireExactRequiredFields(family, ['name', 'requiredNames'], `scope turns semantic manifest.families[${index}]`);
    if (typeof family.name !== 'string' || !(family.name in scopeFixtureFields)) throw new Error(`scope turns semantic manifest has unknown family ${JSON.stringify(family.name)}`);
    if (!Array.isArray(family.requiredNames) || family.requiredNames.some((name) => typeof name !== 'string' || name.length === 0)) throw new Error(`scope turns semantic manifest family ${family.name} must have non-empty string requiredNames`);
    if (new Set(family.requiredNames).size !== family.requiredNames.length) throw new Error(`scope turns semantic manifest family ${family.name} repeats a required name`);
    return { name: family.name as ScopeFixtureFamily, requiredNames: family.requiredNames as string[] };
  });
  if (value.expectedFamilyCount !== families.length || families.length !== Object.keys(scopeFixtureFields).length) throw new Error(`scope turns semantic manifest has ${families.length} families, want exactly ${Object.keys(scopeFixtureFields).length}`);
  if (new Set(families.map((family) => family.name)).size !== families.length) throw new Error('scope turns semantic manifest repeats a family name');
  for (const family of Object.keys(scopeFixtureFields) as ScopeFixtureFamily[]) {
    if (!families.some((entry) => entry.name === family)) throw new Error(`scope turns semantic manifest is missing family ${family}`);
  }
  return { expectedFamilyCount: value.expectedFamilyCount as number, expectedMutationCount: value.expectedMutationCount as number, mutations, families };
}

const scopeManifest = loadScopeManifest(scopeManifestSource);

function loadScopeFixture(source: string, manifest: ScopeManifest = scopeManifest): ScopeFixture {
  const scopeFixtureFamilies = manifest.families.map((family) => family.name);
  const requiredScopeBehaviorNames = Object.fromEntries(
    manifest.families.map((family) => [family.name, family.requiredNames]),
  ) as unknown as Record<ScopeFixtureFamily, readonly string[]>;
  const value = requireRecord(parseStrictYAML(source, 'scope turns fixture'), 'scope turns fixture');
  requireExactRequiredFields(value, ['expectedCounts', ...scopeFixtureFamilies], 'scope turns fixture');
  const declaredCounts = requireRecord(value.expectedCounts, 'scope turns fixture.expectedCounts');
  requireExactRequiredFields(declaredCounts, scopeFixtureFamilies, 'scope turns fixture.expectedCounts');

  for (const family of scopeFixtureFamilies) {
    const values = value[family];
    if (!Array.isArray(values)) {
      throw new Error(`scope turns fixture.${family} must be an array`);
    }
    const rows = values.map((row, index) => requireRecord(row, `scope turns fixture.${family}[${index}]`));
    requireUniqueNames(rows, `scope turns fixture.${family}`);
    rows.forEach((row, index) => {
      requireExactRequiredFields(row, scopeFixtureFields[family], `scope turns fixture.${family}[${index}]`);
    });

    const requiredNames = requiredScopeBehaviorNames[family];
    if (declaredCounts[family] !== requiredNames.length) {
      throw new Error(`scope turns fixture.expectedCounts.${family} is ${String(declaredCounts[family])}, want independently defined ${requiredNames.length}`);
    }
    if (rows.length !== requiredNames.length) {
      throw new Error(`scope turns fixture.${family} has ${rows.length} rows, want independently defined ${requiredNames.length}`);
    }
    const actualNames = new Set(rows.map((row) => row.name));
    for (const name of requiredNames) {
      if (!actualNames.has(name)) {
        throw new Error(`scope turns fixture.${family} is missing required behavior ${JSON.stringify(name)}`);
      }
    }
  }

  for (const [index, row] of (value.originCrumbs as unknown[]).entries()) {
    const parsedRow = requireRecord(row, `scope turns fixture.originCrumbs[${index}]`);
    requireExactRequiredFields(
      requireRecord(parsedRow.params, `scope turns fixture.originCrumbs[${index}].params`),
      ['scope', 'scopeVal', 'origin', 'originNode', 'originBranch', 'returnTo'],
      `scope turns fixture.originCrumbs[${index}].params`,
    );
  }

  return value as unknown as ScopeFixture;
}

const scopeFixture = loadScopeFixture(scopeFixtureSource);

function verifyScopeProduction(source: string): void {
  if (/\bparseScopeParams\b/.test(source)) throw new Error('second scope parser');
  if (/\b(?:type|interface)\s+(?:ScopeParams|ScopeOrigin)\b/.test(source)) throw new Error('duplicate scope type');
  if (!source.includes('type TranscriptRouteQuery') || !source.includes('TranscriptScope') || !source.includes('RouteOrigin')) throw new Error('central scope types');
}

verifyScopeProduction(scopeProductionSource);
for (const mutation of scopeManifest.mutations) {
  if (!scopeProductionSource.includes(mutation.find)) throw new Error(`${mutation.name}: production mutation target is stale`);
  let failure: unknown;
  try { verifyScopeProduction(scopeProductionSource.replace(mutation.find, mutation.replace)); } catch (error) { failure = error; }
  if (!(failure instanceof Error) || !new RegExp(mutation.expectedError).test(failure.message)) throw new Error(`${mutation.name}: production mutant survived or failed for the wrong reason`);
}

describe('scope fixture contract', () => {
  it('rejects unknown manifest fields and additional YAML documents', () => {
    expect(() => loadScopeManifest(scopeManifestSource.replace('expectedFamilyCount:', 'unexpected: true\nexpectedFamilyCount:'))).toThrow(/fields/i);
    expect(() => loadScopeManifest(`${scopeManifestSource}\n---\n{}`)).toThrow();
  });
});

function turn(
  over: Partial<TurnDetail> & { index: number; role: Role },
): TurnDetail {
  return { content: `turn ${over.index}`, depth: 0, timestamp: TS, ...over };
}

function tool(over: Partial<ToolCallDetail> & { id: string }): ToolCallDetail {
  return { name: 'Edit', arguments: '{}', result: '', ...over };
}

const edit = (id: string, filePath?: string) =>
  tool({ id, name: 'Edit', filePath, toolKind: ToolCallKind.Edit });
const read = (id: string, filePath?: string) =>
  tool({ id, name: 'Read', filePath, toolKind: ToolCallKind.Read });

// Task-scope fixture: two depth-0 tasks with depth-1 turns inside the first.
// Depth-1 turns - including the depth-1 "user"-role turn - are NOT task
// boundaries: in wire data they are decomposed content blocks of the
// enclosing exchange (user-message content blocks, AskUserQuestion results),
// not new human prompts. (Subagent runs are separate sessions, linked via
// parentSessionId; they do not appear as depth-1 turns of this session.)
const taskTurns: TurnDetail[] = [
  turn({ index: 0, role: 'user' }),
  turn({ index: 1, role: 'assistant' }),
  turn({ index: 2, role: 'assistant', depth: 1, agentName: 'researcher' }),
  turn({ index: 3, role: 'user', depth: 1 }),
  turn({ index: 4, role: 'user' }),
  turn({ index: 5, role: 'assistant' }),
];

// File-scope fixture: edits + reads on FILE_A, an edit on FILE_B, a pathless
// execute call, and a tool-less user turn.
const fileTurns: TurnDetail[] = [
  turn({ index: 0, role: 'user' }),
  turn({ index: 1, role: 'assistant', toolCalls: [edit('e1', FILE_A)] }),
  turn({ index: 2, role: 'assistant', toolCalls: [read('r1', FILE_A)] }),
  turn({ index: 3, role: 'assistant', toolCalls: [edit('e2', FILE_B)] }),
  turn({
    index: 4,
    role: 'assistant',
    toolCalls: [tool({ id: 'x1', name: 'Bash', toolKind: ToolCallKind.Execute })],
  }),
];

// -- scopeTurns: task ------------------------------------------------------------

describe('scopeTurns — task', () => {
  it.each(scopeFixture.taskScopes)('$name', ({ scopeVal, wantIndexes }) => {
    const got = scopeTurns(taskTurns, TranscriptScope.Task, scopeVal);
    expect(got.map((t) => t.index)).toEqual(wantIndexes);
  });
});

// -- scopeTurns: file ------------------------------------------------------------

describe('scopeTurns — file', () => {
  it.each(scopeFixture.fileScopes)('$name', ({ scopeVal, wantIndexes }) => {
    const got = scopeTurns(fileTurns, TranscriptScope.File, scopeVal);
    expect(got.map((t) => t.index)).toEqual(wantIndexes);
  });
});

// -- turnsToMarkdown -----------------------------------------------------------

describe('turnsToMarkdown', () => {
  it('renders role headings, text, and a compact tool list', () => {
    const turns = [
      { index: 0, role: 'user', content: 'fix the bug', timestamp: '' },
      {
        index: 1,
        role: 'assistant',
        content: 'done',
        timestamp: '',
        toolCalls: [{ id: 't', name: 'Edit', arguments: '', result: '', filePath: 'a.go' }],
      },
    ] as TurnDetail[];
    const md = turnsToMarkdown(turns);
    expect(md).toContain('## You');
    expect(md).toContain('fix the bug');
    expect(md).toContain('## Assistant');
    expect(md).toContain('- tool: Edit · a.go');
    expect(md).not.toMatch(/🔧|📄/); // plain text, no emoji icons
  });
});

// -- scopeTurns: passthrough -------------------------------------------------------

describe('scopeTurns — passthrough', () => {
  it('no scope returns the same turn list unchanged', () => {
    expect(scopeTurns(fileTurns, null, '')).toBe(fileTurns);
  });
});

// -- scopeTurns: change (union of the named task slices) --------------------------

describe('scopeTurns — change', () => {
  it.each(scopeFixture.changeScopes)('$name', ({ scopeVal, wantIndexes }) => {
    const got = scopeTurns(taskTurns, TranscriptScope.Change, scopeVal);
    expect(got.map((t) => t.index)).toEqual(wantIndexes);
  });
});

// -- Path reconciliation (absolute wire paths vs repo-relative Map node ids) ------

const ABS_WD = '/Users/dev/proj';

// Wire-realistic turns: Claude tool calls carry absolute paths.
const absTurns: TurnDetail[] = [
  turn({ index: 0, role: 'assistant', toolCalls: [edit('e1', `${ABS_WD}/${FILE_A}`)] }),
  turn({ index: 1, role: 'assistant', toolCalls: [read('r1', `${ABS_WD}/${FILE_B}`)] }),
];

describe('relativizePath', () => {
  it.each(scopeFixture.relativizePaths)('$name', ({ path, workingDirectory, want }) => {
    expect(relativizePath(path, workingDirectory || undefined)).toBe(want);
  });
});

describe('pathsMatch', () => {
  it.each(scopeFixture.pathMatches)('$name', ({ left, right, want }) => {
    expect(pathsMatch(left, right)).toBe(want);
  });
});

describe('scopeTurns — file across the absolute/relative split', () => {
  it('repo-relative scope value (Map node id) matches absolute tool-call paths', () => {
    const got = scopeTurns(absTurns, TranscriptScope.File, FILE_A);
    expect(got.map((t) => t.index)).toEqual([0]);
  });

  it('absolute scope value matches repo-relative tool-call paths', () => {
    const got = scopeTurns(fileTurns, TranscriptScope.File, `${ABS_WD}/${FILE_A}`);
    expect(got.map((t) => t.index)).toEqual([1, 2]);
  });
});

// -- prefilterTurns (mirror of the package prefilter) -------------------------------

describe('prefilterTurns', () => {
  it('drops turns with no content and no tool calls', () => {
    const turns = [turn({ index: 0, role: 'user' }), turn({ index: 1, role: 'assistant', content: '  ' })];
    expect(prefilterTurns(turns).map((t) => t.index)).toEqual([0]);
  });

  it('drops short tool-less system turns, keeps tool-bearing ones', () => {
    const turns = [
      turn({ index: 0, role: 'system', content: 'ok' }),
      turn({ index: 1, role: 'system', content: 'ok', toolCalls: [edit('e1', FILE_A)] }),
    ];
    expect(prefilterTurns(turns).map((t) => t.index)).toEqual([1]);
  });

  it('dedups consecutive same-role same-content turns, preferring the tool-bearing one', () => {
    const turns = [
      turn({ index: 0, role: 'assistant', content: 'same' }),
      turn({ index: 1, role: 'assistant', content: 'same', toolCalls: [edit('e1', FILE_A)] }),
      turn({ index: 2, role: 'user', content: 'next' }),
    ];
    expect(prefilterTurns(turns).map((t) => t.index)).toEqual([1, 2]);
  });
});

// -- Touch classification ------------------------------------------------------------

describe('isEditTouch / isReadTouch', () => {
  it.each(scopeFixture.touchKinds)('$name', ({ id, toolName, filePath, toolKind, wantEdit, wantRead }) => {
    const tc = tool({ id, name: toolName, filePath: filePath || undefined, toolKind });
    expect(isEditTouch(tc)).toBe(wantEdit);
    expect(isReadTouch(tc)).toBe(wantRead);
  });
});

// -- collectFileTouches ------------------------------------------------------------

describe('collectFileTouches', () => {
  it('groups edits vs reads per turn, keyed by entry index', () => {
    const got = collectFileTouches(fileTurns);
    expect(got).toEqual([
      { turnIndex: 1, edits: [FILE_A], reads: [] },
      { turnIndex: 2, edits: [], reads: [FILE_A] },
      { turnIndex: 3, edits: [FILE_B], reads: [] },
    ]);
  });

  it('dedupes within a group and lists edited+read files under edits only', () => {
    const turns = [
      turn({
        index: 7,
        role: 'assistant',
        toolCalls: [read('r1', FILE_A), edit('e1', FILE_A), edit('e2', FILE_A), read('r2', FILE_B)],
      }),
    ];
    expect(collectFileTouches(turns)).toEqual([
      { turnIndex: 7, edits: [FILE_A], reads: [FILE_B] },
    ]);
  });

  it('omits turns with no file-bearing tool calls', () => {
    const turns = [
      turn({ index: 0, role: 'user' }),
      turn({
        index: 1,
        role: 'assistant',
        toolCalls: [tool({ id: 'x', name: 'Bash', toolKind: ToolCallKind.Execute })],
      }),
    ];
    expect(collectFileTouches(turns)).toEqual([]);
  });

  it('relativizes absolute wire paths to Map node ids via the working directory', () => {
    expect(collectFileTouches(absTurns, ABS_WD)).toEqual([
      { turnIndex: 0, edits: [FILE_A], reads: [] },
      { turnIndex: 1, edits: [], reads: [FILE_B] },
    ]);
  });

  it('dedupes absolute and relative touches of the same file after relativization', () => {
    const turns = [
      turn({
        index: 3,
        role: 'assistant',
        toolCalls: [edit('e1', `${ABS_WD}/${FILE_A}`), read('r1', FILE_A)],
      }),
    ];
    expect(collectFileTouches(turns, ABS_WD)).toEqual([
      { turnIndex: 3, edits: [FILE_A], reads: [] },
    ]);
  });
});

// -- originCrumb ------------------------------------------------------------------

describe('originCrumb', () => {
  it.each(scopeFixture.originCrumbs)('$name', ({ params, want }) => {
    expect(originCrumb(params, PROJECT_HASH)).toEqual(want);
  });
});

// -- clearScopeQuery -----------------------------------------------------------------

describe('clearScopeQuery', () => {
  it('removes scope params but keeps origin and unrelated params', () => {
    const params = new URLSearchParams(
      'scope=task&scopeVal=3&origin=Map&originNode=internal%2Fingest&turn=2',
    );
    expect(clearScopeQuery(params)).toBe('?origin=Map&originNode=internal%2Fingest&turn=2');
  });

  it('returns an empty string when nothing else remains', () => {
    expect(clearScopeQuery(new URLSearchParams('scope=file&scopeVal=a.ts'))).toBe('');
  });
});

// -- scopeChipLabel -----------------------------------------------------------------

describe('scopeChipLabel', () => {
  it.each(scopeFixture.scopeChipLabels)('$name', ({ scope, value, want }) => {
    expect(scopeChipLabel(scope, value)).toBe(want);
  });
});
