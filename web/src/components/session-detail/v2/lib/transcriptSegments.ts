import { adaptTranscript, type AdaptTranscriptOptions } from '@peasant-labs/fairtrade/ui';
import { getApiBaseUrl } from '@/lib/api/base';
import { readLocalAnswer } from '@/lib/api/validated-local';
import type { SessionDetailPayload, TurnDetail } from '@/types/messages';

/**
 * The transcript document reads. The host asks for the directory, then one
 * segment, or a jump descriptor and exactly the segments it names. Nothing
 * here asks for the whole session or a window-sized body.
 */

const WINDOW_SPAN = 10;

export class TranscriptSegmentReadError extends Error {
  constructor(
    readonly path: string,
    readonly status: number,
    message: string,
  ) {
    super(message);
    this.name = 'TranscriptSegmentReadError';
  }
}

/** A segment or window read named a revision the store has already left. */
export class TranscriptRevisionConflict extends TranscriptSegmentReadError {
  constructor(path: string, message: string) {
    super(path, 409, message);
    this.name = 'TranscriptRevisionConflict';
  }
}

export type TranscriptSegmentRef = {
  index: number;
  firstTurn: number;
  lastTurn: number;
  byteLength: number;
};

export type TranscriptSegmentDirectory = {
  sessionId: string;
  revision: string;
  totalTurns: number;
  totalSegments: number;
  segments: TranscriptSegmentRef[];
};

export type TranscriptTurnWire = TurnDetail & {
  truncated: boolean;
  byteLength: number;
};

export type TranscriptSegment = {
  index: number;
  revision: string;
  firstTurn: number;
  lastTurn: number;
  byteLength: number;
  hasOlder: boolean;
  hasNewer: boolean;
  turns: TranscriptTurnWire[];
};

export type TranscriptWindow = {
  sessionId: string;
  revision: string;
  requestedAnchor: number;
  anchorTurn: number;
  span: number;
  firstTurn: number;
  lastTurn: number;
  segments: number[];
  hasOlder: boolean;
  hasNewer: boolean;
};

export type RegionCache = {
  sessionId: string;
  frameRevision: string;
  revision: string;
  directory: TranscriptSegmentDirectory;
  segments: Map<number, TranscriptSegment>;
  windows: Map<string, TranscriptWindow>;
  activeWindowKey: string | null;
};

export type CookedSegment = {
  index: number;
  revision: string;
  firstTurn: number;
  lastTurn: number;
  byteLength: number;
  hasOlder: boolean;
  hasNewer: boolean;
  turns: ReturnType<typeof adaptTranscript>['turns'];
};

type FetchLike = (input: string, init?: RequestInit) => Promise<Response>;

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function fail(path: string, status: number, detail: string): never {
  throw new TranscriptSegmentReadError(
    path,
    status,
    `peasant's transcript read ${path} could not be used: ${detail}. No segment was added to the open transcript. Retry the page; if it repeats, the local server is serving a transcript shape this page does not know.`,
  );
}

function decimalRevision(value: unknown, path: string, field: string): string {
  if (typeof value !== 'string' || !/^[0-9]+$/.test(value)) {
    fail(path, 200, `${field} is ${JSON.stringify(value)}, not a decimal store revision`);
  }
  return value;
}

function safeCount(value: unknown, path: string, field: string): number {
  if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < 0) {
    fail(path, 200, `${field} is ${JSON.stringify(value)}, not a non-negative safe integer`);
  }
  return value;
}

function parseRef(value: unknown, path: string, position: number): TranscriptSegmentRef {
  if (!isRecord(value)) fail(path, 200, `segments[${position}] is not an object`);
  const index = safeCount(value.index, path, `segments[${position}].index`);
  const firstTurn = safeCount(value.firstTurn, path, `segments[${position}].firstTurn`);
  const lastTurn = safeCount(value.lastTurn, path, `segments[${position}].lastTurn`);
  const byteLength = safeCount(value.byteLength, path, `segments[${position}].byteLength`);
  if (lastTurn < firstTurn) {
    fail(path, 200, `segments[${position}] declares turn range [${firstTurn}, ${lastTurn}]`);
  }
  return { index, firstTurn, lastTurn, byteLength };
}

export function parseTranscriptDirectory(value: unknown, path: string): TranscriptSegmentDirectory {
  if (!isRecord(value)) fail(path, 200, 'the directory is not an object');
  if (typeof value.sessionId !== 'string' || value.sessionId.length === 0) {
    fail(path, 200, 'sessionId is missing');
  }
  const revision = decimalRevision(value.revision, path, 'revision');
  const totalTurns = safeCount(value.totalTurns, path, 'totalTurns');
  const totalSegments = safeCount(value.totalSegments, path, 'totalSegments');
  if (!Array.isArray(value.segments)) fail(path, 200, 'segments is not an array');
  const segments = value.segments.map((ref, position) => parseRef(ref, path, position));
  const directory: TranscriptSegmentDirectory = {
    sessionId: value.sessionId,
    revision,
    totalTurns,
    totalSegments,
    segments,
  };
  return directory;
}

function parseTurn(value: unknown, path: string, position: number): TranscriptTurnWire {
  if (!isRecord(value)) fail(path, 200, `turns[${position}] is not an object`);
  const index = safeCount(value.index, path, `turns[${position}].index`);
  if (typeof value.role !== 'string' || value.role.length === 0) {
    fail(path, 200, `turns[${position}].role is missing`);
  }
  if (typeof value.truncated !== 'boolean') {
    fail(path, 200, `turns[${position}].truncated is not a boolean`);
  }
  const byteLength = safeCount(value.byteLength, path, `turns[${position}].byteLength`);
  return { ...(value as unknown as TurnDetail), index, truncated: value.truncated, byteLength };
}

export function parseTranscriptSegment(value: unknown, path: string): TranscriptSegment {
  if (!isRecord(value)) fail(path, 200, 'the segment is not an object');
  const index = safeCount(value.index, path, 'index');
  const revision = decimalRevision(value.revision, path, 'revision');
  const firstTurn = safeCount(value.firstTurn, path, 'firstTurn');
  const lastTurn = safeCount(value.lastTurn, path, 'lastTurn');
  const byteLength = safeCount(value.byteLength, path, 'byteLength');
  if (typeof value.hasOlder !== 'boolean' || typeof value.hasNewer !== 'boolean') {
    fail(path, 200, 'hasOlder and hasNewer must be booleans');
  }
  if (!Array.isArray(value.turns) || value.turns.length === 0) {
    fail(path, 200, 'turns is empty; a segment always holds at least one whole turn');
  }
  if ('content' in value || value.turns == null) {
    fail(path, 200, 'the segment carries content outside its turns');
  }
  const turns = value.turns.map((turn, position) => parseTurn(turn, path, position));
  if (turns[0]?.index !== firstTurn || turns[turns.length - 1]?.index !== lastTurn) {
    fail(path, 200, `declared range [${firstTurn}, ${lastTurn}] does not match the turns served`);
  }
  return {
    index,
    revision,
    firstTurn,
    lastTurn,
    byteLength,
    hasOlder: value.hasOlder,
    hasNewer: value.hasNewer,
    turns,
  };
}

export function parseTranscriptWindow(value: unknown, path: string): TranscriptWindow {
  if (!isRecord(value)) fail(path, 200, 'the window descriptor is not an object');
  for (const forbidden of ['turns', 'content', 'toolCalls']) {
    if (forbidden in value) {
      fail(path, 200, `the window descriptor carries ${forbidden}; a jump must not return turn content`);
    }
  }
  if (typeof value.sessionId !== 'string' || value.sessionId.length === 0) {
    fail(path, 200, 'sessionId is missing');
  }
  const revision = decimalRevision(value.revision, path, 'revision');
  const requestedAnchor = safeCount(value.requestedAnchor, path, 'requestedAnchor');
  const anchorTurn = safeCount(value.anchorTurn, path, 'anchorTurn');
  const span = safeCount(value.span, path, 'span');
  const firstTurn = safeCount(value.firstTurn, path, 'firstTurn');
  const lastTurn = safeCount(value.lastTurn, path, 'lastTurn');
  if (span < 1 || span > WINDOW_SPAN) fail(path, 200, `span ${span} is outside 1..${WINDOW_SPAN}`);
  if (anchorTurn < firstTurn || anchorTurn > lastTurn) {
    fail(path, 200, `anchorTurn ${anchorTurn} lies outside [${firstTurn}, ${lastTurn}]`);
  }
  if (!Array.isArray(value.segments) || value.segments.length === 0) {
    fail(path, 200, 'segments is empty; a window names the covering segment run');
  }
  const segments = value.segments.map((index, position) => safeCount(index, path, `segments[${position}]`));
  for (let i = 1; i < segments.length; i += 1) {
    if (segments[i] !== segments[i - 1]! + 1) {
      fail(path, 200, `segments ${segments.join(',')} are not a contiguous run`);
    }
  }
  if (typeof value.hasOlder !== 'boolean' || typeof value.hasNewer !== 'boolean') {
    fail(path, 200, 'hasOlder and hasNewer must be booleans');
  }
  return {
    sessionId: value.sessionId,
    revision,
    requestedAnchor,
    anchorTurn,
    span,
    firstTurn,
    lastTurn,
    segments,
    hasOlder: value.hasOlder,
    hasNewer: value.hasNewer,
  };
}

export function windowCacheKey(anchor: number, span: number = WINDOW_SPAN): string {
  return `${anchor}:${span}`;
}

/** A turn is known only when a ref this read holds covers it. */
export function heldRefCoversTurn(refs: readonly TranscriptSegmentRef[], turn: number): boolean {
  return refs.some((ref) => turn >= ref.firstTurn && turn <= ref.lastTurn);
}

function refusalMessage(path: string, status: number, body: unknown): string {
  const reason = isRecord(body) && typeof body.error === 'string' && body.error.length > 0
    ? body.error
    : 'the local server gave no reason';
  return `${path} was refused (HTTP ${status}): ${reason}. No transcript segment was added. Read the directory again and retry.`;
}

async function readTranscript(path: string, signal?: AbortSignal): Promise<unknown> {
  let answer;
  try {
    answer = await readLocalAnswer(path, { signal });
  } catch (error) {
    if (signal?.aborted) throw error;
    throw new TranscriptSegmentReadError(
      path,
      0,
      error instanceof Error ? error.message : `peasant did not answer ${path}. Check that the local server is running, then retry.`,
    );
  }
  if (answer.status === 409) {
    throw new TranscriptRevisionConflict(path, refusalMessage(path, answer.status, answer.body));
  }
  if (!answer.ok) {
    throw new TranscriptSegmentReadError(path, answer.status, refusalMessage(path, answer.status, answer.body));
  }
  return answer.body;
}

export type TranscriptSegmentTransport = {
  directory(sessionId: string, signal?: AbortSignal): Promise<TranscriptSegmentDirectory>;
  segment(sessionId: string, index: number, revision: string, signal?: AbortSignal): Promise<TranscriptSegment>;
  window(sessionId: string, anchor: number, span: number, revision: string, signal?: AbortSignal): Promise<TranscriptWindow>;
};

export function transcriptDirectoryPath(sessionId: string): string {
  return `/api/v1/sessions/${encodeURIComponent(sessionId)}/transcript/segments`;
}

export function transcriptSegmentPath(sessionId: string, index: number, revision: string): string {
  const query = new URLSearchParams({ revision });
  return `/api/v1/sessions/${encodeURIComponent(sessionId)}/transcript/segments/${index}?${query}`;
}

export function transcriptWindowPath(sessionId: string, anchor: number, span: number, revision: string): string {
  const query = new URLSearchParams({
    anchor: String(anchor),
    span: String(span),
    revision,
  });
  return `/api/v1/sessions/${encodeURIComponent(sessionId)}/transcript/window?${query}`;
}

export function createTranscriptSegmentTransport(fetchImpl?: FetchLike): TranscriptSegmentTransport {
  const bound = fetchImpl ?? ((input: string, init?: RequestInit) => fetch(input, init));
  const read = async (path: string, signal?: AbortSignal) => {
    if (fetchImpl) {
      const response = await bound(`${getApiBaseUrl()}${path}`, { cache: 'no-store', signal });
      const body: unknown = await response.json();
      if (response.status === 409) throw new TranscriptRevisionConflict(path, refusalMessage(path, response.status, body));
      if (!response.ok) throw new TranscriptSegmentReadError(path, response.status, refusalMessage(path, response.status, body));
      return body;
    }
    return readTranscript(path, signal);
  };
  return {
    async directory(sessionId, signal) {
      const path = transcriptDirectoryPath(sessionId);
      return parseTranscriptDirectory(await read(path, signal), path);
    },
    async segment(sessionId, index, revision, signal) {
      const path = transcriptSegmentPath(sessionId, index, revision);
      const segment = parseTranscriptSegment(await read(path, signal), path);
      if (segment.revision !== revision) {
        throw new TranscriptRevisionConflict(
          path,
          `${path} answered revision ${segment.revision} after the page asked for ${revision}. The cut moved. Read the directory again and retry.`,
        );
      }
      return segment;
    },
    async window(sessionId, anchor, span, revision, signal) {
      const path = transcriptWindowPath(sessionId, anchor, span, revision);
      const descriptor = parseTranscriptWindow(await read(path, signal), path);
      if (descriptor.revision !== revision) {
        throw new TranscriptRevisionConflict(
          path,
          `${path} answered revision ${descriptor.revision} after the page asked for ${revision}. The cut moved. Read the directory again and retry.`,
        );
      }
      return descriptor;
    },
  };
}

export const transcriptSegments = createTranscriptSegmentTransport();

type CookableDetail = SessionDetailPayload & {
  title?: string;
  medians?: unknown;
  revision?: string;
  teammate?: unknown;
  relationshipNavigation?: AdaptTranscriptOptions['relationshipNavigation'];
  gitContext?: unknown;
};

/**
 * Cook one loaded segment's wire turns with the same adapter the whole-session
 * view uses. Session chrome stays on the header view model; this only produces
 * the turns the viewer is allowed to mount. Read-only fields and the session's
 * other partitions stay off this payload so a partial segment is not refused
 * for turns it was never given.
 */
export function cookSegment(
  detail: CookableDetail,
  segment: TranscriptSegment,
  options: AdaptTranscriptOptions = {},
): CookedSegment {
  const {
    turns: _frameTurns,
    relationships: _relationships,
    earlierHistory: _earlierHistory,
    nativeMetadata: _nativeMetadata,
    title: _title,
    medians: _medians,
    revision: _revision,
    teammate: _teammate,
    relationshipNavigation: _navigation,
    gitContext: _gitContext,
    ...session
  } = detail;
  const adapted = adaptTranscript(
    { ...session, turns: segment.turns },
    undefined,
    undefined,
    options,
  );
  const facts = new Map(segment.turns.map((turn) => [turn.index, turn]));
  return {
    index: segment.index,
    revision: segment.revision,
    firstTurn: segment.firstTurn,
    lastTurn: segment.lastTurn,
    byteLength: segment.byteLength,
    hasOlder: segment.hasOlder,
    hasNewer: segment.hasNewer,
    turns: adapted.turns.map((turn) => {
      const wire = facts.get(turn.index);
      return {
        ...turn,
        segmentIndex: segment.index,
        ...(wire?.truncated ? { truncated: true as const } : {}),
        ...(wire ? { byteLength: wire.byteLength } : {}),
      };
    }),
  };
}

export const TRANSCRIPT_WINDOW_SPAN = WINDOW_SPAN;
