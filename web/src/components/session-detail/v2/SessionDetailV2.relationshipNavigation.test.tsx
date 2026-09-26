import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { act, cleanup, render } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ComponentProps } from 'react';
import type { SessionDetailPayload, SessionRelationshipNavigation } from '@peasant-labs/schema';
import { SessionDetailV2 } from './SessionDetailV2';
import { parseTranscriptRouteQuery, transcriptHref, type ProjectHash } from '@/lib/navigation/projectRoutes';
import { loadContextNavigationFixture } from '@/test/contextNavigationFixture';

/**
 * The host's boundary with the published adapter, driven by the same fixture as
 * the mounted route test. It pins two things the mounted test cannot observe
 * from the DOM: the durable payload handed to the adapter carries NO flat-read
 * field (the adapter refuses a durable payload that carries one), and the
 * authorized read navigation travels in the adapter options instead.
 */

const PROJECT_HASH = 'a'.repeat(64) as ProjectHash;
const CASES_PATH = 'src/components/session-detail/v2/testdata/context_navigation.yaml';
const MANIFEST_PATH = 'src/components/session-detail/v2/testdata/context_navigation.manifest.yaml';
const fixture = loadContextNavigationFixture(
  readFileSync(resolve(process.cwd(), CASES_PATH), 'utf8'),
  readFileSync(resolve(process.cwd(), MANIFEST_PATH), 'utf8'),
);

const routerPush = vi.hoisted(() => vi.fn());

vi.mock('next/navigation', () => ({
  useSearchParams: () => new URLSearchParams(),
  usePathname: () => '/projects/alpha-project/sess_contextchild',
  useRouter: () => ({ replace: vi.fn(), push: routerPush }),
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

const captures = vi.hoisted(() => ({
  payloads: [] as Array<Record<string, unknown>>,
  options: [] as Array<Record<string, unknown> | undefined>,
  callbacks: [] as Array<Record<string, unknown> | undefined>,
}));

vi.mock(import('@peasant-labs/fairtrade/ui'), async (importOriginal) => {
  const actual = await importOriginal();
  return {
    ...actual,
    TranscriptViewer: ((props: ComponentProps<typeof actual.TranscriptViewer>) => {
      captures.callbacks.push(props.callbacks as Record<string, unknown> | undefined);
      return <div data-testid="context-navigation-viewer" />;
    }) as unknown as typeof actual.TranscriptViewer,
    adaptTranscript: ((payload: Record<string, unknown>, _annotations: unknown, _analytics: unknown, options?: Record<string, unknown>) => {
      captures.payloads.push(payload);
      captures.options.push(options);
      return { turns: payload.turns };
    }) as unknown as typeof actual.adaptTranscript,
    computeAnalytics: (() => ({})) as unknown as typeof actual.computeAnalytics,
    annotateTranscript: (() => []) as unknown as typeof actual.annotateTranscript,
    computePersonalMedians: (() => undefined) as unknown as typeof actual.computePersonalMedians,
    prefilterTurns: ((turns: unknown) => turns) as unknown as typeof actual.prefilterTurns,
  };
});

function routeQuery() {
  const parsed = parseTranscriptRouteQuery(new URLSearchParams());
  if (!parsed) throw new Error('context-navigation route query must be valid');
  return parsed;
}

function TestDetail() {
  return (
    <SessionDetailV2
      sessionId="sess_contextchild"
      projectHash={PROJECT_HASH}
      projectName="alpha-project"
      routeQuery={routeQuery()}
    />
  );
}

beforeEach(() => {
  captures.payloads.length = 0;
  captures.options.length = 0;
  captures.callbacks.length = 0;
  routerPush.mockClear();
});

afterEach(() => {
  cleanup();
  channelData = undefined;
});

describe('host adapter boundary for authorized relationship navigation', () => {
  for (const testCase of fixture.cases) {
    it(`${testCase.name} separates read metadata from durable content`, () => {
      const durable: SessionDetailPayload = {
        id: 'sess_contextchild',
        harness: 'codex',
        project: 'alpha-project',
        startTime: '2026-08-28T09:00:00.000Z',
        endTime: '2026-08-28T09:02:00.000Z',
        durationMins: 2,
        totalTokens: 20,
        tokensIn: 12,
        tokensOut: 8,
        turnCount: 1,
        toolCallCount: 0,
        relationships: testCase.relationships as unknown as SessionDetailPayload['relationships'],
        turns: [
          { index: 0, role: 'user', entryType: 'text', depth: 0, content: 'child request', timestamp: '2026-08-28T09:00:00.000Z' },
        ],
      };
      channelData = { ...durable, relationshipNavigation: testCase.navigation };

      render(<TestDetail />);

      const payload = captures.payloads.at(-1);
      expect(payload).toBeDefined();
      expect(Object.keys(payload ?? {})).not.toContain('relationshipNavigation');
      expect(captures.options.at(-1)?.relationshipNavigation).toEqual(testCase.navigation);
      expect(typeof captures.callbacks.at(-1)?.onNavigateRelationship).toBe('function');
    });
  }

  it('routes every linkable status to its exact stored target and never routes an honest non-linkable status', () => {
    channelData = {
      id: 'sess_contextchild',
      harness: 'codex',
      project: 'alpha-project',
      turns: [{ index: 0, role: 'user', depth: 0, content: 'q0', toolCalls: [] }],
    };

    render(<TestDetail />);
    const onNavigate = captures.callbacks.at(-1)?.onNavigateRelationship as (entry: SessionRelationshipNavigation) => void;
    expect(typeof onNavigate).toBe('function');

    for (const entry of fixture.linkCases) {
      routerPush.mockClear();
      act(() => {
        onNavigate(entry as unknown as SessionRelationshipNavigation);
      });
      if (entry.expectedTarget) {
        expect(routerPush, entry.name).toHaveBeenCalledWith(transcriptHref(PROJECT_HASH, entry.expectedTarget));
      } else {
        expect(routerPush, entry.name).not.toHaveBeenCalled();
      }
    }
  });
});
