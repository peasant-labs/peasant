import { cleanup, render } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import type { ReactNode } from 'react';
import { parseTranscriptRouteQuery, type ProjectHash } from '@/lib/navigation/projectRoutes';
import { parseStrictYAML, requireExactFields, requireExactRequiredFields, requireRecord, requireUniqueNames } from '@/test/strictYaml';
import { SessionDetailV2 } from './SessionDetailV2';

// computePersonalMedians is mocked as an args-ignoring stub in the other
// SessionDetailV2 test files, so the bridge
// this component owns (`outcome: session.outcome ?? ''`, feeding the
// adapter's optional outcome back into transcript-browser's required-string
// wire contract) had zero coverage — deleting it left all other suites green.
// This file captures the argument reaching computePersonalMedians and asserts
// its shape, so removing or breaking the bridge fails here.

const PROJECT_HASH = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' as ProjectHash;

type AssociationTargetAnnotation = {
  id: string;
  targetKind: 'association';
  targetAssociationId: string;
  isPrimary: boolean;
  annotatorKind: string;
  annotatorName: string;
  typeId: string;
  typeName: string;
  value: string;
  createdAt: number;
};
type BridgeCase = {
  name: string;
  outcome: string;
  expectedBridgedOutcome: string;
  effectiveAnnotations?: AssociationTargetAnnotation[];
};
type LoaderMutation = { name: string; target: 'manifest' | 'cases'; find: string; replace: string; expectedError: string };
type BridgeFixture = { cases: BridgeCase[]; loaderMutations: LoaderMutation[] };

const manifestSource = readFileSync(resolve(process.cwd(), 'src/components/session-detail/v2/testdata/quality_median_bridge.manifest.yaml'), 'utf8');
const casesSource = readFileSync(resolve(process.cwd(), 'src/components/session-detail/v2/testdata/quality_median_bridge.yaml'), 'utf8');

const bridgeCaseFields = ['name', 'outcome', 'expectedBridgedOutcome'] as const;
const associationTargetAnnotationFields = [
  'id',
  'targetKind',
  'targetAssociationId',
  'isPrimary',
  'annotatorKind',
  'annotatorName',
  'typeId',
  'typeName',
  'value',
  'createdAt',
] as const;

function replaceExactlyOnce(source: string, find: string, replacement: string, label: string): string {
  const count = source.split(find).length - 1;
  if (count !== 1) throw new Error(`${label} mutation anchor must occur exactly once, found ${count}`);
  return source.replace(find, replacement);
}

function loadAssociationTargetAnnotations(value: unknown, path: string): AssociationTargetAnnotation[] {
  if (!Array.isArray(value)) throw new Error(`${path} must be an array`);
  return value.map((item, index) => {
    const annotation = requireRecord(item, `${path}[${index}]`);
    requireExactRequiredFields(annotation, associationTargetAnnotationFields, `${path}[${index}]`);
    if (
      typeof annotation.id !== 'string'
      || annotation.targetKind !== 'association'
      || typeof annotation.targetAssociationId !== 'string'
      || typeof annotation.isPrimary !== 'boolean'
      || typeof annotation.annotatorKind !== 'string'
      || typeof annotation.annotatorName !== 'string'
      || typeof annotation.typeId !== 'string'
      || typeof annotation.typeName !== 'string'
      || typeof annotation.value !== 'string'
      || !Number.isSafeInteger(annotation.createdAt)
    ) {
      throw new Error(`${path}[${index}] must be a complete association-target annotation`);
    }
    return annotation as AssociationTargetAnnotation;
  });
}

function loadFixture(manifestText = manifestSource, casesText = casesSource): BridgeFixture {
  const manifest = requireRecord(parseStrictYAML(manifestText, 'quality median bridge manifest'), 'quality median bridge manifest');
  requireExactRequiredFields(manifest, ['expectedCaseCount', 'requiredNames', 'expectedLoaderMutationCount', 'loaderMutations', 'expectedMutationCount', 'mutations'], 'quality median bridge manifest');
  if (!Number.isSafeInteger(manifest.expectedCaseCount) || !Array.isArray(manifest.requiredNames) || !Number.isSafeInteger(manifest.expectedLoaderMutationCount) || !Array.isArray(manifest.loaderMutations) || !Number.isSafeInteger(manifest.expectedMutationCount) || !Array.isArray(manifest.mutations)) {
    throw new Error('quality median bridge manifest requires independent case, loader-mutation, and production-mutation counts');
  }
  const requiredNames = manifest.requiredNames as unknown[];
  if (requiredNames.some((value) => typeof value !== 'string' || value.length === 0) || new Set(requiredNames).size !== requiredNames.length) {
    throw new Error('quality median bridge manifest requiredNames must be unique nonempty strings');
  }
  if (manifest.loaderMutations.length !== manifest.expectedLoaderMutationCount || manifest.mutations.length !== manifest.expectedMutationCount) {
    throw new Error('quality median bridge manifest mutation counts do not match their independent inventories');
  }

  const root = requireRecord(parseStrictYAML(casesText, 'quality median bridge cases'), 'quality median bridge cases');
  requireExactRequiredFields(root, ['cases'], 'quality median bridge cases');
  if (!Array.isArray(root.cases)) throw new Error('quality median bridge cases must be an array');
  const rows = root.cases.map((row, index) => requireRecord(row, `quality median bridge cases[${index}]`));
  requireUniqueNames(rows, 'quality median bridge cases');
  rows.forEach((row, index) => {
    requireExactFields(row, [...bridgeCaseFields, 'effectiveAnnotations'], `quality median bridge cases[${index}]`);
    const missing = bridgeCaseFields.filter((field) => !(field in row));
    if (missing.length > 0) throw new Error(`quality median bridge cases[${index}] is missing required fields: ${missing.join(', ')}`);
    if (typeof row.name !== 'string' || typeof row.outcome !== 'string' || typeof row.expectedBridgedOutcome !== 'string') {
      throw new Error(`quality median bridge cases[${index}] requires string fields`);
    }
    if ('effectiveAnnotations' in row) {
      loadAssociationTargetAnnotations(row.effectiveAnnotations, `quality median bridge cases[${index}].effectiveAnnotations`);
    }
  });
  const names = rows.map((row) => row.name);
  if (rows.length !== manifest.expectedCaseCount || requiredNames.length !== names.length || requiredNames.some((name) => !names.includes(name))) {
    throw new Error('quality median bridge cases do not match their independent manifest');
  }
  return {
    cases: rows.map((row, index) => ({
      name: row.name as string,
      outcome: row.outcome as string,
      expectedBridgedOutcome: row.expectedBridgedOutcome as string,
      ...('effectiveAnnotations' in row
        ? { effectiveAnnotations: loadAssociationTargetAnnotations(row.effectiveAnnotations, `quality median bridge cases[${index}].effectiveAnnotations`) }
        : {}),
    })),
    loaderMutations: manifest.loaderMutations as LoaderMutation[],
  };
}

const fixture = loadFixture();

const captures = vi.hoisted(() => ({ medians: [] as unknown[][] }));

let sessionDetailData: unknown;
let qualityData: unknown;

vi.mock('next/navigation', () => ({
  useSearchParams: () => new URLSearchParams(),
  usePathname: () => '/projects/alpha-project/sess-median-bridge',
  useRouter: () => ({ replace: vi.fn() }),
}));

vi.mock('@/contexts/WebSocketContext', () => ({
  useChannel: (subscription: { topic: string }) => ({
    data: subscription.topic === 'session_detail' ? sessionDetailData : qualityData,
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

// The REAL capturing mock: unlike every other SessionDetailV2 test file,
// this one records the exact argument SessionDetailV2 hands to
// computePersonalMedians, so the bridge's output shape is actually asserted.
vi.mock('@peasant-labs/transcript-browser', () => ({
  TrajectoryGraph: () => null,
  annotateTranscript: () => [],
  prefilterTurns: (turns: unknown) => turns,
  computeTasks: () => [],
  buildTaskWaterfall: () => [],
  computePersonalMedians: (sessions: unknown[]) => {
    captures.medians.push(sessions);
    return undefined;
  },
}));

vi.mock('@peasant-labs/fairtrade/ui', () => ({
  computeAnalytics: () => ({}),
  adaptTranscript: (payload: Record<string, unknown>) => ({
    turns: payload.turns,
    meta: { provider: payload.harness },
  }),
  TranscriptViewer: (props: { viewModel: { turns: unknown[] } }) => (
    <div data-testid="median-bridge-viewer" data-turn-count={props.viewModel.turns.length} />
  ),
}));

const BASE_QUALITY_SESSION = {
  id: 'quality-session-median-bridge',
  project: 'alpha-project',
  title: 'quality median bridge fixture',
  date: '2026-07-16',
  durationMinutes: 1,
  totalTokens: 1,
  inputTokens: 1,
  outputTokens: 0,
  turnCount: 1,
  toolCalls: 0,
  filesTouched: 0,
  linesChanged: 0,
  retryLoops: 0,
  retryTokensWasted: 0,
  withinSessionReverts: 0,
  explorationRatio: 0,
  discoveryTurns: 0,
  scope: 'focused',
  scopeBreadth: 0,
  signalDensity: 0,
  specQualityScore: 0,
};

const DETAIL = {
  id: 'sess-median-bridge',
  project: 'alpha-project',
  turns: [{ index: 0, role: 'user', depth: 0, content: 'q0', toolCalls: [] }],
};

function TestDetail() {
  const routeQuery = parseTranscriptRouteQuery(new URLSearchParams());
  if (!routeQuery) throw new Error('quality median bridge route query must be valid');
  return <SessionDetailV2 sessionId="sess-median-bridge" projectHash={PROJECT_HASH} projectName="alpha-project" routeQuery={routeQuery} />;
}

beforeEach(() => {
  captures.medians.length = 0;
  sessionDetailData = DETAIL;
});

afterEach(() => {
  cleanup();
  sessionDetailData = undefined;
  qualityData = undefined;
});

describe('SessionDetailV2 — quality→computePersonalMedians outcome bridge', () => {
  it('rejects every strict loader mutation from the independent manifest', () => {
    for (const mutation of fixture.loaderMutations) {
      const original = mutation.target === 'manifest' ? manifestSource : casesSource;
      const mutated = replaceExactlyOnce(original, mutation.find, mutation.replace, mutation.name);
      expect(() => loadFixture(mutation.target === 'manifest' ? mutated : manifestSource, mutation.target === 'cases' ? mutated : casesSource), mutation.name).toThrow(new RegExp(mutation.expectedError));
    }
  });

  for (const row of fixture.cases) {
    it(row.name, () => {
      qualityData = {
        sessions: [{
          ...BASE_QUALITY_SESSION,
          outcome: row.outcome,
          ...(row.effectiveAnnotations === undefined ? {} : { effectiveAnnotations: row.effectiveAnnotations }),
        }],
      };
      render(<TestDetail />);

      const passed = captures.medians.at(-1) as Array<Record<string, unknown>> | undefined;
      expect(passed).toHaveLength(1);
      // computePersonalMedians must receive a defined string, never `undefined`,
      // for a session whose adapted outcome was optional or absent.
      expect(typeof passed?.[0]?.outcome).toBe('string');
      expect(passed?.[0]?.outcome).toBe(row.expectedBridgedOutcome);
      // Keep every scorecard input, but do not pass raw annotations whose current
      // association target arm is outside the older viewer's wire contract.
      if (row.effectiveAnnotations !== undefined) {
        expect(passed?.[0]).not.toHaveProperty('effectiveAnnotations');
      }
      expect(passed?.[0]).toEqual({ ...BASE_QUALITY_SESSION, outcome: row.expectedBridgedOutcome });
    });
  }
});
