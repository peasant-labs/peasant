/**
 * The one app-level banner for a provider-wide failure, under the REAL provider.
 *
 * It states a frame or socket failure once for every page, offers no manual
 * retry, and clears when the socket returns.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { act, cleanup, render, screen } from '@testing-library/react';
import { WebSocketProvider } from '@/contexts/WebSocketContext';
import { ConnectionFaultNotice } from './ConnectionFaultNotice';
import { PageNotices } from './PageNotices';
import { MAX_FRAME_BYTES } from '@/lib/faults';
import { MockWebSocket } from '@/test/mockWebSocket';

function renderBanner() {
  return render(
    <WebSocketProvider>
      <ConnectionFaultNotice />
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

describe('ConnectionFaultNotice', () => {
  it('states a frame failure once, with no manual retry control', async () => {
    renderBanner();
    const ws = MockWebSocket.latest();
    await act(async () => ws.simulateOpen());

    expect(screen.queryByTestId('connection-fault-notice')).not.toBeInTheDocument();

    await act(async () => {
      ws.simulateRawMessage(JSON.stringify({ type: 'quality', data: 'x'.repeat(MAX_FRAME_BYTES) }));
    });

    const banner = screen.getByTestId('connection-fault-notice');
    expect(banner).toHaveAttribute('data-connection-fault', 'payload-too-large');
    expect(banner).toHaveTextContent('the quality update was');
    expect(banner).toHaveTextContent('the page kept its last data');
    // Automatic only: the banner never offers a retry or any other control.
    expect(screen.queryAllByRole('button')).toHaveLength(0);
    expect(screen.queryAllByRole('link')).toHaveLength(0);
  });

  it('clears when the socket returns', async () => {
    renderBanner();
    const ws = MockWebSocket.latest();
    await act(async () => ws.simulateOpen());

    await act(async () => {
      ws.simulateRawMessage(JSON.stringify({ type: 'quality', data: 'x'.repeat(MAX_FRAME_BYTES) }));
    });
    expect(screen.getByTestId('connection-fault-notice')).toBeInTheDocument();

    // While the app is stopped the offline notice owns the story, so this
    // banner stands down.
    await act(async () => ws.simulateClose());
    expect(screen.queryByTestId('connection-fault-notice')).not.toBeInTheDocument();

    // The provider's automatic reconnect clears the fault on open.
    await act(async () => ws.simulateOpen());
    expect(screen.queryByTestId('connection-fault-notice')).not.toBeInTheDocument();
  });
});

describe('PageNotices', () => {
  it('mounts exactly one provider-fault notice above the page', async () => {
    render(
      <WebSocketProvider>
        <PageNotices />
      </WebSocketProvider>,
    );
    const ws = MockWebSocket.latest();
    await act(async () => ws.simulateOpen());

    await act(async () => {
      ws.simulateRawMessage(JSON.stringify({ type: 'quality', data: 'x'.repeat(MAX_FRAME_BYTES) }));
    });

    expect(screen.getAllByTestId('connection-fault-notice')).toHaveLength(1);
    // The message is stated once, not once per notice.
    expect(document.body.textContent?.match(/the quality update was/g)).toHaveLength(1);
  });
});
