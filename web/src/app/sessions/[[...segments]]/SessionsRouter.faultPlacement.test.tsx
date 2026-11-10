/**
 * The project-sessions surface under the REAL provider.
 *
 * The channel, the provider store, and the fault placement are production; only
 * next/navigation and the grouped REST routes are stubbed. A provider-wide
 * failure must never render as "Couldn't load sessions".
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, render, screen, waitFor } from '@testing-library/react';
import { PublishProvider } from '@/contexts/PublishContext';
import { WebSocketProvider } from '@/contexts/WebSocketContext';
import { ConnectionFaultNotice } from '@/components/ConnectionFaultNotice';
import { SessionsRouter } from './SessionsRouter';
import { ALPHA_HASH, makeSession } from '@/app/review/[[...segments]]/test-fixtures';
import { MAX_FRAME_BYTES } from '@/lib/faults';
import { MockWebSocket } from '@/test/mockWebSocket';

const PROJECT_HASH = ALPHA_HASH;

vi.mock('next/navigation', () => ({
  usePathname: () => `/sessions/${PROJECT_HASH}`,
  useRouter: () => ({ replace: vi.fn(), push: vi.fn() }),
}));

function okResponse(payload: unknown) {
  return Promise.resolve({ ok: true, json: async () => payload } as Response);
}

function groupedPayload(over: Record<string, unknown> = {}) {
  return {
    page: 1,
    perPage: 25,
    total: 1,
    nextItems: [],
    revision: '0',
    ordinarySessionTotal: 1,
    helperThreadTotal: 0,
    items: [{ kind: 'transcript', transcript: { session: makeSession({ id: 'agent-a1' }) } }],
    ...over,
  };
}

function renderSurface() {
  return render(
    <WebSocketProvider>
      <ConnectionFaultNotice />
      <PublishProvider>
        <SessionsRouter />
      </PublishProvider>
    </WebSocketProvider>,
  );
}

let originalWebSocket: typeof WebSocket;

beforeEach(() => {
  MockWebSocket.reset();
  originalWebSocket = globalThis.WebSocket;
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  globalThis.WebSocket = MockWebSocket as any;
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL) => {
      const url = new URL(String(input));
      if (url.pathname === '/api/v1/sessions') return okResponse(groupedPayload());
      return Promise.resolve({ ok: false, status: 404, text: async () => 'not found' } as Response);
    }),
  );
});

afterEach(() => {
  cleanup();
  globalThis.WebSocket = originalWebSocket;
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('project sessions surface fault placement', () => {
  it('a provider fault leaves the skeleton and never says the sessions list failed', async () => {
    renderSurface();
    const ws = MockWebSocket.latest();
    await act(async () => ws.simulateOpen());

    await act(async () => {
      ws.simulateRawMessage(JSON.stringify({ type: 'quality', data: 'x'.repeat(MAX_FRAME_BYTES) }));
    });

    expect(screen.queryByText(/Couldn’t load sessions/)).not.toBeInTheDocument();
    expect(screen.queryByText(/Couldn't load sessions/)).not.toBeInTheDocument();
    expect(screen.getByLabelText('Loading sessions')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'try again' })).toBeInTheDocument();
    expect(screen.getByTestId('connection-fault-notice')).toHaveTextContent('the quality update was');
  });

  it('a sessions channel fault with no data owns the error paragraph and offers a retry', async () => {
    renderSurface();
    const ws = MockWebSocket.latest();
    await act(async () => ws.simulateOpen());

    await act(async () => {
      ws.simulateMessage({
        type: 'error',
        topic: 'sessions',
        message: 'the session store is locked',
      });
    });

    expect(await screen.findByText(/Couldn’t load sessions:/)).toBeInTheDocument();
    expect(screen.getByText(/the session store is locked/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'try again' })).toBeInTheDocument();
    expect(screen.queryByTestId('connection-fault-notice')).not.toBeInTheDocument();
  });

  it('keeps the loaded sessions and shows the note when its channel refuses a refresh', async () => {
    renderSurface();
    const ws = MockWebSocket.latest();
    await act(async () => ws.simulateOpen());

    await act(async () => {
      ws.simulateMessage({
        type: 'sessions',
        data: { sessions: [makeSession({ id: 'agent-a1' })] },
      });
    });
    expect(await screen.findByRole('heading', { name: 'alpha-project' })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText('agent-a1')).toBeInTheDocument());

    await act(async () => {
      ws.simulateMessage({
        type: 'error',
        topic: 'sessions',
        message: 'the session store is locked',
      });
    });

    // The rows stay; the refusal is a note above them.
    expect(screen.getByRole('heading', { name: 'alpha-project' })).toBeInTheDocument();
    expect(screen.getByText(/the session store is locked/)).toBeInTheDocument();
    expect(screen.getByText('agent-a1')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'try again' })).toBeInTheDocument();
  });

  it('keeps the loaded sessions when a provider fault arrives', async () => {
    renderSurface();
    const ws = MockWebSocket.latest();
    await act(async () => ws.simulateOpen());

    await act(async () => {
      ws.simulateMessage({
        type: 'sessions',
        data: { sessions: [makeSession({ id: 'agent-a1' })] },
      });
    });
    expect(await screen.findByRole('heading', { name: 'alpha-project' })).toBeInTheDocument();

    await act(async () => {
      ws.simulateRawMessage(JSON.stringify({ type: 'quality', data: 'x'.repeat(MAX_FRAME_BYTES) }));
    });

    // The rows stay: the heading still reads its project and the page body
    // still renders the grouped list rather than a fatal paragraph.
    expect(screen.getByRole('heading', { name: 'alpha-project' })).toBeInTheDocument();
    expect(screen.queryByText(/Couldn’t load sessions/)).not.toBeInTheDocument();
    expect(screen.getByTestId('connection-fault-notice')).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText('agent-a1')).toBeInTheDocument());
  });
});
