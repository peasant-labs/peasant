import { render, renderHook, screen, waitFor } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { useTranscriptPublish } from './useTranscriptPublish';

const recorded = vi.hoisted(() => vi.fn());
vi.mock('next/navigation', () => ({ useRouter: () => ({ push: vi.fn() }) }));
vi.mock('@/contexts/PublishContext', () => ({
  usePublishState: () => ({ redactionCache: new Map(), updateRedactionCache: vi.fn(), publishing: new Map(), settled: new Map(), publish: vi.fn() }),
}));
vi.mock('@/lib/share/redactionScan', () => ({
  useRedactionPipeline: (...args: unknown[]) => {
    recorded(args[0]);
    return { phase: 'ready', sessionRedactions: new Map(), sessionMatchCounts: new Map(), scanError: null, runScan: vi.fn(), isReviewReady: vi.fn() };
  },
}));
vi.mock('@/lib/share/publishing', async (original) => ({
  ...(await original<typeof import('@/lib/share/publishing')>()),
  ...(await import('@/test/fixtures/publishingIdle')).PUBLISHING_IDLE,
  fetchPublicationState: async () => ({ publication: { state: 'published', audience: [], publishedAt: '2026-07-15T08:00:00Z' }, audienceKnown: true }),
}));
vi.mock('@/lib/ft-ui', async (original) => ({
  ...(await original<typeof import('@/lib/ft-ui')>()),
  PublishDialog: ({ changes, state, stoppedAt }: { changes?: { summary: string }; state: string; stoppedAt?: string }) => <div><span>{changes?.summary}</span><span>{state}</span><span>{stoppedAt}</span></div>,
}));

afterEach(() => { vi.unstubAllGlobals(); recorded.mockClear(); });

it('sends the detail revision to the scan and updates it on revision change', async () => {
  vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new Error('offline')));
  const options = {
    sessionId: 'sess-segment-host', title: 'session', turns: [{ timestamp: '2026-07-15T08:00:00Z' }],
    coverage: { loadedSegments: 1, totalSegments: 8 }, loaded: true, moreItems: [],
    openOnArrival: true, onArrivalHandled: vi.fn(),
  };
  const { rerender } = renderHook(({ revision }) => useTranscriptPublish({ ...options, revision }), { initialProps: { revision: '7' } });
  await waitFor(() => expect(recorded).toHaveBeenCalledWith([{ id: 'sess-segment-host', version: 'revision:7' }]));
  rerender({ revision: '8' });
  await waitFor(() => expect(recorded).toHaveBeenCalledWith([{ id: 'sess-segment-host', version: 'revision:8' }]));
});

it('uses the per-session turn count without a revision and stops without either signal', async () => {
  vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new Error('offline')));
  const base = { sessionId: 'sess-segment-host', title: 'session', revision: '', turns: [{ timestamp: '2026-07-15T08:00:00Z' }], coverage: { loadedSegments: 1, totalSegments: 8 }, loaded: true, moreItems: [], openOnArrival: true, onArrivalHandled: vi.fn() };
  const initialProps: { turnCount?: number } = { turnCount: 12 };
  const { result, rerender } = renderHook(({ turnCount }) => useTranscriptPublish({ ...base, turnCount }), { initialProps });
  await waitFor(() => expect(recorded).toHaveBeenCalledWith([{ id: 'sess-segment-host', version: 'turn-count:12' }]));
  rerender({ turnCount: 13 });
  await waitFor(() => expect(recorded).toHaveBeenCalledWith([{ id: 'sess-segment-host', version: 'turn-count:13' }]));
  rerender({ turnCount: undefined });
  render(<>{result.current.dialog}</>);
  expect(screen.getByText('stopped')).toBeTruthy();
  expect(screen.getByText('checking this session: its revision and turn count are missing; refresh the session or re-index it before publishing')).toBeTruthy();
});

it('shows incomplete and complete comparison text in the update dialog', async () => {
  vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new Error('offline')));
  const base = {
    sessionId: 'sess-segment-host', title: 'session', revision: '7',
    turns: [{ timestamp: '2026-07-15T08:00:01Z' }], loaded: true, moreItems: [],
    openOnArrival: true, onArrivalHandled: vi.fn(),
  };
  const { result, rerender } = renderHook(({ loadedSegments }) => useTranscriptPublish({
    ...base, coverage: { loadedSegments, totalSegments: 8 },
  }), { initialProps: { loadedSegments: 1 } });
  const view = render(<>{result.current.dialog}</>);
  await waitFor(() => {
    view.rerender(<>{result.current.dialog}</>);
    expect(screen.getByText('loaded 1 of 8 segments; new turns are not compared until the rest of the transcript is loaded')).toBeTruthy();
  });
  rerender({ loadedSegments: 8 });
  view.rerender(<>{result.current.dialog}</>);
  expect(screen.getByText('1 new turn since you published')).toBeTruthy();
});
