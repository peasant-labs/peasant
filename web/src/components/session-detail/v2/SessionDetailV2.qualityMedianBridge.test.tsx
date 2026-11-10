import { cleanup, render } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import type { ReactNode } from 'react';
import { parseTranscriptRouteQuery, type ProjectHash } from '@/lib/navigation/projectRoutes';
import { parseStrictYAML, requireExactFields, requireExactRequiredFields, requireRecord, requireUniqueNames } from '@/test/strictYaml';
import { PublishProvider } from '@/contexts/PublishContext';
import { SessionDetailV2 } from './SessionDetailV2';

// The transcript reads its display metadata from the local
// SessionDetailReadPayload. The full quality topic is retired for this surface,
// so a reader that only ever receives one session_detail frame must still get
// the stored title, the personal medians, and the revision. These cases mount
// the real component and capture the exact arguments it hands to
// computeAnalytics and adaptTranscript, and the view model it hands to the
// viewer.

const PROJECT_HASH = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' as ProjectHash;
const SESSION_ID = 'sess-read-payload-metadata';
const STORED_TITLE = 'refactor the ingest pipeline';

type Medians = { retryShare?: number; specQualityScore?: number; withinSessionReverts?: number };
type MetadataCase = {
  name: string;
  revision: string;
  medians: Medians | null;
  expectedMedians: Medians | null;
  nextRevision?: string;
  nextMedians?: Medians;
  expectedNextMedians?: Medians;
};
type LoaderMutation = { name: string; target: 'manifest' | 'cases'; find: string; replace: string; expectedError: string };
type MetadataFixture = { cases: MetadataCase[]; loaderMutations: LoaderMutation[] };

const manifestSource = readFileSync(resolve(process.cwd(), 'src/components/session-detail/v2/testdata/quality_median_bridge.manifest.yaml'), 'utf8');
const casesSource = readFileSync(resolve(process.cwd(), 'src/components/session-detail/v2/testdata/quality_median_bridge.yaml'), 'utf8');

const caseFields = ['name', 'revision', 'medians', 'expectedMedians', 'nextRevision', 'nextMedians', 'expectedNextMedians'] as const;
const medianFields = ['retryShare', 'specQualityScore', 'withinSessionReverts'] as const;

function replaceExactlyOnce(source: string, find: string, replacement: string, label: string): string {
  const count = source.split(find).length - 1;
  if (count !== 1) throw new Error(`${label} mutation anchor must occur exactly once, found ${count}`);
  return source.replace(find, replacement);
}

function requireMedians(value: unknown, path: string): Medians | null {
  if (value === null) return null;
  const record = requireRecord(value, path);
  requireExactFields(record, [...medianFields], path);
  const medians: Medians = {};
  for (const field of medianFields) {
    if (!(field in record)) continue;
    if (typeof record[field] !== 'number' || !Number.isFinite(record[field])) {
      throw new Error(`${path}.${field} must be a finite number`);
    }
    medians[field] = record[field] as number;
  }
  return medians;
}

function loadFixture(manifestText = manifestSource, casesText = casesSource): MetadataFixture {
  const manifest = requireRecord(parseStrictYAML(manifestText, 'read payload metadata manifest'), 'read payload metadata manifest');
  requireExactRequiredFields(manifest, ['expectedCaseCount', 'requiredNames', 'expectedLoaderMutationCount', 'loaderMutations', 'expectedMutationCount', 'mutations'], 'read payload metadata manifest');
  if (!Number.isSafeInteger(manifest.expectedCaseCount) || !Array.isArray(manifest.requiredNames) || !Number.isSafeInteger(manifest.expectedLoaderMutationCount) || !Array.isArray(manifest.loaderMutations) || !Number.isSafeInteger(manifest.expectedMutationCount) || !Array.isArray(manifest.mutations)) {
    throw new Error('read payload metadata manifest requires independent case, loader-mutation, and production-mutation counts');
  }
  const requiredNames = manifest.requiredNames as unknown[];
  if (requiredNames.some((value) => typeof value !== 'string' || value.length === 0) || new Set(requiredNames).size !== requiredNames.length) {
    throw new Error('read payload metadata manifest requiredNames must be unique nonempty strings');
  }
  if (manifest.loaderMutations.length !== manifest.expectedLoaderMutationCount || manifest.mutations.length !== manifest.expectedMutationCount) {
    throw new Error('read payload metadata manifest mutation counts do not match their independent inventories');
  }

  const root = requireRecord(parseStrictYAML(casesText, 'read payload metadata cases'), 'read payload metadata cases');
  requireExactRequiredFields(root, ['cases'], 'read payload metadata cases');
  if (!Array.isArray(root.cases)) throw new Error('read payload metadata cases must be an array');
  const rows = root.cases.map((row, index) => requireRecord(row, `read payload metadata cases[${index}]`));
  requireUniqueNames(rows, 'read payload metadata cases');
  rows.forEach((row, index) => {
    requireExactFields(row, [...caseFields], `read payload metadata cases[${index}]`);
    const required = ['name', 'revision', 'medians', 'expectedMedians'] as const;
    const missing = required.filter((field) => !(field in row));
    if (missing.length > 0) throw new Error(`read payload metadata cases[${index}] is missing required fields: ${missing.join(', ')}`);
    if (typeof row.name !== 'string' || typeof row.revision !== 'string') {
      throw new Error(`read payload metadata cases[${index}] requires string name and revision`);
    }
    requireMedians(row.medians, `read payload metadata cases[${index}].medians`);
    requireMedians(row.expectedMedians, `read payload metadata cases[${index}].expectedMedians`);
    if ('nextRevision' in row && typeof row.nextRevision !== 'string') {
      throw new Error(`read payload metadata cases[${index}].nextRevision must be a string`);
    }
    if ('nextMedians' in row) requireMedians(row.nextMedians, `read payload metadata cases[${index}].nextMedians`);
    if ('expectedNextMedians' in row) requireMedians(row.expectedNextMedians, `read payload metadata cases[${index}].expectedNextMedians`);
    if ('nextMedians' in row && !('expectedNextMedians' in row)) {
      throw new Error(`read payload metadata cases[${index}] carries nextMedians without an expectation`);
    }
  });
  const names = rows.map((row) => row.name);
  if (rows.length !== manifest.expectedCaseCount || requiredNames.length !== names.length || requiredNames.some((name) => !names.includes(name))) {
    throw new Error('read payload metadata cases do not match their independent manifest');
  }
  return {
    cases: rows.map((row) => ({
      name: row.name as string,
      revision: row.revision as string,
      medians: requireMedians(row.medians, 'medians'),
      expectedMedians: requireMedians(row.expectedMedians, 'expectedMedians'),
      ...('nextRevision' in row ? { nextRevision: row.nextRevision as string } : {}),
      ...('nextMedians' in row ? { nextMedians: requireMedians(row.nextMedians, 'nextMedians') as Medians } : {}),
      ...('expectedNextMedians' in row ? { expectedNextMedians: requireMedians(row.expectedNextMedians, 'expectedNextMedians') as Medians } : {}),
    })),
    loaderMutations: manifest.loaderMutations as LoaderMutation[],
  };
}

const fixture = loadFixture();

const captures = vi.hoisted(() => ({
  topics: [] as string[],
  analytics: [] as unknown[],
  payloads: [] as Array<Record<string, unknown>>,
  viewModels: [] as Array<{ session?: { title?: string } }>,
}));

let sessionDetailData: Record<string, unknown> | undefined;

vi.mock('next/navigation', () => ({
  useSearchParams: () => new URLSearchParams(),
  usePathname: () => `/projects/alpha-project/${SESSION_ID}`,
  useRouter: () => ({ replace: vi.fn() }),
}));

vi.mock('@/contexts/WebSocketContext', () => ({
  useChannel: (subscription: { topic: string }) => {
    captures.topics.push(subscription.topic);
    return { data: sessionDetailData, connected: true, error: null };
  },
}));

vi.mock('./lib/useEntryLabels', () => ({
  useEntryLabels: () => ({ entryTypes: [], labelsByEntry: new Map(), addLabel: vi.fn() }),
}));

vi.mock('@/hooks/useTheme', () => ({
  useTheme: () => ({ theme: 'dark', setTheme: vi.fn(), toggle: vi.fn() }),
}));

vi.mock('./publish/useTranscriptPublish', () => ({
  useTranscriptPublish: () => ({ bar: null, dialog: null }),
}));

vi.mock('@/lib/ft-ui', () => ({
  Skeleton: ({ label }: { label?: string }) => <div aria-label={label} />,
  FeedbackPanel: ({ title, children }: { title?: ReactNode; children?: ReactNode }) => <div><p>{title}</p>{children}</div>,
}));

vi.mock('@peasant-labs/fairtrade/graph', () => ({
  TrajectoryGraph: () => null,
}));

vi.mock('@peasant-labs/fairtrade/ui', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@peasant-labs/fairtrade/ui')>()),
  computeAnalytics: (_turns: unknown, opts: unknown) => {
    captures.analytics.push(opts);
    return {};
  },
  adaptTranscript: (payload: Record<string, unknown>) => {
    captures.payloads.push(payload);
    return { turns: payload.turns, meta: { provider: payload.harness } };
  },
  TranscriptViewer: (props: { viewModel: { session?: { title?: string } } }) => {
    captures.viewModels.push(props.viewModel);
    return <div data-testid="read-payload-metadata-viewer" />;
  },
  annotateTranscript: () => [],
  prefilterTurns: (turns: unknown) => turns,
  computeTasks: () => [],
  buildTaskWaterfall: () => [],
}));

const DETAIL = {
  id: SESSION_ID,
  project: 'alpha-project',
  harness: 'claude-code',
  startTime: '2026-07-16T08:00:00Z',
  endTime: '2026-07-16T08:01:00Z',
  durationMins: 1,
  totalTokens: 12,
  tokensIn: 5,
  tokensOut: 7,
  turnCount: 1,
  toolCallCount: 0,
  title: STORED_TITLE,
  turns: [{ index: 0, role: 'user', depth: 0, content: 'q0', timestamp: '2026-07-16T08:00:00Z', toolCalls: [] }],
};

function TestDetail() {
  const routeQuery = parseTranscriptRouteQuery(new URLSearchParams());
  if (!routeQuery) throw new Error('read payload metadata route query must be valid');
  return (
    <PublishProvider>
      <SessionDetailV2 sessionId={SESSION_ID} projectHash={PROJECT_HASH} projectName="alpha-project" routeQuery={routeQuery} />
    </PublishProvider>
  );
}

function readDetail(revision: string, medians: Medians | null): Record<string, unknown> {
  return { ...DETAIL, revision, ...(medians === null ? {} : { medians }) };
}

beforeEach(() => {
  captures.topics.length = 0;
  captures.analytics.length = 0;
  captures.payloads.length = 0;
  captures.viewModels.length = 0;
});

afterEach(() => {
  cleanup();
  sessionDetailData = undefined;
});

describe('SessionDetailV2 — read payload metadata', () => {
  it('rejects every strict loader mutation from the independent manifest', () => {
    for (const mutation of fixture.loaderMutations) {
      const original = mutation.target === 'manifest' ? manifestSource : casesSource;
      const mutated = replaceExactlyOnce(original, mutation.find, mutation.replace, mutation.name);
      expect(() => loadFixture(mutation.target === 'manifest' ? mutated : manifestSource, mutation.target === 'cases' ? mutated : casesSource), mutation.name).toThrow(new RegExp(mutation.expectedError));
    }
  });

  it('never subscribes to the retired full quality topic', () => {
    sessionDetailData = readDetail('7', { retryShare: 0.03 });
    render(<TestDetail />);
    expect(captures.topics).not.toContain('quality');
    expect(captures.topics).toContain('session_detail');
  });

  for (const row of fixture.cases) {
    it(row.name, () => {
      sessionDetailData = readDetail(row.revision, row.medians);
      const view = render(<TestDetail />);

      const analytics = captures.analytics.at(-1) as { medians?: Medians } | undefined;
      expect(analytics?.medians).toEqual(row.expectedMedians ?? undefined);
      // The stored title is the hero heading, and the read-only metadata never
      // reaches the durable push payload.
      expect(captures.viewModels.at(-1)?.session?.title).toBe(STORED_TITLE);
      const durable = captures.payloads.at(-1);
      expect(durable).toBeDefined();
      for (const field of ['title', 'medians', 'revision', 'relationshipNavigation', 'teammate']) {
        expect(durable, `the durable payload must not carry ${field}`).not.toHaveProperty(field);
      }

      if (row.nextMedians !== undefined && row.nextRevision !== undefined) {
        sessionDetailData = readDetail(row.nextRevision, row.nextMedians);
        view.rerender(<TestDetail />);
        const nextAnalytics = captures.analytics.at(-1) as { medians?: Medians } | undefined;
        expect(nextAnalytics?.medians).toEqual(row.expectedNextMedians);
      }
    });
  }
});
