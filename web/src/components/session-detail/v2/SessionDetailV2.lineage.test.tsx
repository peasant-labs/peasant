import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';
import { parse } from 'yaml';
import { SessionDetailV2 } from './SessionDetailV2';
import { parseTranscriptRouteQuery, transcriptHref, type ProjectHash } from '@/lib/navigation/projectRoutes';
import { PublishProvider } from '@/contexts/PublishContext';

/**
 * The transcript prelude's host-owned lineage line, driven by the flat local
 * read. The fixture states what the read carries; the suite asserts the mounted
 * prelude renders exactly the identity line and the parent link it names, and
 * that neither read-only field reaches the durable payload the viewer adapts.
 */

const PROJECT_HASH = 'a'.repeat(64) as ProjectHash;
const CASES_PATH = 'src/components/session-detail/v2/testdata/transcript_lineage.yaml';

interface LineageCase {
  name: string;
  teammate: { team: string; name: string } | null;
  parentSessionId: string | null;
}

interface LineageFixture {
  requiredNames: string[];
  cases: LineageCase[];
}

function loadFixture(): LineageFixture {
  const fixture = parse(readFileSync(resolve(process.cwd(), CASES_PATH), 'utf8')) as LineageFixture;
  const names = fixture.cases.map((entry) => entry.name);
  if (
    fixture.requiredNames.length !== names.length ||
    new Set(names).size !== names.length ||
    fixture.requiredNames.some((name) => !names.includes(name))
  ) {
    throw new Error('transcript lineage fixture cases and their required-name manifest must name the same cases');
  }
  return fixture;
}

vi.mock('@/lib/share/publishing', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/lib/share/publishing')>()),
  ...(await import('@/test/fixtures/publishingIdle')).PUBLISHING_IDLE,
}));

vi.mock('next/navigation', () => ({
  useSearchParams: () => new URLSearchParams(),
  usePathname: () => `/projects/${PROJECT_HASH}/sess_lineage`,
  useRouter: () => ({ replace: vi.fn() }),
}));

let channelData: unknown;
vi.mock('@/contexts/WebSocketContext', () => ({
  useChannel: () => ({ data: channelData, connected: true, error: null }),
}));

vi.mock('./lib/useEntryLabels', () => ({
  useEntryLabels: () => ({ entryTypes: [], labelsByEntry: new Map(), addLabel: vi.fn() }),
}));

vi.mock('@/hooks/useTheme', () => ({
  useTheme: () => ({ theme: 'dark', setTheme: vi.fn(), toggle: vi.fn() }),
}));

vi.mock('@peasant-labs/fairtrade/graph', () => ({ TrajectoryGraph: () => null }));

const payloads = vi.hoisted(() => [] as Array<Record<string, unknown>>);

vi.mock(import('@peasant-labs/fairtrade/ui'), async (importOriginal) => {
  const actual = await importOriginal();
  return {
    ...actual,
    TranscriptViewer: ((props: { streamPrelude?: unknown }) => (
      <div data-testid="package-session-detail">{props.streamPrelude as React.ReactNode}</div>
    )) as unknown as typeof actual.TranscriptViewer,
    adaptTranscript: ((payload: Record<string, unknown>) => {
      payloads.push(payload);
      return { turns: payload.turns ?? [] };
    }) as unknown as typeof actual.adaptTranscript,
    computeAnalytics: (() => ({})) as unknown as typeof actual.computeAnalytics,
    annotateTranscript: (() => []) as unknown as typeof actual.annotateTranscript,
    computePersonalMedians: (() => undefined) as unknown as typeof actual.computePersonalMedians,
    prefilterTurns: ((turns: unknown) => turns) as unknown as typeof actual.prefilterTurns,
  };
});

function detailFor(testCase: LineageCase) {
  return {
    id: 'sess_lineage',
    harness: 'claude-code',
    project: 'alpha-project',
    startTime: '2026-10-25T09:00:00Z',
    endTime: '2026-10-25T09:10:00Z',
    durationMins: 10,
    totalTokens: 0,
    tokensIn: 0,
    tokensOut: 0,
    turnCount: 0,
    toolCallCount: 0,
    turns: [],
    parentSessionId: testCase.parentSessionId,
    teammate: testCase.teammate
      ? { ...testCase.teammate, evidence: 'native_dynamic_workflow' }
      : null,
  };
}

function TestDetail() {
  const routeQuery = parseTranscriptRouteQuery(new URLSearchParams());
  if (!routeQuery) throw new Error('route query must be valid');
  return (
    <PublishProvider>
      <SessionDetailV2 sessionId="sess_lineage" projectHash={PROJECT_HASH} projectName="alpha-project" routeQuery={routeQuery} />
    </PublishProvider>
  );
}

afterEach(() => {
  cleanup();
  channelData = undefined;
  payloads.length = 0;
});

describe('transcript prelude lineage', () => {
  for (const testCase of loadFixture().cases) {
    it(`${testCase.name} renders the recorded lineage and keeps it out of the durable payload`, () => {
      channelData = detailFor(testCase);
      render(<TestDetail />);

      if (testCase.teammate) {
        const line = screen.getByText(`teammate ${testCase.teammate.name} on ${testCase.teammate.team}`);
        expect(line).toHaveAttribute('data-transcript-teammate', 'true');
      } else {
        expect(screen.queryByText(/^teammate /)).not.toBeInTheDocument();
        expect(document.querySelector('[data-transcript-teammate]')).toBeNull();
      }

      if (testCase.teammate && testCase.parentSessionId) {
        const link = screen.getByRole('link', { name: 'parent transcript' });
        expect(link).toHaveAttribute('href', transcriptHref(PROJECT_HASH, testCase.parentSessionId));
      } else {
        expect(screen.queryByRole('link', { name: 'parent transcript' })).not.toBeInTheDocument();
      }

      expect(payloads.length).toBeGreaterThan(0);
      for (const [index, payload] of payloads.entries()) {
        const keys = Object.keys(payload);
        expect(keys, `captured durable payload ${index}`).not.toContain('teammate');
        expect(keys, `captured durable payload ${index}`).not.toContain('relationshipNavigation');
      }
    });
  }
});
