/**
 * The Local API reads and writes the publish popup runs on:
 *
 *   GET  /api/v1/sync/auth             whether this computer is signed in to village
 *   POST /api/v1/sync/login            start the GitHub sign-in in the browser
 *   GET  /api/v1/publications          whether a session is published, and who can read it
 *   GET  /api/v1/village/collectives   the collectives the signed-in user can publish to
 *   POST /api/v1/sync/push             publish a session and change who can read it
 *
 * Every response is decoded against the published `@peasant-labs/schema`
 * contract at this boundary. A body the contract refuses stops the popup with
 * an actionable error; it is never read as a guess, because a guessed
 * publication state or audience would tell the developer something untrue about
 * who can read their work.
 */

import {
  zLocalPublicationsResponse,
  zLocalVillageCollectivesResponse,
  zSyncAuthResponse,
  zSyncLoginResponse,
  zSyncPushResponse,
  type LocalPublication,
  type LocalVillageCollective,
  type SyncAuthResponse,
  type SyncLoginResponse,
  type SyncPushRequest,
  type SyncPushResponse,
} from '@peasant-labs/schema';
import { getApiBaseUrl } from '@/lib/api/base';

/** The error codes of the publishing reads this client acts on. */
export const PublishingErrorCode = {
  /** This computer holds no valid village credential. */
  VillageSignedOut: 'village_signed_out',
  /** Village could not be read. */
  VillageUnreachable: 'village_unreachable',
  /** Village no longer holds the transcript this computer's receipt names. */
  VillageTranscriptMissing: 'village_transcript_missing',
} as const;

export type PublishingErrorCode = (typeof PublishingErrorCode)[keyof typeof PublishingErrorCode];

/** A publishing request the server refused, with its status and error code. */
export class PublishingRequestError extends Error {
  readonly status: number;
  readonly code?: string;

  constructor(message: string, status: number, code?: string) {
    super(message);
    this.name = 'PublishingRequestError';
    this.status = status;
    this.code = code;
  }
}

/** Whether an error is a refusal with the given code. */
export function isPublishingError(error: unknown, code: PublishingErrorCode): boolean {
  return error instanceof PublishingRequestError && error.code === code;
}

interface Decoder<T> {
  safeParse(value: unknown): { success: true; data: T } | { success: false; error: { issues: readonly { path: PropertyKey[]; message: string }[] } };
}

function describeIssues(issues: readonly { path: PropertyKey[]; message: string }[]): string {
  return issues
    .slice(0, 4)
    .map((issue) => `${issue.path.map((segment) => String(segment)).join('.') || '<root>'}: ${issue.message}`)
    .join('; ');
}

async function request<T>(method: 'GET' | 'POST', path: string, decoder: Decoder<T>, body?: unknown): Promise<T> {
  const response = await fetch(`${getApiBaseUrl()}${path}`, {
    method,
    headers: body === undefined ? undefined : { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await response.text().catch(() => '');
  let payload: unknown = undefined;
  try {
    payload = text ? JSON.parse(text) : undefined;
  } catch {
    payload = undefined;
  }
  if (!response.ok) {
    const envelope = (payload && typeof payload === 'object' ? payload : {}) as { error?: unknown; code?: unknown };
    const message = typeof envelope.error === 'string' && envelope.error.trim()
      ? envelope.error
      : text.trim() || `the server answered HTTP ${response.status} with no error details; retry, then check the Peasant server log`;
    throw new PublishingRequestError(message, response.status, typeof envelope.code === 'string' ? envelope.code : undefined);
  }
  const parsed = decoder.safeParse(payload);
  if (!parsed.success) {
    throw new PublishingRequestError(
      `${method} ${path} answered a body the Local API contract refuses (${describeIssues(parsed.error.issues)}), so it is not shown. Check that Peasant and its web assets are the same version, then reload the page.`,
      response.status,
    );
  }
  return parsed.data;
}

/** Whether this computer is signed in to village. */
export function fetchVillageAuth(): Promise<SyncAuthResponse> {
  return request('GET', '/api/v1/sync/auth', zSyncAuthResponse);
}

/**
 * Start the village sign-in. The server opens GitHub in the browser and waits
 * up to three minutes for it; the caller polls {@link fetchVillageAuth}.
 */
export function startVillageSignIn(): Promise<SyncLoginResponse> {
  return request('POST', '/api/v1/sync/login', zSyncLoginResponse);
}

/**
 * The publication state of one session for the signed-in account, or null when
 * this computer has not recorded the session. With `audience`, a published
 * session also names the collectives that can read it, which asks village.
 */
export async function fetchPublication(sessionId: string, options: { audience: boolean }): Promise<LocalPublication | null> {
  const params = new URLSearchParams({ sessionIds: sessionId });
  if (options.audience) params.set('include', 'audience');
  const response = await request('GET', `/api/v1/publications?${params.toString()}`, zLocalPublicationsResponse);
  return response.publications.find((publication) => publication.sessionId === sessionId) ?? null;
}

/**
 * The publication state of one session, with its audience when village can be
 * read. When village cannot be read, the state is still the local receipt, and
 * `audienceKnown` is false so the bar leaves the count out rather than state it.
 * When village answers that the transcript is gone, the refusal is kept: the
 * receipt would call the session published when it is not.
 */
export async function fetchPublicationState(sessionId: string): Promise<{ publication: LocalPublication | null; audienceKnown: boolean }> {
  try {
    return { publication: await fetchPublication(sessionId, { audience: true }), audienceKnown: true };
  } catch (error) {
    if (!isPublishingError(error, PublishingErrorCode.VillageUnreachable)) throw error;
    return { publication: await fetchPublication(sessionId, { audience: false }), audienceKnown: false };
  }
}

/**
 * The village collectives the signed-in user belongs to, with the ones that
 * link this session's repository or GitHub organization suggested. The server
 * compares the remotes; nothing here matches them.
 */
export async function fetchVillageCollectives(sessionId: string): Promise<LocalVillageCollective[]> {
  const params = new URLSearchParams({ sessionId });
  const response = await request('GET', `/api/v1/village/collectives?${params.toString()}`, zLocalVillageCollectivesResponse);
  return response.collectives;
}

/**
 * Publish sessions and change who can read them. The request is the typed,
 * closed contract: sessions, a redaction level, and the collectives to add and
 * remove. It carries no visibility and no license, because publishing from the
 * local web is for collectives; an update keeps the audience a transcript has
 * unless the request names a change.
 */
export function publishSessions(body: SyncPushRequest): Promise<SyncPushResponse> {
  return request('POST', '/api/v1/sync/push', zSyncPushResponse, body);
}
