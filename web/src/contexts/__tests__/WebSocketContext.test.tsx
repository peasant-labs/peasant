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
import { parseStrictYAML, requireExactFields, requireExactRequiredFields, requireRecord, requireUniqueNames } from '@/test/strictYaml';
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
    this.simulateRawMessage(JSON.stringify(data));
  }

  simulateRawMessage(data: string): void {
    this.onmessage?.(
      new MessageEvent('message', { data }),
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

function loadRawMessageFixtures() {
  const directory = resolve(process.cwd(), 'src/contexts/__tests__/testdata');
  const value = requireRecord(parseStrictYAML(readFileSync(resolve(directory, 'raw_server_messages.yaml'), 'utf8'), 'raw messages'), 'raw messages');
  requireExactRequiredFields(value, ['detailFields', 'cases'], 'raw messages');
  if (typeof value.detailFields !== 'string' || !value.detailFields) throw new Error('raw messages.detailFields must be nonempty JSON members');
  const detailFields = value.detailFields;
  const legacyFields = detailFields.replace('"harness":"pi"', '"harness":"claude-code"');
  const legacyDetail = requireRecord(JSON.parse(`{${legacyFields},"turns":[]}`), 'legacy detail');
  if (!Array.isArray(value.cases)) throw new Error('raw messages.cases must be an array');
  const rows = value.cases.map((row, index) => requireRecord(row, `raw messages.cases[${index}]`));
  requireUniqueNames(rows, 'raw messages.cases');
  const cases = rows.map<{ name: string; topic: ChannelName; raw: string; accepted: boolean }>((row) => {
    const label = `raw messages.${row.name}`;
    requireExactFields(row, ['name', 'topic', 'accepted', 'raw', 'repeat'], label);
    if (typeof row.name !== 'string' || typeof row.raw !== 'string' || !row.raw || typeof row.accepted !== 'boolean') throw new Error(`${label} requires name, raw text and boolean accepted`);
    const topic = row.topic;
    if (topic !== 'session_detail' && topic !== 'dashboard' && topic !== 'sessions') throw new Error(`${label} has unsupported topic`);
    let raw = row.raw.replaceAll('$DETAIL_LEGACY', legacyFields).replaceAll('$DETAIL', detailFields);
    if (row.repeat !== undefined) {
      const repeat = requireRecord(row.repeat, `${label}.repeat`);
      requireExactRequiredFields(repeat, ['unit', 'count'], `${label}.repeat`);
      if (typeof repeat.unit !== 'string' || !repeat.unit || typeof repeat.count !== 'number' || !Number.isSafeInteger(repeat.count) || repeat.count < 1 || repeat.count > 100000 || !raw.includes('$REPEAT')) throw new Error(`${label}.repeat requires a unit, bounded positive integer count and template marker`);
      raw = raw.replaceAll('$REPEAT', repeat.unit.repeat(repeat.count));
    }
    if (/\$(?:DETAIL|REPEAT)/.test(raw)) throw new Error(`${label} contains an unresolved template marker`);
    return { name: row.name, topic, accepted: row.accepted, raw };
  });
  const manifest = requireRecord(parseStrictYAML(readFileSync(resolve(directory, 'raw_server_messages.manifest.yaml'), 'utf8'), 'raw message manifest'), 'raw message manifest');
  requireExactRequiredFields(manifest, ['requiredNames'], 'raw message manifest');
  if (!Array.isArray(manifest.requiredNames) || manifest.requiredNames.length === 0 || manifest.requiredNames.some((name) => typeof name !== 'string' || !name) || new Set(manifest.requiredNames).size !== manifest.requiredNames.length) throw new Error('raw message manifest requires unique nonempty names');
  const names = new Set(cases.map((row) => row.name));
  for (const name of manifest.requiredNames) if (!names.has(name)) throw new Error(`raw messages missing required case ${name}`);
  return { cases, legacyDetail };
}

const rawMessages = loadRawMessageFixtures();

function TypedDetailConsumer() {
  const { data } = useChannel(mkSub.sessionDetail('raw-session'));
  return <div data-testid="typed-detail">{JSON.stringify(data ?? null)}</div>;
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
  for (const fixture of rawMessages.cases) {
    it(fixture.name, async () => {
      render(
        <WebSocketProvider>
          <RecoveryConsumer topic={fixture.topic} id="raw-session" />
          <TypedDetailConsumer />
        </WebSocketProvider>,
      );
      const ws = latestInstance();
      await act(async () => ws.simulateOpen());
      await act(async () => ws.simulateRawMessage(fixture.raw));
      const expected = fixture.accepted ? JSON.parse(fixture.raw).data : null;
      expect(JSON.parse(screen.getByTestId('data').textContent!)).toEqual(expected);
      expect(JSON.parse(screen.getByTestId('typed-detail').textContent!)).toEqual(fixture.topic === 'session_detail' ? expected : null);
      if (fixture.accepted) {
        expect(screen.getByTestId('error')).toBeEmptyDOMElement();
      } else {
        expect(screen.getByTestId('error')).toHaveTextContent('WebSocketProvider');
        expect(screen.getByTestId('error')).toHaveTextContent('not applied');
        expect(screen.getByTestId('error')).toHaveTextContent('Retry');
      }
      // A rejected frame must not poison the socket or either subscription API.
      const recovery = fixture.topic === 'session_detail' ? rawMessages.legacyDetail : { total: 1 };
      await act(async () => ws.simulateMessage({ type: fixture.topic, data: recovery }));
      expect(JSON.parse(screen.getByTestId('data').textContent!)).toEqual(recovery);
      expect(screen.getByTestId('error')).toBeEmptyDOMElement();
    });
  }
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

    const payload = { ...rawMessages.legacyDetail, id: 'abc', turnCount: 3 };

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
    const payloadForAbc = { ...rawMessages.legacyDetail, id: 'abc', turnCount: 1 };

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

    const payloadAbc = { ...rawMessages.legacyDetail, id: 'abc', turnCount: 1 };
    const payloadXyz = { ...rawMessages.legacyDetail, id: 'xyz', turnCount: 2 };

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

    const payload = { ...rawMessages.legacyDetail, id: 'legacy-session-id', turnCount: 7 };

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
