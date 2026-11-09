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
import { faultOperationForMessageType, MAX_FRAME_BYTES } from '@/lib/faults';

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

  simulateClose(): void {
    this.readyState = WebSocket.CLOSED;
    this.onclose?.();
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

type FixtureTopic =
  | 'dashboard'
  | 'sessions'
  | 'trends'
  | 'quality'
  | 'session_detail'
  | 'annotations'
  | 'project_familiarity';

const FIXTURE_TOPICS: readonly FixtureTopic[] = [
  'dashboard',
  'sessions',
  'trends',
  'quality',
  'session_detail',
  'annotations',
  'project_familiarity',
];

function subscriptionFor(topic: FixtureTopic, id?: string, axis?: AnnotationAxis): SubscriptionMessage {
  switch (topic) {
    case 'session_detail': return mkSub.sessionDetail(id ?? 'raw-session');
    case 'annotations': return mkSub.annotations(axis ?? AnnotationAxis.Session, id ?? 'raw-session');
    case 'project_familiarity': return mkSub.projectFamiliarity(id ?? 'raw-session');
    case 'dashboard': return mkSub.dashboard();
    case 'sessions': return mkSub.sessions();
    case 'trends': return mkSub.trends();
    case 'quality': return mkSub.quality();
  }
}

/** A valid message for the operation a fault names, used to prove recovery. */
function recoveryMessage(topic: FixtureTopic, id?: string, axis?: AnnotationAxis): Record<string, unknown> {
  if (topic === 'session_detail') return rawMessages.legacyDetail;
  if (topic === 'annotations') return { axis: axis ?? 'session', id: id ?? 'raw-session', annotations: [] };
  if (topic === 'project_familiarity') return { projectHash: id ?? 'a'.repeat(64), files: [] };
  return { total: 1 };
}

type ChannelRecoveryFixture = {
  cases: Array<{
    name: string;
    topic: FixtureTopic;
    id?: string;
    axis?: AnnotationAxis;
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
for (const [index, fixtureCase] of channelRecoveryCases.entries()) requireExactFields(fixtureCase, ['name', 'topic', 'id', 'axis', 'initialData', 'errorMessage', 'errorCode', 'recoveryData'], `channel recovery fixture.cases[${index}]`);
const channelRecoveryFixture = channelRecoveryValue as unknown as ChannelRecoveryFixture;
const channelRecoveryManifest = requireRecord(
  parseStrictYAML(readFileSync(resolve(process.cwd(), 'src/contexts/__tests__/testdata/channel_recovery.manifest.yaml'), 'utf8'), 'channel recovery manifest'),
  'channel recovery manifest',
);
requireExactRequiredFields(channelRecoveryManifest, ['requiredNames'], 'channel recovery manifest');
if (!Array.isArray(channelRecoveryManifest.requiredNames)) throw new Error('channel recovery manifest requires requiredNames');
const channelRecoveryNames = new Set(channelRecoveryFixture.cases.map((testCase) => testCase.name));
for (const name of channelRecoveryManifest.requiredNames) {
  if (typeof name !== 'string' || !channelRecoveryNames.has(name)) throw new Error(`channel recovery fixture missing required case ${JSON.stringify(name)}`);
}

function RecoveryConsumer({ sub }: { sub: SubscriptionMessage }) {
  const { data, error, errorCode, fault } = useChannel(sub);
  return (
    <>
      <div data-testid="data">{JSON.stringify(data ?? null)}</div>
      <div data-testid="error">{error ?? ''}</div>
      <div data-testid="error-code">{errorCode ?? ''}</div>
      <div data-testid="fault-operation">{fault?.operation.wire ?? ''}</div>
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
  const cases = rows.map<{
    name: string;
    topic: FixtureTopic;
    id?: string;
    axis?: AnnotationAxis;
    raw: string;
    accepted: boolean;
    faultOperation: string;
    faultCause: 'frame_invalid' | 'payload_too_large';
  }>((row) => {
    const label = `raw messages.${row.name}`;
    requireExactFields(row, ['name', 'topic', 'accepted', 'faultOperation', 'faultCause', 'axis', 'id', 'raw', 'repeat'], label);
    if (typeof row.name !== 'string' || typeof row.raw !== 'string' || !row.raw || typeof row.accepted !== 'boolean') throw new Error(`${label} requires name, raw text and boolean accepted`);
    const topic = row.topic;
    if (typeof topic !== 'string' || !FIXTURE_TOPICS.includes(topic as FixtureTopic)) throw new Error(`${label} has unsupported topic`);
    if (typeof row.faultOperation !== 'string' || !FIXTURE_TOPICS.includes(row.faultOperation as FixtureTopic)) throw new Error(`${label} requires a faultOperation matching a provider topic`);
    if (row.accepted === false && row.faultCause !== undefined && row.faultCause !== 'frame_invalid' && row.faultCause !== 'payload_too_large') throw new Error(`${label} has an unsupported faultCause`);
    if (row.accepted === true && row.faultCause !== undefined) throw new Error(`${label} cannot name a fault cause for an accepted frame`);
    if (row.id !== undefined && typeof row.id !== 'string') throw new Error(`${label}.id must be a string`);
    if (row.axis !== undefined && typeof row.axis !== 'string') throw new Error(`${label}.axis must be a string`);
    let raw = row.raw.replaceAll('$DETAIL_LEGACY', legacyFields).replaceAll('$DETAIL', detailFields);
    if (row.repeat !== undefined) {
      const repeat = requireRecord(row.repeat, `${label}.repeat`);
      requireExactRequiredFields(repeat, ['unit', 'count'], `${label}.repeat`);
      if (typeof repeat.unit !== 'string' || !repeat.unit || typeof repeat.count !== 'number' || !Number.isSafeInteger(repeat.count) || repeat.count < 1 || repeat.count > 1000000 || !raw.includes('$REPEAT')) throw new Error(`${label}.repeat requires a unit, bounded positive integer count and template marker`);
      raw = raw.replaceAll('$REPEAT', repeat.unit.repeat(repeat.count));
    }
    if (/\$(?:DETAIL|REPEAT)/.test(raw)) throw new Error(`${label} contains an unresolved template marker`);
    const faultCause = row.faultCause === 'payload_too_large' ? 'payload_too_large' : 'frame_invalid';
    return {
      name: row.name,
      topic: topic as FixtureTopic,
      id: row.id as string | undefined,
      axis: row.axis as AnnotationAxis | undefined,
      accepted: row.accepted,
      faultOperation: row.faultOperation,
      faultCause,
      raw,
    };
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
      const subscription = subscriptionFor(fixture.topic, fixture.id, fixture.axis);
      render(
        <WebSocketProvider>
          <RecoveryConsumer sub={subscription} />
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
        expect(screen.getByTestId('fault-operation')).toBeEmptyDOMElement();
      } else {
        const operation = faultOperationForMessageType(fixture.faultOperation);
        const error = screen.getByTestId('error');
        expect(error).not.toBeEmptyDOMElement();
        expect(error).toHaveTextContent('the page kept its last data');
        expect(error).toHaveTextContent(operation.label);
        if (fixture.faultCause === 'payload_too_large') {
          expect(error).toHaveTextContent(new RegExp(`${operation.label} update was \\d+(?:\\.\\d)? MiB; the app accepts up to 8 MiB`));
          expect(error).toHaveTextContent(
            fixture.faultOperation === 'sessions' || fixture.faultOperation === 'projects'
              ? 'lower the sessions per page'
              : 'try again',
          );
        } else {
          expect(error).toHaveTextContent('did not match this app');
          expect(error).toHaveTextContent('update peasant, then try again');
        }
      }
      // A rejected frame must not poison the socket or either subscription API:
      // the same operation's next good message clears its fault, and no other
      // operation's success is needed to do it.
      const recovery = recoveryMessage(
        fixture.faultOperation as FixtureTopic,
        fixture.id,
        fixture.axis,
      );
      await act(async () => ws.simulateMessage({ type: fixture.faultOperation, data: recovery }));
      const topicRecovery = fixture.faultOperation === fixture.topic
        ? recovery
        : recoveryMessage(fixture.topic, fixture.id, fixture.axis);
      if (fixture.faultOperation !== fixture.topic) {
        await act(async () => ws.simulateMessage({ type: fixture.topic, data: topicRecovery }));
      }
      expect(JSON.parse(screen.getByTestId('data').textContent!)).toEqual(topicRecovery);
      expect(screen.getByTestId('error')).toBeEmptyDOMElement();
      expect(screen.getByTestId('fault-operation')).toBeEmptyDOMElement();
    });
  }
  for (const testCase of channelRecoveryFixture.cases) {
    it(testCase.name, async () => {
      const subscription = subscriptionFor(testCase.topic, testCase.id, testCase.axis);
      render(
        <WebSocketProvider>
          <RecoveryConsumer sub={subscription} />
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
      expect(screen.getByTestId('error')).toHaveTextContent('the server refused the');
      expect(screen.getByTestId('error')).toHaveTextContent('the page kept its last data');
    expect(screen.getByTestId('error-code')).toHaveTextContent(testCase.errorCode ?? '');
      expect(screen.getByTestId('fault-operation')).not.toBeEmptyDOMElement();

      await act(async () => {
        ws.simulateMessage({ type: testCase.topic, data: testCase.recoveryData });
      });
      expect(JSON.parse(screen.getByTestId('data').textContent!)).toEqual(testCase.recoveryData);
      expect(screen.getByTestId('error')).toBeEmptyDOMElement();
      expect(screen.getByTestId('fault-operation')).toBeEmptyDOMElement();
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

  it('asks the server again for a channel the page holds when the page refreshes it', async () => {
    let refresh: () => void = () => undefined;
    function RefreshingConsumer() {
      const channel = useChannel(mkSub.sessionDetail('abc'));
      refresh = channel.refresh;
      return null;
    }
    render(
      <WebSocketProvider>
        <RefreshingConsumer />
      </WebSocketProvider>,
    );
    const ws = latestInstance();
    await act(async () => {
      ws.simulateOpen();
    });
    const subscribes = () => ws.sent.map((raw) => JSON.parse(raw)).filter((msg) => msg.type === 'subscribe');
    const before = subscribes().length;

    await act(async () => {
      refresh();
    });

    // One more subscribe for the same channel: the server answers it with a
    // fresh snapshot and keeps a single subscription.
    expect(subscribes()).toHaveLength(before + 1);
    expect(subscribes().at(-1)).toEqual({ type: 'subscribe', channels: [mkSub.sessionDetail('abc')] });
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

  it('keeps a quality fault when an unrelated sessions frame succeeds', async () => {
    function QualityAndSessions() {
      const quality = useChannel(mkSub.quality());
      const sessions = useChannel(mkSub.sessions());
      return (
        <>
          <div data-testid="quality-error">{quality.error ?? ''}</div>
          <div data-testid="quality-operation">{quality.connectionFault?.operation.wire ?? ''}</div>
          <div data-testid="sessions-data">{JSON.stringify(sessions.data ?? null)}</div>
        </>
      );
    }

    render(
      <WebSocketProvider>
        <QualityAndSessions />
      </WebSocketProvider>,
    );
    const ws = latestInstance();
    await act(async () => ws.simulateOpen());

    const oversized = JSON.stringify({ type: 'quality', data: 'x'.repeat(MAX_FRAME_BYTES) });
    await act(async () => ws.simulateRawMessage(oversized));

    expect(screen.getByTestId('quality-error')).toHaveTextContent('the quality update was');
    expect(screen.getByTestId('quality-error')).toHaveTextContent('the app accepts up to 8 MiB');
    expect(screen.getByTestId('quality-operation')).toHaveTextContent('quality-page');

    // The unrelated success must not erase the quality frame's failure.
    await act(async () => ws.simulateMessage({ type: 'sessions', data: { sessions: [] } }));
    expect(JSON.parse(screen.getByTestId('sessions-data').textContent!)).toEqual({ sessions: [] });
    expect(screen.getByTestId('quality-error')).toHaveTextContent('the quality update was');
    expect(screen.getByTestId('quality-operation')).toHaveTextContent('quality-page');

    // The same operation's next good frame clears it.
    await act(async () => ws.simulateMessage({ type: 'quality', data: { sessions: [] } }));
    expect(screen.getByTestId('quality-error')).toBeEmptyDOMElement();
    expect(screen.getByTestId('quality-operation')).toBeEmptyDOMElement();
  });

  it('never renders session A\'s fault on session B and never clears A on B\'s success', async () => {
    function TwoSessions() {
      const a = useChannel(mkSub.sessionDetail('session-a'));
      const b = useChannel(mkSub.sessionDetail('session-b'));
      return (
        <>
          <div data-testid="a-error">{a.error ?? ''}</div>
          <div data-testid="a-fault">{a.fault?.operation.wire ?? ''}</div>
          <div data-testid="b-error">{b.error ?? ''}</div>
        </>
      );
    }

    render(
      <WebSocketProvider>
        <TwoSessions />
      </WebSocketProvider>,
    );
    const ws = latestInstance();
    await act(async () => ws.simulateOpen());

    await act(async () => {
      ws.simulateMessage({
        type: 'error',
        topic: ChannelTopic.SessionDetail,
        id: 'session-a',
        message: 'session a is unavailable until the store is repaired',
      });
    });
    expect(screen.getByTestId('a-error')).toHaveTextContent('session a is unavailable');
    expect(screen.getByTestId('a-fault')).toHaveTextContent('session-detail-read');
    expect(screen.getByTestId('b-error')).toBeEmptyDOMElement();

    const payloadB = { ...rawMessages.legacyDetail, id: 'session-b' };
    await act(async () => ws.simulateMessage({ type: 'session_detail', data: payloadB }));
    expect(screen.getByTestId('a-error')).toHaveTextContent('session a is unavailable');

    const payloadA = { ...rawMessages.legacyDetail, id: 'session-a' };
    await act(async () => ws.simulateMessage({ type: 'session_detail', data: payloadA }));
    expect(screen.getByTestId('a-error')).toBeEmptyDOMElement();
    expect(screen.getByTestId('a-fault')).toBeEmptyDOMElement();
  });

  it('a retry keeps a fault on failure and clears it on success', async () => {
    let refresh: () => void = () => undefined;
    function RetryingConsumer() {
      const channel = useChannel(mkSub.sessions());
      refresh = channel.refresh;
      return <div data-testid="error">{channel.error ?? ''}</div>;
    }
    render(
      <WebSocketProvider>
        <RetryingConsumer />
      </WebSocketProvider>,
    );
    const ws = latestInstance();
    await act(async () => ws.simulateOpen());

    await act(async () => {
      ws.simulateMessage({ type: 'error', topic: ChannelTopic.Sessions, message: 'the store is locked' });
    });
    expect(screen.getByTestId('error')).toHaveTextContent('the server refused the sessions read');

    await act(async () => refresh());
    await act(async () => {
      ws.simulateMessage({ type: 'error', topic: ChannelTopic.Sessions, message: 'the store is still locked' });
    });
    expect(screen.getByTestId('error')).toHaveTextContent('the store is still locked');

    await act(async () => refresh());
    await act(async () => ws.simulateMessage({ type: 'sessions', data: { sessions: [] } }));
    expect(screen.getByTestId('error')).toBeEmptyDOMElement();
  });

  it('a reconnect clears the connection fault and keeps a channel fault', async () => {
    function DashboardConsumer() {
      const dashboard = useChannel(mkSub.dashboard());
      return (
        <>
          <div data-testid="connection-error">{dashboard.connectionFault?.message ?? ''}</div>
          <div data-testid="channel-error">{dashboard.error ?? ''}</div>
        </>
      );
    }
    render(
      <WebSocketProvider>
        <DashboardConsumer />
      </WebSocketProvider>,
    );
    const ws = latestInstance();
    await act(async () => ws.simulateOpen());

    await act(async () => {
      ws.simulateRawMessage(JSON.stringify({ type: 'quality', data: 'x'.repeat(MAX_FRAME_BYTES) }));
    });
    expect(screen.getByTestId('connection-error')).toHaveTextContent('the quality update was');

    await act(async () => {
      ws.simulateMessage({ type: 'error', topic: ChannelTopic.Dashboard, message: 'the store is locked' });
    });
    expect(screen.getByTestId('channel-error')).toHaveTextContent('the server refused the dashboard read');

    await act(async () => ws.simulateClose());
    expect(screen.getByTestId('connection-error')).toHaveTextContent('the connection to peasant closed');

    await act(async () => ws.simulateOpen());
    expect(screen.getByTestId('connection-error')).toBeEmptyDOMElement();
    expect(screen.getByTestId('channel-error')).toHaveTextContent('the server refused the dashboard read');
  });
});
