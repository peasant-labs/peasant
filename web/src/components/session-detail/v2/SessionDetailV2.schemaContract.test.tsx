import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import type { ReactNode } from 'react';
import { Harness, StopReason, ToolCallKind, type SessionDetailPayload } from '@peasant-labs/schema';
import { parseTranscriptRouteQuery, type ProjectHash } from '@/lib/navigation/projectRoutes';
import { parseStrictYAML, requireExactRequiredFields, requireRecord, requireUniqueNames } from '@/test/strictYaml';
import { SessionDetailV2 } from './SessionDetailV2';

const PROJECT_HASH = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' as ProjectHash;
const casesSource = readFileSync(resolve(process.cwd(), 'src/components/session-detail/v2/testdata/schema_contract.yaml'), 'utf8');
const manifestSource = readFileSync(resolve(process.cwd(), 'src/components/session-detail/v2/testdata/schema_contract.manifest.yaml'), 'utf8');
const caseFields = ['name', 'family', 'harness', 'turnsMode', 'depth', 'stopReason', 'toolOptionalNumbersMode', 'durationMs', 'exitCode', 'scorecardMode', 'workingDirectoryMode', 'workingDirectory', 'legacyWorkingDirectory', 'filePath', 'expectedTurnCount', 'expectedFileNode', 'expectedScorecardState'] as const;
const mutationFields = ['name', 'find', 'replace', 'expectedTestFile', 'expectedFailedTestNames', 'expectedFailurePattern'] as const;
const loaderMutationFields = ['name', 'target', 'find', 'replace', 'expectedError'] as const;

type ContractCase = {
  name: string;
  family: string;
  harness: (typeof Harness)[keyof typeof Harness];
  turnsMode: 'null' | 'populated';
  depth: number;
  stopReason: (typeof StopReason)[keyof typeof StopReason];
  toolOptionalNumbersMode: 'absent' | 'populated';
  durationMs: number;
  exitCode: number;
  scorecardMode: 'absent' | 'null' | 'nullable-member';
  workingDirectoryMode: 'absent' | 'populated';
  workingDirectory: string;
  legacyWorkingDirectory: string;
  filePath: string;
  expectedTurnCount: number;
  expectedFileNode: string;
  expectedScorecardState: 'absent' | 'nullable-member';
};

type SourceMutation = { name: string; find: string; replace: string; expectedTestFile: string; expectedFailedTestNames: string[]; expectedFailurePattern: string };
type LoaderMutation = { name: string; target: 'manifest' | 'cases'; find: string; replace: string; expectedError: string };
type ContractFixtures = {
  cases: ContractCase[];
  loaderMutations: LoaderMutation[];
  mutations: SourceMutation[];
};

const captures = vi.hoisted(() => ({
  adapted: [] as Array<Record<string, unknown>>,
  analytics: [] as Array<{ turns: unknown[]; options: Record<string, unknown> | undefined }>,
  annotated: [] as unknown[][],
  medians: [] as unknown[][],
  graphs: [] as Array<Record<string, unknown>>,
}));

let channelData: unknown;

vi.mock('next/navigation', () => ({
  useSearchParams: () => new URLSearchParams(),
  usePathname: () => '/projects/alpha-project/sess-contract',
  useRouter: () => ({ replace: vi.fn() }),
}));

vi.mock('@/contexts/WebSocketContext', () => ({
  useChannel: (subscription: { topic: string }) => ({
    data: subscription.topic === 'session_detail' ? channelData : { sessions: null },
    connected: true,
    error: null,
  }),
}));

vi.mock('./lib/useEntryLabels', () => ({
  useEntryLabels: () => ({ entryTypes: [], labelsByEntry: new Map(), addLabel: vi.fn() }),
}));

vi.mock('@/hooks/useTheme', () => ({
  useTheme: () => ({ theme: 'dark', setTheme: vi.fn(), toggle: vi.fn() }),
}));

vi.mock('@/lib/ft-ui', () => ({
  Skeleton: ({ label }: { label?: string }) => <div aria-label={label} />,
  FeedbackPanel: ({ title, children }: { title?: ReactNode; children?: ReactNode }) => <div><p>{title}</p>{children}</div>,
}));

vi.mock('@peasant-labs/transcript-browser', () => ({
  TrajectoryGraph: (props: Record<string, unknown>) => {
    captures.graphs.push(props);
    return <div data-testid="trajectory-graph" />;
  },
  annotateTranscript: (turns: unknown[]) => {
    captures.annotated.push(turns);
    return [];
  },
  computePersonalMedians: (sessions: unknown[]) => {
    captures.medians.push(sessions);
    return undefined;
  },
  prefilterTurns: (turns: unknown[]) => turns,
  computeTasks: () => [],
  buildTaskWaterfall: () => [],
}));

vi.mock('@peasant-labs/fairtrade/ui', () => ({
    computeAnalytics: (turns: unknown[], options?: Record<string, unknown>) => {
      captures.analytics.push({ turns, options });
      return { scorecard: options?.scorecard };
    },
    adaptTranscript: (payload: Record<string, unknown>, _unused: unknown, analytics: unknown) => {
      captures.adapted.push({ payload, analytics });
      return {
        turns: payload.turns,
        meta: { provider: payload.harness },
      };
    },
    TranscriptViewer: (props: {
      viewModel: { turns: Array<Record<string, unknown>>; meta: { provider: string } };
      graphSlot?: () => ReactNode;
      renderTurnPanel?: (turn: Record<string, unknown>) => ReactNode;
      streamPrelude?: ReactNode;
    }) => (
      <div
        data-testid="contract-viewer"
        data-provider={props.viewModel.meta.provider}
        data-turn-count={props.viewModel.turns.length}
      >
        {props.streamPrelude}
        {props.graphSlot?.()}
        {props.viewModel.turns.map((turn) => (
          <div key={String(turn.index)}>{props.renderTurnPanel?.(turn)}</div>
        ))}
      </div>
    ),
}));

function replaceExactlyOnce(source: string, find: string, replacement: string, label: string): string {
  const count = source.split(find).length - 1;
  if (count !== 1) throw new Error(`${label} mutation anchor must occur exactly once, found ${count}`);
  return source.replace(find, replacement);
}

function stringInventory(value: unknown, path: string, fields: readonly string[], arrayFields: readonly string[] = []): Record<string, unknown>[] {
  if (!Array.isArray(value)) throw new Error(`${path} must be an array`);
  const rows = value.map((row, index) => requireRecord(row, `${path}[${index}]`));
  requireUniqueNames(rows, path);
  rows.forEach((row, index) => {
    requireExactRequiredFields(row, fields, `${path}[${index}]`);
    const stringFields = fields.filter((field) => !arrayFields.includes(field));
    if (stringFields.some((field) => typeof row[field] !== 'string' || (row[field] as string).length === 0)) throw new Error(`${path}[${index}] requires nonempty string fields`);
    for (const field of arrayFields) {
      const arrayValue = row[field];
      if (!Array.isArray(arrayValue) || arrayValue.length === 0 || arrayValue.some((entry) => typeof entry !== 'string' || entry.length === 0) || new Set(arrayValue).size !== arrayValue.length) {
        throw new Error(`${path}[${index}].${field} must be a non-empty array of unique nonempty strings`);
      }
    }
  });
  return rows;
}

function loadFixtures(manifestText = manifestSource, contractText = casesSource): ContractFixtures {
  const manifest = requireRecord(parseStrictYAML(manifestText, 'schema contract manifest'), 'schema contract manifest');
  requireExactRequiredFields(manifest, ['expectedCaseCount', 'requiredFamilies', 'requiredNames', 'expectedLoaderMutationCount', 'loaderMutations', 'expectedMutationCount', 'mutations'], 'schema contract manifest');
  if (!Number.isSafeInteger(manifest.expectedCaseCount) || !Array.isArray(manifest.requiredFamilies) || !Array.isArray(manifest.requiredNames) || !Number.isSafeInteger(manifest.expectedLoaderMutationCount) || !Number.isSafeInteger(manifest.expectedMutationCount)) throw new Error('schema contract manifest requires independent case and mutation counts');
  const requiredFamilies = manifest.requiredFamilies as unknown[];
  const requiredNames = manifest.requiredNames as unknown[];
  if ([...requiredFamilies, ...requiredNames].some((value) => typeof value !== 'string' || value.length === 0) || new Set(requiredFamilies).size !== requiredFamilies.length || new Set(requiredNames).size !== requiredNames.length) throw new Error('schema contract manifest names and families must be unique nonempty strings');
  const loaderMutations = stringInventory(manifest.loaderMutations, 'schema contract manifest.loaderMutations', loaderMutationFields) as LoaderMutation[];
  if (loaderMutations.some((mutation) => !['manifest', 'cases'].includes(mutation.target))) throw new Error('schema contract loader mutation target must be manifest or cases');
  const mutations = stringInventory(manifest.mutations, 'schema contract manifest.mutations', mutationFields, ['expectedFailedTestNames']) as unknown as SourceMutation[];
  if (loaderMutations.length !== manifest.expectedLoaderMutationCount || mutations.length !== manifest.expectedMutationCount) throw new Error('schema contract mutation counts do not match their independent manifest');

  const root = requireRecord(parseStrictYAML(contractText, 'schema contract cases'), 'schema contract cases');
  requireExactRequiredFields(root, ['cases'], 'schema contract cases');
  if (!Array.isArray(root.cases)) throw new Error('schema contract cases must be an array');
  const rows = root.cases.map((row, index) => requireRecord(row, `schema contract cases[${index}]`));
  requireUniqueNames(rows, 'schema contract cases');
  rows.forEach((row, index) => {
    requireExactRequiredFields(row, caseFields, `schema contract cases[${index}]`);
    for (const field of ['name', 'family', 'harness', 'turnsMode', 'stopReason', 'toolOptionalNumbersMode', 'scorecardMode', 'workingDirectoryMode', 'workingDirectory', 'legacyWorkingDirectory', 'filePath', 'expectedFileNode', 'expectedScorecardState']) {
      if (typeof row[field] !== 'string') throw new Error(`schema contract cases[${index}].${field} must be a string`);
    }
    if (!Object.values(Harness).includes(row.harness as ContractCase['harness']) || !Object.values(StopReason).includes(row.stopReason as ContractCase['stopReason'])) throw new Error(`schema contract cases[${index}] uses a noncanonical harness or stop reason`);
    if (!['null', 'populated'].includes(row.turnsMode as string) || !['absent', 'populated'].includes(row.toolOptionalNumbersMode as string) || !['absent', 'null', 'nullable-member'].includes(row.scorecardMode as string) || !['absent', 'populated'].includes(row.workingDirectoryMode as string) || !['absent', 'nullable-member'].includes(row.expectedScorecardState as string)) throw new Error(`schema contract cases[${index}] uses an invalid collection mode`);
    if (!Number.isSafeInteger(row.depth) || (row.depth as number) < 0 || !Number.isSafeInteger(row.durationMs) || !Number.isSafeInteger(row.exitCode) || !Number.isSafeInteger(row.expectedTurnCount) || (row.expectedTurnCount as number) < 0) throw new Error(`schema contract cases[${index}] requires safe integer numeric fields`);
    if ((row.turnsMode === 'null') !== (row.expectedTurnCount === 0) || (row.scorecardMode === 'nullable-member') !== (row.expectedScorecardState === 'nullable-member')) throw new Error(`schema contract cases[${index}] has inconsistent expectations`);
  });
  const names = rows.map((row) => row.name);
  const families = rows.map((row) => row.family);
  if (rows.length !== manifest.expectedCaseCount || requiredNames.length !== rows.length || requiredFamilies.length !== rows.length || requiredNames.some((name) => !names.includes(name)) || requiredFamilies.some((family) => !families.includes(family))) throw new Error('schema contract cases do not match their independent manifest');
  return { cases: rows as ContractCase[], loaderMutations, mutations };
}

function buildPayload(fixture: ContractCase): SessionDetailPayload & { gitContext: { workingDirectory: string } } {
  const turns = fixture.turnsMode === 'null' ? null : [{
    index: 0,
    role: 'assistant' as const,
    depth: fixture.depth,
    content: 'contract fixture response',
    timestamp: '2026-07-16T08:00:30Z',
    stopReason: fixture.stopReason,
    toolCalls: fixture.filePath ? [{
      id: 'tool-contract',
      name: 'Read',
      arguments: fixture.filePath,
      result: 'ok',
      filePath: fixture.filePath,
      toolKind: ToolCallKind.Read,
      ...(fixture.toolOptionalNumbersMode === 'populated'
        ? { durationMs: fixture.durationMs, exitCode: fixture.exitCode }
        : {}),
    }] : [],
  }];
  const scorecard = fixture.scorecardMode === 'null'
    ? null
    : fixture.scorecardMode === 'nullable-member'
      ? { m2TokenOutcomeRatio: null }
      : undefined;
  return {
    id: 'sess-contract',
    project: 'alpha-project',
    harness: fixture.harness,
    startTime: '2026-07-16T08:00:00Z',
    endTime: '2026-07-16T08:01:00Z',
    durationMins: 1,
    totalTokens: 12,
    tokensIn: 5,
    tokensOut: 7,
    turnCount: fixture.expectedTurnCount,
    toolCallCount: fixture.filePath ? 1 : 0,
    turns,
    scorecard,
    gitBranch: 'feature/schema-contract',
    gitRemote: 'https://example.invalid/flat-project.git',
    ...(fixture.workingDirectoryMode === 'populated' ? { workingDirectory: fixture.workingDirectory } : {}),
    gitContext: { workingDirectory: fixture.legacyWorkingDirectory },
  };
}

function TestDetail() {
  const routeQuery = parseTranscriptRouteQuery(new URLSearchParams());
  if (!routeQuery) throw new Error('schema contract route query must be valid');
  return <SessionDetailV2 sessionId="sess-contract" projectHash={PROJECT_HASH} projectName="alpha-project" routeQuery={routeQuery} />;
}

const fixtureSet = loadFixtures();

beforeEach(() => {
  captures.adapted.length = 0;
  captures.analytics.length = 0;
  captures.annotated.length = 0;
  captures.medians.length = 0;
  captures.graphs.length = 0;
  window.localStorage.clear();
});

afterEach(() => {
  cleanup();
  channelData = undefined;
});

describe('mounted canonical schema contract', () => {
  it('rejects every strict loader mutation from the independent manifest', () => {
    for (const mutation of fixtureSet.loaderMutations) {
      const original = mutation.target === 'manifest' ? manifestSource : casesSource;
      const mutated = replaceExactlyOnce(original, mutation.find, mutation.replace, mutation.name);
      expect(() => loadFixtures(mutation.target === 'manifest' ? mutated : manifestSource, mutation.target === 'cases' ? mutated : casesSource), mutation.name).toThrow(new RegExp(mutation.expectedError));
    }
  });

  for (const fixture of fixtureSet.cases) {
    it(fixture.name, () => {
      channelData = buildPayload(fixture);
      render(<TestDetail />);

      const viewer = screen.getByTestId('contract-viewer');
      expect(viewer).toHaveAttribute('data-provider', fixture.harness);
      expect(viewer).toHaveAttribute('data-turn-count', String(fixture.expectedTurnCount));
      expect(captures.medians.at(-1)).toEqual([]);
      expect(captures.annotated.at(-1)).toHaveLength(fixture.expectedTurnCount);
      expect(captures.graphs.at(-1)?.provider).toBe(fixture.harness);

      const adaptedPayload = captures.adapted.at(-1)?.payload as SessionDetailPayload;
      expect(adaptedPayload.turns).toHaveLength(fixture.expectedTurnCount);
      if (fixture.expectedTurnCount > 0) {
        expect(adaptedPayload.turns?.[0].depth).toBe(fixture.depth);
        expect(adaptedPayload.turns?.[0].stopReason).toBe(fixture.stopReason);
        const tool = adaptedPayload.turns?.[0].toolCalls?.[0];
        if (tool && fixture.toolOptionalNumbersMode === 'populated') {
          expect(tool.durationMs).toBe(fixture.durationMs);
          expect(tool.exitCode).toBe(fixture.exitCode);
        } else if (tool) {
          expect(tool).not.toHaveProperty('durationMs');
          expect(tool).not.toHaveProperty('exitCode');
        }
      }

      const analyticsScorecard = captures.analytics.at(-1)?.options?.scorecard as Record<string, unknown> | undefined;
      if (fixture.expectedScorecardState === 'nullable-member') expect(analyticsScorecard?.m2TokenOutcomeRatio).toBeNull();
      else expect(analyticsScorecard).toBeUndefined();

      if (fixture.expectedFileNode) {
        const link = screen.getByRole('link', { name: `Open ${fixture.expectedFileNode} on the Map` });
        expect(link).toHaveAttribute('href', `/map/${PROJECT_HASH}?node=${encodeURIComponent(fixture.expectedFileNode)}`);
        expect(screen.queryByText(fixture.legacyWorkingDirectory)).not.toBeInTheDocument();
      } else {
        expect(screen.queryByRole('link', { name: / on the Map$/ })).not.toBeInTheDocument();
      }
    });
  }
});
