/**
 * Raw-preview frames built from the page fixture's synthetic streams.
 *
 * A stream names its session, its capture and its segments, each a list of
 * turns written as role and text. This file turns them into the wire frames the
 * server sends (`opened`, `segment`, `closed`), with the byte lengths, the
 * segment refs and the conversation positions the contract requires, so a case
 * states what the reader sees and not the arithmetic of the wire. It holds no
 * rule of the merge: a frame it builds is applied by the page with the
 * contract's own `mergeLocalKickstartPreview`, and a frame the contract
 * refuses fails the case.
 *
 * Every transcript here is synthetic.
 */

import {
  EntryType,
  Harness,
  LocalKickstartLineKind,
  LocalKickstartPreviewContinuation,
  LocalKickstartPreviewEventKind,
  MessageType,
  Role,
  isConversationTurn,
  type LocalKickstartPreviewCloseReason,
  type LocalKickstartPreviewCutReason,
} from '@peasant-labs/schema';

export interface StreamTurn {
  readonly role: typeof Role.User | typeof Role.Assistant;
  readonly text: string;
}

export interface StreamSpec {
  /** The setup session the capture belongs to. */
  readonly setupSession: string;
  /** The transcript's own session id (the header's id). */
  readonly sessionId: string;
  /** 22 base64url characters, as the server mints them. */
  readonly captureId: string;
  /** The capture revision, decimal. */
  readonly revision: string;
  readonly segments: readonly (readonly StreamTurn[])[];
}

type Json = Record<string, unknown>;

const START = Date.parse('2026-02-03T09:00:00Z');

/** One wire turn, its byte length the length of its own JSON without that member. */
function wireTurn(turn: StreamTurn, index: number): Json {
  const base: Json = {
    index,
    role: turn.role,
    entryType: EntryType.Text,
    content: turn.text,
    depth: 0,
    timestamp: new Date(START + index * 1000).toISOString(),
    truncated: false,
  };
  return { ...base, byteLength: new TextEncoder().encode(JSON.stringify(base)).length };
}

/** The turns of every segment, numbered across the whole stream. */
function numbered(spec: StreamSpec): Json[][] {
  let next = 0;
  return spec.segments.map((turns) => turns.map((turn) => wireTurn(turn, next++)));
}

/**
 * The capture's header as the server sends it: the session as far as it is
 * read, with the figures it never measures (duration, tokens, tool calls) at
 * the wire's required zero.
 */
function header(spec: StreamSpec, loadedTurns: number): Json {
  return {
    id: spec.sessionId,
    harness: Harness.ClaudeCode,
    startTime: new Date(START).toISOString(),
    endTime: new Date(START + Math.max(loadedTurns, 1) * 1000).toISOString(),
    durationMins: 0,
    tokensIn: 0,
    tokensOut: 0,
    totalTokens: 0,
    toolCallCount: 0,
    turnCount: loadedTurns,
    turns: [],
  };
}

function refs(segments: Json[][], count: number): Json[] {
  let before = 0;
  return segments.slice(0, count).map((turns, index) => {
    const conversation = turns.filter((turn) => isConversationTurn(turn as never)).length;
    const ref = {
      index,
      firstTurn: turns[0].index,
      lastTurn: turns.at(-1)!.index,
      byteLength: turns.reduce((sum, turn) => sum + (turn.byteLength as number), 0),
      conversationTurns: { before, count: conversation },
    };
    before += conversation;
    return ref;
  });
}

function directory(spec: StreamSpec, segments: Json[][], loaded: number, complete: boolean): Json {
  const held = segments.slice(0, loaded);
  const loadedTurns = held.reduce((sum, turns) => sum + turns.length, 0);
  const conversation = held.flat().filter((turn) => isConversationTurn(turn as never)).length;
  const d: Json = {
    sessionId: spec.sessionId,
    revision: spec.revision,
    complete,
    loadedSegments: loaded,
    loadedTurns,
    loadedConversationTurns: conversation,
    segments: refs(segments, loaded),
  };
  if (complete) {
    d.totalSegments = loaded;
    d.totalTurns = loadedTurns;
    d.totalConversationTurns = conversation;
  }
  return d;
}

/** The topic message the server writes, around one event. */
function message(event: Json): Json {
  return { type: MessageType.KickstartPreview, topic: 'kickstart', data: event };
}

export interface FrameRequest {
  /** `opened`: answers the open the page sent, with its request id. */
  readonly opened?: true;
  /**
   * `segment`: the segment of this index (or none, with `final` and no index),
   * with the directory after it.
   */
  readonly segment?: number | null;
  /** The frame ends the capture: the directory completes. */
  readonly final?: typeof LocalKickstartPreviewContinuation.End | typeof LocalKickstartPreviewContinuation.Cut;
  readonly cutReason?: LocalKickstartPreviewCutReason;
  /** The server's notice lines on a final frame. */
  readonly lines?: readonly string[];
  /** `closed`: the capture ends with this reason. */
  readonly closed?: LocalKickstartPreviewCloseReason;
  readonly code?: string;
  readonly message?: string;
  /** Answer a request id instead of the capture (a refusal of an open). */
  readonly refuse?: true;
  /** Send it as another capture's frame. */
  readonly otherCapture?: true;
}

/** Build one frame. `seq` and the request id come from the conversation so far. */
export function buildFrame(spec: StreamSpec, request: FrameRequest, at: { seq: number; requestId: string; loaded: number }): Json {
  const other = `${'Z'.repeat(21)}A`;
  const captureId = request.otherCapture ? (spec.captureId === other ? `${'Y'.repeat(21)}A` : other) : spec.captureId;
  const segments = numbered(spec);
  if (request.closed !== undefined) {
    const event: Json = { kind: LocalKickstartPreviewEventKind.Closed, sessionId: spec.setupSession, reason: request.closed };
    if (request.refuse) event.requestId = at.requestId;
    else { event.captureId = captureId; event.seq = at.seq; }
    if (request.refuse) event.seq = 0;
    if (request.code !== undefined) event.code = request.code;
    if (request.message !== undefined) event.message = request.message;
    return message(event);
  }
  if (request.opened) {
    return message({
      kind: LocalKickstartPreviewEventKind.Opened,
      sessionId: spec.setupSession,
      requestId: at.requestId,
      captureId,
      seq: 0,
      header: header(spec, 0),
      directory: directory(spec, segments, 0, false),
      continuation: LocalKickstartPreviewContinuation.More,
    });
  }
  const carries = request.segment !== undefined && request.segment !== null;
  const loaded = carries ? (request.segment as number) + 1 : at.loaded;
  const complete = request.final !== undefined;
  const loadedTurns = segments.slice(0, loaded).reduce((sum, turns) => sum + turns.length, 0);
  const event: Json = {
    kind: LocalKickstartPreviewEventKind.Segment,
    sessionId: spec.setupSession,
    captureId,
    seq: at.seq,
    header: header(spec, loadedTurns),
    directory: directory(spec, segments, loaded, complete),
    continuation: request.final ?? LocalKickstartPreviewContinuation.More,
  };
  if (carries) {
    const index = request.segment as number;
    const turns = segments[index];
    const ref = refs(segments, index + 1)[index];
    event.segment = {
      index,
      revision: spec.revision,
      firstTurn: ref.firstTurn,
      lastTurn: ref.lastTurn,
      byteLength: ref.byteLength,
      hasOlder: index > 0,
      hasNewer: !complete,
      turns,
    };
  }
  if (request.final === LocalKickstartPreviewContinuation.Cut) event.cutReason = request.cutReason;
  if (request.lines !== undefined) event.lines = request.lines.map((text) => ({ kind: LocalKickstartLineKind.Text, text }));
  return message(event);
}
