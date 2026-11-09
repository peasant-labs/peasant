import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { ToolCallKind } from '@peasant-labs/schema';
import { parseTranscriptRouteQuery, type ProjectHash } from '@/lib/navigation/projectRoutes';
import { parseStrictYAML, requireExactRequiredFields, requireRecord, requireUniqueNames } from '@/test/strictYaml';
import { PublishProvider } from '@/contexts/PublishContext';
import { SessionDetailV2 } from './SessionDetailV2';

const PROJECT_HASH = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' as ProjectHash;
const PATHNAME = '/projects/alpha-project/sess-files-panel';
const caseFields = ['name', 'search', 'workingDirectory', 'turns', 'expectedTurns', 'expectedAbsentTurns', 'expectedToolPaths', 'expectedFilesTabPaths', 'expectedFileRowCount'] as const;
const turnFields = ['index', 'toolPath'] as const;
const mutationFields = ['name', 'find', 'replace', 'expectedTestFile', 'expectedFailedTestNames', 'expectedFailurePattern'] as const;

type FixtureTurn = { index: number; toolPath: string };
type FixtureCase = {
  name: string;
  search: string;
  workingDirectory: string;
  turns: FixtureTurn[];
  expectedTurns: number[];
  expectedAbsentTurns: number[];
  expectedToolPaths: string[];
  expectedFilesTabPaths: string[];
  expectedFileRowCount: number;
};
type ProductionMutation = {
  name: string;
  find: string;
  replace: string;
  expectedTestFile: string;
  expectedFailedTestNames: string[];
  expectedFailurePattern: string;
};
type FixtureManifest = {
  expectedCaseCount: number;
  requiredNames: string[];
  expectedMutationCount: number;
  mutations: ProductionMutation[];
};

const manifestSource = readFileSync(resolve(process.cwd(), 'src/components/session-detail/v2/testdata/turn_files_panel.manifest.yaml'), 'utf8');
const casesSource = readFileSync(resolve(process.cwd(), 'src/components/session-detail/v2/testdata/turn_files_panel.yaml'), 'utf8');

const routerReplace = vi.hoisted(() => vi.fn());
let currentSearchParams = new URLSearchParams();
let channelData: unknown;

vi.mock('@/lib/share/publishing', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/lib/share/publishing')>()),
  ...(await import('@/test/fixtures/publishingIdle')).PUBLISHING_IDLE,
}));
vi.mock('next/navigation', () => ({
  useSearchParams: () => currentSearchParams,
  usePathname: () => PATHNAME,
  useRouter: () => ({ replace: routerReplace }),
}));
vi.mock('@/contexts/WebSocketContext', () => ({ useChannel: () => ({ data: channelData, connected: true, error: null }) }));
vi.mock('./lib/useEntryLabels', () => ({ useEntryLabels: () => ({ entryTypes: [], labelsByEntry: new Map(), addLabel: vi.fn() }) }));
vi.mock('@/hooks/useTheme', () => ({ useTheme: () => ({ theme: 'dark', setTheme: vi.fn(), toggle: vi.fn() }) }));
vi.mock('@peasant-labs/fairtrade/ui', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@peasant-labs/fairtrade/ui')>();
  return {
    ...actual,
    annotateTranscript: () => [],
    computePersonalMedians: () => undefined,
  };
});
vi.mock('@peasant-labs/fairtrade/graph', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@peasant-labs/fairtrade/graph')>();
  return { ...actual, TrajectoryGraph: () => null };
});

function strictNonEmptyString(value: unknown, label: string): string {
  if (typeof value !== 'string' || value.length === 0) throw new Error(`${label} must be a nonempty string`);
  return value;
}

function strictIndexList(value: unknown, label: string): number[] {
  if (!Array.isArray(value) || value.some((index) => !Number.isSafeInteger(index) || (index as number) < 0) || new Set(value).size !== value.length) throw new Error(`${label} must be unique safe nonnegative integers`);
  return value as number[];
}

function strictPathList(value: unknown, label: string): string[] {
  if (!Array.isArray(value) || value.length === 0 || value.some((path) => typeof path !== 'string' || path.length === 0) || new Set(value).size !== value.length) throw new Error(`${label} must be unique nonempty paths`);
  return value as string[];
}

function loadFixtures(manifestText = manifestSource, casesText = casesSource): { cases: FixtureCase[]; manifest: FixtureManifest } {
  const manifest = requireRecord(parseStrictYAML(manifestText, 'turn files panel manifest'), 'turn files panel manifest');
  requireExactRequiredFields(manifest, ['expectedCaseCount', 'requiredNames', 'expectedMutationCount', 'mutations'], 'turn files panel manifest');
  if (!Number.isSafeInteger(manifest.expectedCaseCount) || !Array.isArray(manifest.requiredNames) || !Number.isSafeInteger(manifest.expectedMutationCount) || !Array.isArray(manifest.mutations)) throw new Error('turn files panel manifest requires case and mutation inventories');
  const requiredNames = manifest.requiredNames as unknown[];
  if (requiredNames.some((name) => typeof name !== 'string' || name.length === 0) || new Set(requiredNames).size !== requiredNames.length) throw new Error('turn files panel manifest requires unique nonempty names');
  const mutations = manifest.mutations.map((row, index) => {
    const mutation = requireRecord(row, `turn files panel manifest.mutations[${index}]`);
    requireExactRequiredFields(mutation, mutationFields, `turn files panel manifest.mutations[${index}]`);
    for (const field of ['name', 'find', 'replace', 'expectedTestFile', 'expectedFailurePattern']) strictNonEmptyString(mutation[field], `manifest.mutations[${index}].${field}`);
    if (!Array.isArray(mutation.expectedFailedTestNames) || mutation.expectedFailedTestNames.length === 0 || mutation.expectedFailedTestNames.some((name) => typeof name !== 'string' || name.length === 0)) throw new Error(`turn files panel mutation ${index} requires its failing test names`);
    return mutation as unknown as ProductionMutation;
  });
  if (mutations.length !== manifest.expectedMutationCount || new Set(mutations.map((mutation) => mutation.name)).size !== mutations.length) throw new Error('turn files panel manifest mutation inventory count or names are invalid');

  const root = requireRecord(parseStrictYAML(casesText, 'turn files panel cases'), 'turn files panel cases');
  requireExactRequiredFields(root, ['cases'], 'turn files panel cases');
  if (!Array.isArray(root.cases)) throw new Error('turn files panel cases must be an array');
  const rows = root.cases.map((row, index) => requireRecord(row, `turn files panel cases[${index}]`));
  requireUniqueNames(rows, 'turn files panel cases');
  rows.forEach((row, index) => {
    requireExactRequiredFields(row, caseFields, `turn files panel cases[${index}]`);
    strictNonEmptyString(row.name, `cases[${index}].name`);
    if (typeof row.search !== 'string' || typeof row.workingDirectory !== 'string') throw new Error(`turn files panel cases[${index}] requires string search and working directory fields`);
    if (!Array.isArray(row.turns) || row.turns.length === 0) throw new Error(`turn files panel cases[${index}].turns must be a nonempty array`);
    row.turns.forEach((entry, turnIndex) => {
      const turn = requireRecord(entry, `cases[${index}].turns[${turnIndex}]`);
      requireExactRequiredFields(turn, turnFields, `cases[${index}].turns[${turnIndex}]`);
      strictIndexList([turn.index], `cases[${index}].turns[${turnIndex}].index`);
      strictNonEmptyString(turn.toolPath, `cases[${index}].turns[${turnIndex}].toolPath`);
    });
    const expectedTurns = strictIndexList(row.expectedTurns, `cases[${index}].expectedTurns`);
    const absentTurns = strictIndexList(row.expectedAbsentTurns, `cases[${index}].expectedAbsentTurns`);
    if (new Set([...expectedTurns, ...absentTurns]).size !== expectedTurns.length + absentTurns.length) throw new Error(`turn files panel cases[${index}] classifies a turn twice`);
    strictPathList(row.expectedToolPaths, `cases[${index}].expectedToolPaths`);
    strictPathList(row.expectedFilesTabPaths, `cases[${index}].expectedFilesTabPaths`);
    if (!Number.isSafeInteger(row.expectedFileRowCount) || (row.expectedFileRowCount as number) !== (row.expectedFilesTabPaths as string[]).length) throw new Error(`turn files panel cases[${index}].expectedFileRowCount must equal its unique file paths`);
    if (!(row.expectedTurns as number[]).every((index) => (row.turns as FixtureTurn[]).some((turn) => turn.index === index)) || !(row.expectedAbsentTurns as number[]).every((index) => (row.turns as FixtureTurn[]).some((turn) => turn.index === index))) throw new Error(`turn files panel cases[${index}] expectations name a turn outside its fixture`);
  });
  const names = rows.map((row) => row.name as string);
  if (rows.length !== manifest.expectedCaseCount || requiredNames.length !== rows.length || requiredNames.some((name) => !names.includes(name as string)) || names.some((name) => !requiredNames.includes(name))) throw new Error('turn files panel cases do not match their independent manifest');
  return { cases: rows as unknown as FixtureCase[], manifest: manifest as unknown as FixtureManifest };
}

function TestDetail() {
  const routeQuery = parseTranscriptRouteQuery(currentSearchParams);
  if (!routeQuery) throw new Error('fixture route query must be valid');
  return (
    <PublishProvider>
      <SessionDetailV2 sessionId="sess-files-panel" projectHash={PROJECT_HASH} projectName="alpha-project" routeQuery={routeQuery} />
    </PublishProvider>
  );
}

beforeEach(() => {
  routerReplace.mockClear();
  currentSearchParams = new URLSearchParams();
  channelData = undefined;
  window.localStorage.clear();
});
afterEach(() => cleanup());

const fixtureSet = loadFixtures();

describe('mounted transcript without a per-turn files panel', () => {
  for (const fixture of fixtureSet.cases) {
    it(fixture.name, async () => {
      currentSearchParams = new URLSearchParams(fixture.search);
      channelData = {
        id: 'sess-files-panel', project: 'alpha-project', harness: 'codex',
        startTime: '2026-07-15T08:00:00Z', endTime: '2026-07-15T08:01:00Z', durationMins: 1,
        totalTokens: 12, tokensIn: 5, tokensOut: 7,
        turnCount: fixture.turns.length, toolCallCount: fixture.turns.length,
        workingDirectory: fixture.workingDirectory,
        turns: fixture.turns.map((turn) => ({
          index: turn.index,
          role: 'assistant' as const,
          depth: 0,
          content: '',
          timestamp: '2026-07-15T08:00:00Z',
          toolCalls: [{
            id: `tool-${turn.index}`,
            name: 'Read',
            arguments: JSON.stringify({ file_path: turn.toolPath }),
            result: 'ok',
            filePath: turn.toolPath,
            toolKind: ToolCallKind.Read,
          }],
        })),
      };

      const view = render(<TestDetail />);
      await waitFor(() => expect(view.container.querySelector('.txn-app')).toBeInTheDocument());

      // The removed per-turn panel would have rendered these under every turn,
      // including the wordless ones. Nothing may render them now.
      expect(screen.queryAllByText('files read')).toEqual([]);
      expect(screen.queryAllByText('files changed')).toEqual([]);
      expect(screen.queryAllByLabelText(/^Files touched in turn /)).toEqual([]);

      for (const index of fixture.expectedTurns) {
        expect(view.container.querySelector(`article[data-turn="${index}"]`)).toBeInTheDocument();
      }
      for (const index of fixture.expectedAbsentTurns) {
        expect(view.container.querySelector(`article[data-turn="${index}"]`)).not.toBeInTheDocument();
      }

      // The tool-call rows still carry each file's real path.
      for (const path of fixture.expectedToolPaths) {
        expect(screen.getAllByText(path).length).toBeGreaterThan(0);
      }

      // The Files tab is still the session-wide, deduplicated list of touched files.
      const sessionViews = screen.getByRole('tablist', { name: 'session views' });
      fireEvent.click(within(sessionViews).getByRole('tab', { name: /^files\b/ }));
      const table = await screen.findByRole('table');
      expect(within(table).getAllByRole('row')).toHaveLength(fixture.expectedFileRowCount + 1);
      for (const path of fixture.expectedFilesTabPaths) {
        expect(within(table).getByRole('button', { name: `${path}, jump to last read` })).toBeInTheDocument();
      }
    });
  }
});
