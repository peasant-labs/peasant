/**
 * The project browser (browse by project) under the REAL provider.
 *
 * The project read comes from its own REST route, so a provider-wide socket or
 * frame failure must not block it. A topic error on THIS channel blocks the
 * browse only while the channel has delivered no data; with data loaded it is a
 * note above the rows.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, render, screen } from '@testing-library/react';
import { WebSocketProvider } from '@/contexts/WebSocketContext';
import { ConnectionFaultNotice } from '@/components/ConnectionFaultNotice';
import LegacyHomeBrowse from './LegacyHomeBrowse';
import { ALPHA_HASH, makeSession } from '@/app/review/[[...segments]]/test-fixtures';
import { MAX_FRAME_BYTES } from '@/lib/faults';
import { MockWebSocket } from '@/test/mockWebSocket';

const SUMMARIES = {
  projects: [
    {
      project: 'alpha-project',
      projectHash: ALPHA_HASH,
      sessions: 3,
      recordedFiles: 2,
      totalFiles: 4,
      lastWorkMs: 1_700_000_000_000,
      openChanges: 1,
    },
  ],
  selection: { active: false, hiddenProjects: 0, hiddenSessions: 0 },
};

// The summary cache is per-test: a channel fault blocks the browse only when
// this channel has no data, so the test controls what the REST route delivered.
const summaries = vi.hoisted(() => ({ current: null as typeof SUMMARIES | null }));

vi.mock('@/lib/api/map', () => ({
  cachedProjectSummaries: () => summaries.current,
  fetchProjectSummaries: () => Promise.resolve(summaries.current),
}));

function renderSurface() {
  return render(
    <WebSocketProvider>
      <ConnectionFaultNotice />
      <LegacyHomeBrowse view="projects" />
    </WebSocketProvider>,
  );
}

let originalWebSocket: typeof WebSocket;

beforeEach(() => {
  MockWebSocket.reset();
  originalWebSocket = globalThis.WebSocket;
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  globalThis.WebSocket = MockWebSocket as any;
  summaries.current = SUMMARIES;
});

afterEach(() => {
  cleanup();
  globalThis.WebSocket = originalWebSocket;
});

describe('project browser fault placement', () => {
  it('a provider fault does not block the project read and shows no error panel', async () => {
    renderSurface();
    const ws = MockWebSocket.latest();
    await act(async () => ws.simulateOpen());

    await act(async () => {
      ws.simulateMessage({ type: 'sessions', data: { sessions: [makeSession({ id: 'agent-a1' })] } });
    });
    expect(await screen.findByText('alpha-project')).toBeInTheDocument();

    await act(async () => {
      ws.simulateRawMessage(JSON.stringify({ type: 'quality', data: 'x'.repeat(MAX_FRAME_BYTES) }));
    });

    // The real negative: the provider fault is the app-level banner's story,
    // never a browse error panel, and the project row survives.
    expect(screen.getByText('alpha-project')).toBeInTheDocument();
    expect(screen.queryByText(/the server refused the sessions read/)).not.toBeInTheDocument();
    expect(screen.getByTestId('connection-fault-notice')).toHaveTextContent('the quality update was');
  });

  it('a sessions channel fault with no data blocks the browse', async () => {
    summaries.current = null;
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

    expect(await screen.findByText(/the server refused the sessions read/)).toBeInTheDocument();
    expect(screen.getByText(/the session store is locked/)).toBeInTheDocument();
    // No browse body: no project row, no picker heading.
    expect(screen.queryByText('alpha-project')).not.toBeInTheDocument();
    expect(screen.queryByText('projects', { selector: 'h3' })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'try again' })).toBeInTheDocument();
    expect(screen.queryByTestId('connection-fault-notice')).not.toBeInTheDocument();
  });

  it('keeps the loaded rows and notes a channel fault above them', async () => {
    renderSurface();
    const ws = MockWebSocket.latest();
    await act(async () => ws.simulateOpen());

    await act(async () => {
      ws.simulateMessage({ type: 'sessions', data: { sessions: [makeSession({ id: 'agent-a1' })] } });
    });
    expect(await screen.findByText('alpha-project')).toBeInTheDocument();

    await act(async () => {
      ws.simulateMessage({
        type: 'error',
        topic: 'sessions',
        message: 'the session store is locked',
      });
    });

    expect(screen.getByText(/the session store is locked/)).toBeInTheDocument();
    expect(screen.getByText('alpha-project')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'try again' })).toBeInTheDocument();
  });
});
