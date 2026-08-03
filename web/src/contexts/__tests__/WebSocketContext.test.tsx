/**
 * Integration tests for WebSocketContext store keying logic.
 *
 * These tests verify that server messages are stored under the correct key
 * so that useChannel can read them. The key bug they guard against:
 *   - onmessage stored ALL messages under msg.type (e.g. "session_detail")
 *   - useChannel reads via subscriptionKey() which returns "session_detail:{id}"
 *   - Keys never matched, so useChannel never received session_detail data
 */

import { describe, it, expect, beforeEach, afterEach } from 'vitest';
import { render, screen, act } from '@testing-library/react';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { parseStrictYAML, requireExactFields, requireRecord, requireUniqueNames } from '@/test/strictYaml';
import { WebSocketProvider, useChannel } from '@/contexts/WebSocketContext';
import { ChannelTopic, AnnotationAxis, subscribe as mkSub } from '@/types/messages';
import type { ChannelName, SubscriptionMessage } from '@/types/messages';

// ---------------------------------------------------------------------------
// MockWebSocket — replaces globalThis.WebSocket in jsdom
// ---------------------------------------------------------------------------

class MockWebSocket {
  static instances: MockWebSocket[] = [];

  onopen: ((ev: Event) => void) | null = null;
  onmessage: ((ev: MessageEvent) => void) | null = null;
  onclose: (() => void) | null = null;
  onerror: (() => void) | null = null;
  readyState: number = WebSocket.CONNECTING;
  sent: string[] = [];

  constructor(_url: string) {
    MockWebSocket.instances.push(this);
  }

  send(data: string): void {
    this.sent.push(data);
  }

  close(): void {
    this.readyState = WebSocket.CLOSED;
  }

  // Test helpers

  simulateOpen(): void {
    this.readyState = WebSocket.OPEN;
    this.onopen?.(new Event('open'));
  }

  simulateMessage(data: unknown): void {
    this.onmessage?.(
      new MessageEvent('message', { data: JSON.stringify(data) }),
    );
  }
}

// ---------------------------------------------------------------------------
// Test consumer components
// ---------------------------------------------------------------------------

/** Renders useChannel data for a simple (no-id) channel. */
function SimpleConsumer({ topic }: { topic: ChannelName }) {
  const { data } = useChannel([topic]);
  return <div data-testid="data">{JSON.stringify(data ?? null)}</div>;
}

/** Renders useChannel data for session_detail using the new typed API. */
function SessionDetailConsumer({ sessionId }: { sessionId: string }) {
  const { data } = useChannel([ChannelTopic.SessionDetail as ChannelName], sessionId);
  return <div data-testid="data">{JSON.stringify(data ?? null)}</div>;
}

type ChannelRecoveryFixture = {
  cases: Array<{
    name: string;
    topic: ChannelName;
    id?: string;
    initialData: unknown;
    errorMessage: string;
  errorCode?: 'selection_visibility';
    recoveryData: unknown;
  }>;
};

const channelRecoverySource = readFileSync(resolve(process.cwd(), 'src/contexts/__tests__/testdata/channel_recovery.yaml'), 'utf8');
const channelRecoveryValue = requireRecord(parseStrictYAML(channelRecoverySource, 'channel recovery fixture'), 'channel recovery fixture');
requireExactFields(channelRecoveryValue, ['cases'], 'channel recovery fixture');
if (!Array.isArray(channelRecoveryValue.cases) || channelRecoveryValue.cases.length < 2) throw new Error('channel recovery fixture must cover selection and non-selection errors');
const channelRecoveryCases = channelRecoveryValue.cases.map((value, index) => requireRecord(value, `channel recovery fixture.cases[${index}]`));
requireUniqueNames(channelRecoveryCases, 'channel recovery fixture.cases');
for (const [index, fixtureCase] of channelRecoveryCases.entries()) requireExactFields(fixtureCase, ['name', 'topic', 'id', 'initialData', 'errorMessage', 'errorCode', 'recoveryData'], `channel recovery fixture.cases[${index}]`);
const channelRecoveryFixture = channelRecoveryValue as unknown as ChannelRecoveryFixture;

function RecoveryConsumer({ topic, id }: { topic: ChannelName; id?: string }) {
  const { data, error, errorCode } = useChannel([topic], id);
  return (
    <>
      <div data-testid="data">{JSON.stringify(data ?? null)}</div>
      <div data-testid="error">{error ?? ''}</div>
    <div data-testid="error-code">{errorCode ?? ''}</div>
    </>
  );
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function latestInstance(): MockWebSocket {
  return MockWebSocket.instances[MockWebSocket.instances.length - 1];
}

// ---------------------------------------------------------------------------
// Setup / teardown
// ---------------------------------------------------------------------------

let originalWebSocket: typeof WebSocket;

beforeEach(() => {
  MockWebSocket.instances = [];
  originalWebSocket = globalThis.WebSocket;
  // Replace the global WebSocket with the mock.
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  globalThis.WebSocket = MockWebSocket as any;
});

afterEach(() => {
  globalThis.WebSocket = originalWebSocket;
});

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

describe('WebSocketContext store keying', () => {
  for (const testCase of channelRecoveryFixture.cases) {
    it(testCase.name, async () => {
      render(
        <WebSocketProvider>
          <RecoveryConsumer topic={testCase.topic} id={testCase.id} />
        </WebSocketProvider>,
      );
      const ws = latestInstance();
      await act(async () => ws.simulateOpen());

      await act(async () => {
        ws.simulateMessage({ type: testCase.topic, data: testCase.initialData });
      });
      expect(JSON.parse(screen.getByTestId('data').textContent!)).toEqual(testCase.initialData);

      await act(async () => {
        ws.simulateMessage({
          type: 'error',
          topic: testCase.topic,
          id: testCase.id,
          message: testCase.errorMessage,
      data: testCase.errorCode ? { code: testCase.errorCode } : undefined,
        });
      });
      expect(JSON.parse(screen.getByTestId('data').textContent!)).toBeNull();
      expect(screen.getByTestId('error')).toHaveTextContent(testCase.errorMessage);
    expect(screen.getByTestId('error-code')).toHaveTextContent(testCase.errorCode ?? '');

      await act(async () => {
        ws.simulateMessage({ type: testCase.topic, data: testCase.recoveryData });
      });
      expect(JSON.parse(screen.getByTestId('data').textContent!)).toEqual(testCase.recoveryData);
      expect(screen.getByTestId('error')).toBeEmptyDOMElement();
    });
  }

  it('delivers a dashboard message to useChannel([dashboard])', async () => {
    render(
      <WebSocketProvider>
        <SimpleConsumer topic="dashboard" />
      </WebSocketProvider>,
    );

    const ws = latestInstance();

    await act(async () => {
      ws.simulateOpen();
    });

    const payload = { totalSessions: 42, totalTokens: 1000, avgDurationMins: 5 };

    await act(async () => {
      ws.simulateMessage({ type: 'dashboard', data: payload });
    });

    const rendered = JSON.parse(screen.getByTestId('data').textContent!);
    expect(rendered).toEqual(payload);
  });

  it('delivers a session_detail message keyed by id to useChannel([session_detail], id)', async () => {
    render(
      <WebSocketProvider>
        <SessionDetailConsumer sessionId="abc" />
      </WebSocketProvider>,
    );

    const ws = latestInstance();

    await act(async () => {
      ws.simulateOpen();
    });

    const payload = { id: 'abc', harness: 'claude-code', turns: [], turnCount: 3 };

    await act(async () => {
      ws.simulateMessage({ type: 'session_detail', data: payload });
    });

    const rendered = JSON.parse(screen.getByTestId('data').textContent!);
    expect(rendered).toEqual(payload);
  });

  it('does NOT deliver a session_detail message for session "abc" to a listener for session "xyz"', async () => {
    render(
      <WebSocketProvider>
        <SessionDetailConsumer sessionId="xyz" />
      </WebSocketProvider>,
    );

    const ws = latestInstance();

    await act(async () => {
      ws.simulateOpen();
    });

    // Deliver a message for session "abc", not "xyz"
    const payloadForAbc = { id: 'abc', harness: 'claude-code', turns: [], turnCount: 1 };

    await act(async () => {
      ws.simulateMessage({ type: 'session_detail', data: payloadForAbc });
    });

    // The consumer for "xyz" must remain empty (null)
    const rendered = JSON.parse(screen.getByTestId('data').textContent!);
    expect(rendered).toBeNull();
  });

  it('isolates two simultaneous session_detail listeners — each receives only its own messages', async () => {
    function DualConsumer() {
      const { data: dataAbc } = useChannel(
        [ChannelTopic.SessionDetail as ChannelName],
        'abc',
      );
      const { data: dataXyz } = useChannel(
        [ChannelTopic.SessionDetail as ChannelName],
        'xyz',
      );
      return (
        <>
          <div data-testid="data-abc">{JSON.stringify(dataAbc ?? null)}</div>
          <div data-testid="data-xyz">{JSON.stringify(dataXyz ?? null)}</div>
        </>
      );
    }

    render(
      <WebSocketProvider>
        <DualConsumer />
      </WebSocketProvider>,
    );

    const ws = latestInstance();

    await act(async () => {
      ws.simulateOpen();
    });

    const payloadAbc = { id: 'abc', harness: 'claude-code', turns: [], turnCount: 1 };
    const payloadXyz = { id: 'xyz', harness: 'claude-code', turns: [], turnCount: 2 };

    await act(async () => {
      ws.simulateMessage({ type: 'session_detail', data: payloadAbc });
    });

    await act(async () => {
      ws.simulateMessage({ type: 'session_detail', data: payloadXyz });
    });

    const renderedAbc = JSON.parse(screen.getByTestId('data-abc').textContent!);
    const renderedXyz = JSON.parse(screen.getByTestId('data-xyz').textContent!);

    expect(renderedAbc).toEqual(payloadAbc);
    expect(renderedXyz).toEqual(payloadXyz);
  });

  it('supports the legacy ChannelName[] + sessionId API used by SessionDetailClient', async () => {
    // This is the exact call shape used in SessionDetailClient.tsx:
    //   useChannel(['session_detail'], sessionId)
    render(
      <WebSocketProvider>
        <SessionDetailConsumer sessionId="legacy-session-id" />
      </WebSocketProvider>,
    );

    const ws = latestInstance();

    await act(async () => {
      ws.simulateOpen();
    });

    const payload = { id: 'legacy-session-id', harness: 'claude-code', turns: [], turnCount: 7 };

    await act(async () => {
      ws.simulateMessage({ type: 'session_detail', data: payload });
    });

    const rendered = JSON.parse(screen.getByTestId('data').textContent!);
    expect(rendered).toEqual(payload);
  });

  it('delivers an annotations message keyed by axis+id', async () => {
    function AnnotationsConsumer({ sub }: { sub: SubscriptionMessage }) {
      const { data } = useChannel(sub);
      return <div data-testid="data">{JSON.stringify(data ?? null)}</div>;
    }

    const sub = mkSub.annotations(AnnotationAxis.Session, 'sess-1');

    render(
      <WebSocketProvider>
        <AnnotationsConsumer sub={sub} />
      </WebSocketProvider>,
    );

    const ws = latestInstance();

    await act(async () => {
      ws.simulateOpen();
    });

    const payload = { axis: 'session', id: 'sess-1', annotations: [] };

    await act(async () => {
      ws.simulateMessage({ type: 'annotations', data: payload });
    });

    const rendered = JSON.parse(screen.getByTestId('data').textContent!);
    expect(rendered).toEqual(payload);
  });

  it('does NOT deliver an annotations message for one axis+id to a listener on a different axis+id', async () => {
    function AnnotationsConsumer({ sub }: { sub: SubscriptionMessage }) {
      const { data } = useChannel(sub);
      return <div data-testid="data">{JSON.stringify(data ?? null)}</div>;
    }

    // Listener is subscribed to 'other-sess'; message will arrive for 'sess-1'
    const sub = mkSub.annotations(AnnotationAxis.Session, 'other-sess');

    render(
      <WebSocketProvider>
        <AnnotationsConsumer sub={sub} />
      </WebSocketProvider>,
    );

    const ws = latestInstance();

    await act(async () => {
      ws.simulateOpen();
    });

    // Message for 'sess-1', listener is for 'other-sess'
    const payload = { axis: 'session', id: 'sess-1', annotations: [] };

    await act(async () => {
      ws.simulateMessage({ type: 'annotations', data: payload });
    });

    const rendered = JSON.parse(screen.getByTestId('data').textContent!);
    expect(rendered).toBeNull();
  });
});
