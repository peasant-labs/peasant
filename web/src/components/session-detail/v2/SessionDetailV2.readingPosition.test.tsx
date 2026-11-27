import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, waitFor } from '@testing-library/react';
import { parseTranscriptRouteQuery, type ProjectHash } from '@/lib/navigation/projectRoutes';
import { PublishProvider } from '@/contexts/PublishContext';
import { SessionDetailV2 } from './SessionDetailV2';
import { readTranscriptReadingState } from './lib/transcriptReadingState';

/**
 * The saved reading position on the mounted session page when the transcript
 * loads through its segment routes.
 *
 * The session_detail frame carries no turns, so the page shows its loading
 * skeleton until the first segment read answers, and only then mounts the
 * viewer and its inner `.txn-stream` scroller. Only the WebSocket channel, the
 * HTTP reads, and the theme are replaced: the page, the reading-state hook and
 * fairtrade's real `TranscriptViewer` all run, so the offset is read from the
 * record the production path writes.
 */

const PROJECT_HASH = 'b'.repeat(64) as ProjectHash;
const SESSION_ID = 'sess-reading-position';
const REVISION = '3';
const TURN_COUNT = 6;
const SCROLLED_TO = 432;
const Labels = Object.freeze({ Loading: 'Loading transcript', Graph: 'graph', List: 'list' });

let channelData: unknown;
let releaseSegments: (() => void) | null = null;
let segmentsHeld: Promise<void> = Promise.resolve();

vi.mock('@/lib/share/publishing', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/lib/share/publishing')>()),
  ...(await import('@/test/fixtures/publishingIdle')).PUBLISHING_IDLE,
}));
vi.mock('next/navigation', () => ({
  useSearchParams: () => new URLSearchParams(),
  usePathname: () => `/projects/${PROJECT_HASH}/${SESSION_ID}`,
  useRouter: () => ({ replace: vi.fn(), push: vi.fn() }),
}));
vi.mock('@/contexts/WebSocketContext', () => ({
  useChannel: () => ({
    data: channelData,
    channelFault: null,
    connectionFault: null,
    connected: true,
    error: null,
    refresh: vi.fn(),
  }),
}));
vi.mock('./lib/useEntryLabels', () => ({
  useEntryLabels: () => ({ entryTypes: [], labelsByEntry: new Map(), addLabel: vi.fn() }),
}));
vi.mock('@/hooks/useTheme', () => ({
  useTheme: () => ({ theme: 'dark', setTheme: vi.fn(), toggle: vi.fn() }),
}));
vi.mock('@peasant-labs/fairtrade/graph', () => ({ TrajectoryGraph: () => null }));

const turnIndexes = Array.from({ length: TURN_COUNT }, (_, index) => index);

/** The metadata-only frame the shipped server sends: no turns, so the page reads segments. */
function frame() {
  return {
    id: SESSION_ID,
    project: 'alpha-project',
    harness: 'claude-code',
    startTime: '2026-07-15T08:00:00Z',
    endTime: '2026-07-15T08:01:00Z',
    durationMins: 1,
    totalTokens: 12,
    tokensIn: 5,
    tokensOut: 7,
    turnCount: TURN_COUNT,
    toolCallCount: 0,
    revision: REVISION,
    turns: [],
    title: 'reading position host',
  };
}

function turnWire(index: number) {
  return {
    index,
    role: index % 2 === 0 ? 'user' : 'assistant',
    entryType: 'text',
    depth: 0,
    content: `turn ${index}`,
    timestamp: '2026-07-15T08:00:00Z',
    toolCalls: [],
    truncated: false,
    byteLength: 32,
  };
}

async function answer(input: RequestInfo | URL): Promise<Response> {
  const url = String(input);
  const segment = url.match(/\/transcript\/segments\/(\d+)/);
  if (segment) {
    // The segment read stays pending until the test releases it, so the page
    // renders its loading skeleton first, as it does against a real server.
    await segmentsHeld;
    if (Number(segment[1]) !== 0) return Response.json({ error: `segment ${segment[1]} is not served` }, { status: 404 });
    const turns = turnIndexes.map(turnWire);
    return Response.json({
      index: 0,
      revision: REVISION,
      firstTurn: 0,
      lastTurn: TURN_COUNT - 1,
      byteLength: turns.length * 32,
      hasOlder: false,
      hasNewer: false,
      turns,
    });
  }
  if (url.includes('/transcript/segments')) {
    return Response.json({
      sessionId: SESSION_ID,
      revision: REVISION,
      totalTurns: TURN_COUNT,
      totalSegments: 1,
      segments: [{ index: 0, firstTurn: 0, lastTurn: TURN_COUNT - 1, byteLength: TURN_COUNT * 32 }],
    });
  }
  return Response.json({ error: 'not a transcript read' }, { status: 404 });
}

function mountPage() {
  const routeQuery = parseTranscriptRouteQuery(new URLSearchParams());
  if (!routeQuery) throw new Error('the empty transcript route query must parse');
  return render(
    <PublishProvider>
      <SessionDetailV2 sessionId={SESSION_ID} projectHash={PROJECT_HASH} projectName="alpha-project" routeQuery={routeQuery} />
    </PublishProvider>,
  );
}

/** Mount behind the skeleton, then release the segment read and return the mounted stream. */
async function mountAfterSkeleton(view: ReturnType<typeof render>): Promise<HTMLElement> {
  await waitFor(() => expect(view.getByLabelText(Labels.Loading)).toBeInTheDocument());
  expect(view.container.querySelector('.txn-stream')).toBeNull();
  await act(async () => { releaseSegments?.(); });
  return waitFor(() => {
    const stream = view.container.querySelector<HTMLElement>('.txn-stream[data-segment-mode="true"]');
    if (!stream) throw new Error('the transcript stream did not mount after the segment read answered');
    return stream;
  });
}

/** Let any frame-scheduled host work run before the reader scrolls. */
async function nextFrame(): Promise<void> {
  await act(async () => { await new Promise<void>((resolve) => window.requestAnimationFrame(() => resolve())); });
}

async function expectSavedOffset(expected: number, detail: string): Promise<void> {
  await waitFor(() => {
    const saved = readTranscriptReadingState(SESSION_ID)?.scrollTop ?? null;
    if (saved !== expected) {
      throw new Error(`saved reading position invariant failed: ${detail}; the record says ${JSON.stringify(saved)}, the stream is at ${expected}`);
    }
  });
}

function scrollStream(stream: HTMLElement, top: number): void {
  stream.scrollTop = top;
  fireEvent.scroll(stream);
}

beforeEach(() => {
  window.sessionStorage.clear();
  channelData = frame();
  segmentsHeld = new Promise<void>((resolve) => { releaseSegments = resolve; });
  vi.stubGlobal('fetch', vi.fn(answer));
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  releaseSegments = null;
  window.sessionStorage.clear();
});

describe('SessionDetailV2 saved reading position for a segmented transcript', () => {
  it('saves the scroll offset of a stream that mounts after the loading skeleton', async () => {
    const view = mountPage();
    const stream = await mountAfterSkeleton(view);
    await nextFrame();

    scrollStream(stream, SCROLLED_TO);

    await expectSavedOffset(SCROLLED_TO, 'scrolling a stream mounted after the skeleton was not recorded');
  });

  it('keeps saving after the viewer replaces its stream for the graph view and back', async () => {
    const view = mountPage();
    await mountAfterSkeleton(view);
    await nextFrame();

    fireEvent.click(view.getByRole('button', { name: Labels.Graph }));
    await waitFor(() => expect(view.container.querySelector('.txn-stream')).toBeNull());
    fireEvent.click(view.getByRole('button', { name: Labels.List }));
    const replaced = await waitFor(() => {
      const stream = view.container.querySelector<HTMLElement>('.txn-stream');
      if (!stream) throw new Error('the transcript stream did not return after switching back to the list view');
      return stream;
    });
    await nextFrame();

    scrollStream(replaced, SCROLLED_TO);

    await expectSavedOffset(SCROLLED_TO, 'scrolling the stream the viewer mounted after the graph view was not recorded');
  });
});
