/**
 * The project browser (browse by project) under the REAL provider.
 *
 * The project read comes from its own REST route, so a provider-wide socket or
 * frame failure must not block it. Only a topic error on THIS channel does.
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

vi.mock('@/lib/api/map', () => ({
  cachedProjectSummaries: () => SUMMARIES,
  fetchProjectSummaries: () => Promise.resolve(SUMMARIES),
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
});

afterEach(() => {
  cleanup();
  globalThis.WebSocket = originalWebSocket;
});

describe('project browser fault placement', () => {
  it('a provider fault does not block the project read', async () => {
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

    // The project row survives; the provider fault is the app-level banner's
    // story, not a browse error panel.
    expect(screen.getByText('alpha-project')).toBeInTheDocument();
    expect(screen.queryByText(/the sessions read is unavailable/)).not.toBeInTheDocument();
    expect(screen.getByTestId('connection-fault-notice')).toHaveTextContent('the quality update was');
  });

  it('a sessions channel fault renders its own local error panel', async () => {
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
    expect(screen.queryByTestId('connection-fault-notice')).not.toBeInTheDocument();
  });
});
