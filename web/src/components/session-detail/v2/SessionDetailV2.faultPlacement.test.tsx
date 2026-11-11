/**
 * The transcript surface under the REAL provider.
 *
 * Only the channel itself is production: `WebSocketProvider` is mounted and a
 * `MockWebSocket` delivers the frames. Everything around the transcript that
 * needs a browser, a REST server, or Fairtrade's heavy viewer is stubbed so the
 * failure placement — not the rendering package — is what this suite measures.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { PublishProvider } from '@/contexts/PublishContext';
import { WebSocketProvider } from '@/contexts/WebSocketContext';
import { ConnectionFaultNotice } from '@/components/ConnectionFaultNotice';
import { SessionDetailV2 } from './SessionDetailV2';
import { parseTranscriptRouteQuery, type ProjectHash } from '@/lib/navigation/projectRoutes';
import { MAX_FRAME_BYTES } from '@/lib/faults';
import { parseStrictYAML, requireRecord } from '@/test/strictYaml';
import { MockWebSocket } from '@/test/mockWebSocket';

const PROJECT_HASH = 'a'.repeat(64) as ProjectHash;
const SESSION_ID = 'sess-fault-placement';
const PATHNAME = '/projects/alpha-project/sess-fault-placement';

vi.mock('next/navigation', () => ({
  useSearchParams: () => new URLSearchParams(),
  usePathname: () => PATHNAME,
  useRouter: () => ({ replace: vi.fn(), push: vi.fn() }),
}));

// The publish flow is not this suite's subject; its Local API calls stay idle.
vi.mock('@/lib/share/publishing', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/lib/share/publishing')>()),
  ...(await import('@/test/fixtures/publishingIdle')).PUBLISHING_IDLE,
}));

// REST-backed label loading and the additive session extras are out of scope.
vi.mock('./lib/useEntryLabels', () => ({
  useEntryLabels: () => ({ entryTypes: [], labelsByEntry: new Map(), addLabel: vi.fn() }),
}));
vi.mock('@/hooks/useSessionExtras', () => ({
  useSessionExtras: () => ({ commits: [], pullRequests: [], unverifiedPullRequests: [] }),
}));

// Theme persistence is unrelated to this suite.
vi.mock('@/hooks/useTheme', () => ({
  useTheme: () => ({ theme: 'dark', setTheme: vi.fn(), toggle: vi.fn() }),
}));

// The graph canvas and the packaged viewer are rendering concerns; the host's
// fault placement is what this suite renders around them.
vi.mock('@peasant-labs/fairtrade/graph', () => ({ TrajectoryGraph: () => null }));
vi.mock(import('@peasant-labs/fairtrade/ui'), async (importOriginal) => {
  const actual = await importOriginal();
  const { createElement } = await import('react');
  return {
    ...actual,
    TranscriptViewer: (props: React.ComponentProps<typeof actual.TranscriptViewer>) =>
      createElement(
        'div',
        { 'data-testid': 'package-session-detail' },
        createElement('div', {}, props.streamPrelude as React.ReactNode),
      ),
    adaptTranscript: (() => ({ turns: [] })) as unknown as typeof actual.adaptTranscript,
    computeAnalytics: (() => ({})) as unknown as typeof actual.computeAnalytics,
    annotateTranscript: (() => []) as unknown as typeof actual.annotateTranscript,
    computePersonalMedians: (() => undefined) as unknown as typeof actual.computePersonalMedians,
    prefilterTurns: ((turns: unknown) => turns) as unknown as typeof actual.prefilterTurns,
  };
});

// A schema-valid durable detail, shaped like the provider's own fixture family.
const rawMessages = requireRecord(
  parseStrictYAML(
    readFileSync(resolve(process.cwd(), 'src/contexts/__tests__/testdata/raw_server_messages.yaml'), 'utf8'),
    'raw messages',
  ),
  'raw messages',
);
const detailFields = String(rawMessages.detailFields);

const DETAIL = JSON.parse(
  `{${detailFields.replace('"id":"raw-session"', `"id":"${SESSION_ID}"`).replace('"harness":"pi"', '"harness":"claude-code"')},"project":"alpha-project","turns":[{"index":0,"role":"user","entryType":"text","depth":0,"content":"question","timestamp":"2026-01-01T00:00:00Z"},{"index":1,"role":"assistant","entryType":"text","depth":0,"content":"answer","timestamp":"2026-01-01T00:00:01Z"}]}`,
);

function transcriptSurface() {
  return (
    <WebSocketProvider>
      <ConnectionFaultNotice />
      <PublishProvider>
        <SessionDetailV2
          sessionId={SESSION_ID}
          projectHash={PROJECT_HASH}
          projectName="alpha-project"
          routeQuery={parseTranscriptRouteQuery('')!}
        />
      </PublishProvider>
    </WebSocketProvider>
  );
}

let originalWebSocket: typeof WebSocket;

beforeEach(() => {
  MockWebSocket.reset();
  originalWebSocket = globalThis.WebSocket;
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  globalThis.WebSocket = MockWebSocket as any;
});

afterEach(() => {
  cleanup();
  globalThis.WebSocket = originalWebSocket;
});

describe('transcript surface fault placement', () => {
  it('provider fault before detail keeps the transcript skeleton, not a fatal panel', async () => {
    render(transcriptSurface());

    const ws = MockWebSocket.latest();
    await act(async () => ws.simulateOpen());

    // The quality frame the live store cannot deliver is a PROVIDER failure.
    // It is not this session's data failure, so the transcript keeps loading.
    await act(async () => {
      ws.simulateRawMessage(JSON.stringify({ type: 'quality', data: 'x'.repeat(MAX_FRAME_BYTES) }));
    });

    expect(screen.queryByText('transcript unavailable')).not.toBeInTheDocument();
    // The transcript stays on its skeleton, and a frame failure on a live
    // socket offers a retry of this channel.
    expect(screen.getByLabelText('Loading transcript')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'try again' })).toBeInTheDocument();
    // The one app-level banner states the provider failure, naming its
    // operation, cause, and remedy.
    const banner = screen.getByTestId('connection-fault-notice');
    expect(banner).toHaveTextContent('the quality update was');
    expect(banner).toHaveTextContent('the app accepts up to 8 MiB');
  });

  it('session_detail channel fault shows transcript unavailable with its message and a retry', async () => {
    render(transcriptSurface());

    const ws = MockWebSocket.latest();
    await act(async () => ws.simulateOpen());

    await act(async () => {
      ws.simulateMessage({
        type: 'error',
        topic: 'session_detail',
        id: SESSION_ID,
        message: 'session detail is unavailable until the store is repaired',
      });
    });

    expect(screen.getByText('transcript unavailable')).toBeInTheDocument();
    expect(screen.getByText(/session detail is unavailable/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'try again' })).toBeInTheDocument();
    // A channel fault is local; the provider-wide banner stays silent.
    expect(screen.queryByTestId('connection-fault-notice')).not.toBeInTheDocument();
  });

  it('keeps the loaded transcript when its channel refuses a refresh, with the note above it', async () => {
    render(transcriptSurface());

    const ws = MockWebSocket.latest();
    await act(async () => ws.simulateOpen());
    await act(async () => ws.simulateMessage({ type: 'session_detail', data: DETAIL }));
    expect(screen.getByTestId('package-session-detail')).toBeInTheDocument();

    await act(async () => {
      ws.simulateMessage({
        type: 'error',
        topic: 'session_detail',
        id: SESSION_ID,
        message: 'the re-index refused this read',
      });
    });

    // The rows stay; the failure is a distinct channel banner with a retry.
    expect(screen.getByTestId('package-session-detail')).toBeInTheDocument();
    expect(screen.queryByText('transcript unavailable')).not.toBeInTheDocument();
    const banner = screen.getByRole('alert');
    expect(banner).toHaveTextContent('transcript update unavailable');
    expect(banner).toHaveTextContent('the re-index refused this read');
    const before = ws.sent.length;
    fireEvent.click(within(banner).getByRole('button', { name: 'try again' }));
    expect(ws.sent.length).toBeGreaterThan(before);
  });

  it('topicless provider errors do not create a transcript channel banner', async () => {
    render(transcriptSurface());
    const ws = MockWebSocket.latest();
    await act(async () => ws.simulateOpen());
    await act(async () => ws.simulateMessage({ type: 'session_detail', data: DETAIL }));
    await act(async () => ws.simulateMessage({ type: 'error', message: 'provider refused the request' }));
    expect(screen.getByTestId('package-session-detail')).toBeInTheDocument();
    expect(screen.queryByText('transcript update unavailable')).not.toBeInTheDocument();
  });

  it('socket onerror does not create a transcript channel banner', async () => {
    render(transcriptSurface());
    const ws = MockWebSocket.latest();
    await act(async () => ws.simulateOpen());
    await act(async () => ws.simulateMessage({ type: 'session_detail', data: DETAIL }));
    await act(async () => ws.simulateError());
    expect(screen.getByTestId('package-session-detail')).toBeInTheDocument();
    expect(screen.queryByText('transcript update unavailable')).not.toBeInTheDocument();
  });

  it('a late detail replaces the skeleton with no residual fatal state', async () => {
    render(transcriptSurface());

    const ws = MockWebSocket.latest();
    await act(async () => ws.simulateOpen());
    await act(async () => {
      ws.simulateRawMessage(JSON.stringify({ type: 'quality', data: 'x'.repeat(MAX_FRAME_BYTES) }));
    });
    expect(screen.queryByTestId('package-session-detail')).not.toBeInTheDocument();

    await act(async () => ws.simulateMessage({ type: 'session_detail', data: DETAIL }));

    expect(screen.getByTestId('package-session-detail')).toBeInTheDocument();
    expect(screen.queryByText('transcript unavailable')).not.toBeInTheDocument();
  });

  it('a channel fault on another channel never renders as this transcript failure', async () => {
    render(transcriptSurface());

    const ws = MockWebSocket.latest();
    await act(async () => ws.simulateOpen());

    // A sessions topic error belongs to the sessions surface; the transcript
    // must not claim it.
    await act(async () => {
      ws.simulateMessage({
        type: 'error',
        topic: 'sessions',
        message: 'the sessions read is unavailable',
      });
    });

    expect(screen.queryByText('transcript unavailable')).not.toBeInTheDocument();
    expect(screen.queryByText(/the sessions read is unavailable/)).not.toBeInTheDocument();
  });
});
