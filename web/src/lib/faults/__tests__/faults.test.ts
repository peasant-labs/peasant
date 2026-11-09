import { describe, expect, it } from 'vitest';
import * as schema from '@peasant-labs/schema';
import { AllChannelTopics } from '@peasant-labs/schema';
import { DiscoveryErrorCode, DiscoveryRequestError } from '@/lib/api/errors';
import {
  ALL_FAULT_CAUSES,
  ALL_FAULT_OPERATIONS,
  ALL_FAULT_REMEDIES,
  boundedDetail,
  codeFromFault,
  DetailEvidence,
  FaultCause,
  FaultOperation,
  FaultRemedy,
  FaultStore,
  faultOperationForTopic,
  faultFromDiscoveryError,
  formatByteSize,
  HttpEvidence,
  MAX_EVIDENCE_DETAIL,
  MAX_FRAME_BYTES,
  PayloadEvidence,
  peekMessageType,
  TopicEvidence,
  WebFault,
} from '@/lib/faults';

describe('closed fault sets', () => {
  it('lists every operation, cause, and remedy in its ALL_ set', () => {
    expect(new Set(ALL_FAULT_OPERATIONS)).toEqual(new Set(Object.values(FaultOperation)));
    expect(new Set(ALL_FAULT_CAUSES)).toEqual(new Set(Object.values(FaultCause)));
    expect(new Set(ALL_FAULT_REMEDIES)).toEqual(new Set(Object.values(FaultRemedy)));
  });

  it('gives every member a unique wire and a non-empty label', () => {
    for (const set of [ALL_FAULT_OPERATIONS, ALL_FAULT_CAUSES, ALL_FAULT_REMEDIES]) {
      const wires = set.map((member) => member.wire);
      expect(new Set(wires).size).toBe(wires.length);
      for (const member of set) expect(member.label.length).toBeGreaterThan(0);
    }
  });

  it('gives every provider channel an operation, including project familiarity', () => {
    for (const topic of AllChannelTopics) {
      expect(ALL_FAULT_OPERATIONS).toContain(faultOperationForTopic(topic));
    }
    expect(faultOperationForTopic(schema.ChannelTopic.ProjectFamiliarity)).toBe(FaultOperation.ProjectFamiliarity);
    expect(faultOperationForTopic(schema.ChannelTopic.Quality)).toBe(FaultOperation.QualityPage);
  });

  it('is client-only: the schema package exports neither WebFault nor the closed sets', () => {
    expect('WebFault' in schema).toBe(false);
    expect('FaultOperation' in schema).toBe(false);
    expect('FaultCause' in schema).toBe(false);
    expect('FaultRemedy' in schema).toBe(false);
  });
});

describe('branded evidence', () => {
  it('has no kind string and narrows by instanceof', () => {
    const evidence = new DetailEvidence('a bounded detail');
    expect('kind' in evidence).toBe(false);
    expect(evidence).toBeInstanceOf(DetailEvidence);
    expect(evidence).not.toBeInstanceOf(TopicEvidence);
    expect(evidence).not.toBeInstanceOf(HttpEvidence);
    expect(evidence).not.toBeInstanceOf(PayloadEvidence);
  });

  it('caps every detail at 512 characters', () => {
    const long = 'x'.repeat(MAX_EVIDENCE_DETAIL + 200);
    expect(MAX_EVIDENCE_DETAIL).toBe(512);
    expect(new DetailEvidence(long).detail).toHaveLength(512);
    expect(new HttpEvidence(500, long).detail).toHaveLength(512);
    expect(new TopicEvidence(schema.ChannelTopic.Sessions, long).detail).toHaveLength(512);
    expect(boundedDetail('short')).toBe('short');
  });
});

describe('WebFault messages', () => {
  it('names the operation, measured bytes, limit, and remedy for an oversized frame', () => {
    const fault = WebFault.payloadTooLarge(FaultOperation.QualityPage, MAX_FRAME_BYTES + 1, MAX_FRAME_BYTES);
    expect(fault.cause).toBe(FaultCause.PayloadTooLarge);
    expect(fault.remedy).toBe(FaultRemedy.Retry);
    expect(fault.evidence).toBeInstanceOf(PayloadEvidence);
    expect(fault.message).toContain('the quality update was');
    expect(fault.message).toContain(`${formatByteSize(MAX_FRAME_BYTES + 1)}`);
    expect(fault.message).toContain('the app accepts up to 8 MiB');
    expect(fault.message).toContain('the page kept its last data');
    expect(fault.message).toContain('try again');
  });

  it('lower the page size for a sessions read that was too large', () => {
    const fault = WebFault.payloadTooLarge(FaultOperation.SessionsRead, 16 * 1024 * 1024, MAX_FRAME_BYTES);
    expect(fault.remedy).toBe(FaultRemedy.LowerPageSize);
    expect(fault.message).toContain('lower the sessions per page');
  });

  it('names the operation and a bounded schema path for a frame that did not match', () => {
    const fault = WebFault.frameInvalid(FaultOperation.SessionDetailRead, '/data/turns/0/toolCalls/0/namespace (invalid_type)');
    expect(fault.cause).toBe(FaultCause.FrameInvalid);
    expect(fault.remedy).toBe(FaultRemedy.UpdateApp);
    expect(fault.message).toContain('the session detail update did not match this app');
    expect(fault.message).toContain('/data/turns/0/toolCalls/0/namespace');
    expect(fault.message).toContain('update peasant, then try again');
  });

  it('names the topic and the server detail for a topic refusal', () => {
    const fault = WebFault.topicError(
      FaultOperation.SessionsRead,
      schema.ChannelTopic.Sessions,
      'the store is locked',
      DiscoveryErrorCode.SelectionVisibility,
    );
    expect(fault.cause).toBe(FaultCause.TopicError);
    expect(fault.evidence).toBeInstanceOf(TopicEvidence);
    expect(fault.message).toContain('the server refused the sessions read: the store is locked');
    expect(fault.message).toContain('try again');
    expect(codeFromFault(fault)).toBe(DiscoveryErrorCode.SelectionVisibility);
  });

  it('carries the status and the REST code on HttpEvidence without rendering the code', () => {
    const fault = WebFault.httpError(
      FaultOperation.QualityPage,
      500,
      'the store is unavailable',
      DiscoveryErrorCode.HomeParentNotFound,
    );
    expect(fault.cause).toBe(FaultCause.HttpError);
    expect(fault.evidence).toBeInstanceOf(HttpEvidence);
    expect((fault.evidence as HttpEvidence).code).toBe(DiscoveryErrorCode.HomeParentNotFound);
    expect(fault.message).toContain('the quality page request failed with status 500');
    expect(fault.message).not.toContain('home_parent_not_found');
  });

  it('gives the start command for a connection failure and the reconnect note for a close', () => {
    const failed = WebFault.connection(FaultCause.ConnectFailed, 'the socket reported an error');
    expect(failed.operation).toBe(FaultOperation.Socket);
    expect(failed.remedy).toBe(FaultRemedy.StartApp);
    expect(failed.message).toContain('peasant is not answering on this computer');
    expect(failed.message).toContain('start it with peasant web start, then try again');

    const closed = WebFault.connection(FaultCause.SocketClosed, 'the socket closed');
    expect(closed.message).toContain('the connection to peasant closed');
    expect(closed.message).toContain('it reconnects automatically');
  });

  it('builds the message once, when the fault is created', () => {
    const fault = WebFault.frameInvalid(FaultOperation.TrendsRead, '/data (invalid_type)');
    expect(Object.getOwnPropertyNames(fault)).toContain('message');
    expect(fault.message).toBe(fault.message);
  });

  it('never echoes payload bytes or transcript content', () => {
    const secret = '{"turns":[{"content":"a private transcript line"}]}';
    const fault = WebFault.frameInvalid(FaultOperation.SessionDetailRead, '/data/turns (invalid_type)');
    expect(fault.message).not.toContain(secret);
    expect(fault.message).not.toContain('private transcript line');
  });
});

describe('FaultStore', () => {
  it('keeps an unrelated operation and an unrelated session isolated', () => {
    const store = new FaultStore();
    const sessions = WebFault.topicError(FaultOperation.SessionsRead, schema.ChannelTopic.Sessions, 'locked');
    const sessionA = WebFault.topicError(FaultOperation.SessionDetailRead, schema.ChannelTopic.SessionDetail, 'a is unavailable');

    store.set(FaultOperation.SessionsRead, 'sessions', sessions);
    store.set(FaultOperation.SessionDetailRead, 'session_detail:session-a', sessionA);

    expect(store.get(FaultOperation.SessionsRead, 'sessions')).toBe(sessions);
    expect(store.get(FaultOperation.SessionDetailRead, 'session_detail:session-b')).toBeNull();
    expect(store.get(FaultOperation.DashboardRead, 'sessions')).toBeNull();

    expect(store.clear(FaultOperation.DashboardRead, 'sessions')).toBe(false);
    expect(store.get(FaultOperation.SessionsRead, 'sessions')).toBe(sessions);

    expect(store.clear(FaultOperation.SessionsRead, 'sessions')).toBe(true);
    expect(store.get(FaultOperation.SessionDetailRead, 'session_detail:session-a')).toBe(sessionA);
  });
});

describe('REST control flow', () => {
  it('keeps DiscoveryRequestError as the control-flow type and copies its code onto HttpEvidence', () => {
    const error = new DiscoveryRequestError(
      '/api/v1/sync/sessions',
      400,
      'the page scope changed',
      DiscoveryErrorCode.SyncPageScopeChanged,
    );
    expect(error.code).toBe(DiscoveryErrorCode.SyncPageScopeChanged);

    const fault = faultFromDiscoveryError(FaultOperation.SessionsRead, error);
    expect(fault).not.toBeNull();
    expect(fault?.evidence).toBeInstanceOf(HttpEvidence);
    expect((fault?.evidence as HttpEvidence).code).toBe(DiscoveryErrorCode.SyncPageScopeChanged);
    expect(codeFromFault(fault)).toBe(DiscoveryErrorCode.SyncPageScopeChanged);
    expect(fault?.message).toContain('the sessions page request failed with status 400');
    expect(fault?.message).not.toContain('sync_page_scope_changed');
  });
});

describe('wire peek', () => {
  it('reads the type from a frame the server encoded with type first', () => {
    expect(peekMessageType('{"type":"quality","data":{}}')).toBe('quality');
  });

  it('reads a type that follows the data on a frame under the size limit', () => {
    expect(peekMessageType('{"data":{"nested":true},"type":"session_detail"}')).toBe('session_detail');
  });

  it('does not scan an oversized frame past its head', () => {
    const oversized = `{"data":"${'x'.repeat(MAX_FRAME_BYTES)}","type":"quality"}`;
    expect(peekMessageType(oversized)).toBeNull();
  });
});
