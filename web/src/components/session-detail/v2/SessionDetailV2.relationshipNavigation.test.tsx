import { act, cleanup, render } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ComponentProps } from 'react';
import { SessionDetailV2 } from './SessionDetailV2';
import { parseTranscriptRouteQuery, transcriptHref, type ProjectHash } from '@/lib/navigation/projectRoutes';

const PROJECT_HASH = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' as ProjectHash;

const routerPush = vi.hoisted(() => vi.fn());

vi.mock('next/navigation', () => ({
  useSearchParams: () => new URLSearchParams(),
  usePathname: () => '/projects/alpha-project/sess-relnav',
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

// Capture the published adapter-options boundary and the viewer callbacks so a
// host-plumbing test can prove the flat local read's authorized navigation is
// passed through and the exact-ID callback routes to the stored target. The
// pinned package does not yet consume either, so this asserts HOST wiring only;
// mounted click/Back behavior stays gated behind the canonical package release.
const captures = vi.hoisted(() => ({
  options: [] as Array<Record<string, unknown> | undefined>,
  callbacks: [] as Array<Record<string, unknown> | undefined>,
}));

vi.mock(import('@peasant-labs/fairtrade/ui'), async (importOriginal) => {
  const actual = await importOriginal();
  return {
    ...actual,
    TranscriptViewer: ((props: ComponentProps<typeof actual.TranscriptViewer>) => {
      captures.callbacks.push(props.callbacks as Record<string, unknown> | undefined);
      return <div data-testid="relnav-viewer" />;
    }) as unknown as typeof actual.TranscriptViewer,
    adaptTranscript: ((payload: { turns: unknown }, _annotations: unknown, _analytics: unknown, options?: Record<string, unknown>) => {
      captures.options.push(options);
      return { turns: payload.turns };
    }) as unknown as typeof actual.adaptTranscript,
    computeAnalytics: (() => ({})) as unknown as typeof actual.computeAnalytics,
    annotateTranscript: (() => []) as unknown as typeof actual.annotateTranscript,
    computePersonalMedians: (() => undefined) as unknown as typeof actual.computePersonalMedians,
    prefilterTurns: ((turns: unknown) => turns) as unknown as typeof actual.prefilterTurns,
  };
});

const TARGET_ID = '90000000-0000-4000-8000-000000000001';

function routeQuery() {
  const parsed = parseTranscriptRouteQuery(new URLSearchParams());
  if (!parsed) throw new Error('relationship-navigation route query must be valid');
  return parsed;
}

function TestDetail() {
  return (
    <SessionDetailV2
      sessionId="sess-relnav"
      projectHash={PROJECT_HASH}
      projectName="alpha-project"
      routeQuery={routeQuery()}
    />
  );
}

beforeEach(() => {
  captures.options.length = 0;
  captures.callbacks.length = 0;
  routerPush.mockClear();
});

afterEach(() => {
  cleanup();
  channelData = undefined;
});

describe('mounted relationship-navigation data plumbing', () => {
  it('passes the flat read navigation through the adapter boundary and supplies the exact-ID callback', () => {
    const resolved = { kind: 'started_by', status: 'resolved', localId: TARGET_ID };
    const unavailable = { kind: 'context_from', status: 'known_unavailable' };
    channelData = {
      id: 'sess-relnav',
      project: 'alpha-project',
      harness: 'claude-code',
      turns: [{ index: 0, role: 'user', depth: 0, content: 'q0', toolCalls: [] }],
      relationshipNavigation: [resolved, unavailable],
    };

    render(<TestDetail />);

    expect(captures.options.at(-1)?.relationshipNavigation).toEqual([resolved, unavailable]);

    const callbacks = captures.callbacks.at(-1);
    expect(typeof callbacks?.onNavigateRelationship).toBe('function');

    act(() => {
      (callbacks?.onNavigateRelationship as (entry: unknown) => void)(resolved);
    });
    expect(routerPush).toHaveBeenCalledWith(transcriptHref(PROJECT_HASH, TARGET_ID));

    // A non-resolved target never navigates; an honest unavailable link cannot
    // route to the wrong session.
    act(() => {
      (callbacks?.onNavigateRelationship as (entry: unknown) => void)(unavailable);
    });
    expect(routerPush).toHaveBeenCalledTimes(1);
  });
});
