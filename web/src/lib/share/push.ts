/**
 * The publish request both publish surfaces send to POST /api/v1/sync/push,
 * which runs the same pipeline as `peasant village push` (redact, upload, then
 * the collective steps) and returns one result per session.
 *
 * The request is typed and closed: the sessions, the redaction level, and the
 * collectives to add and remove. It carries no visibility and no license; a
 * first publication opens private, and an update keeps the audience the
 * transcript has unless the request names a change. The answer is decoded
 * against the schema contract in `publishSessions`.
 *
 * Honest failure: when the user isn't signed in, the server returns 401 with
 * "not authenticated — run 'peasant village login' first", surfaced verbatim.
 */

import type { SyncPushRequest, SyncPushResponse } from '@peasant-labs/schema';
import { publishSessions } from '@/lib/share/publishing';
import type { SelectableRedactionLevel } from '@/lib/share/redactions';

/** A change of audience, by collective id. Empty lists change nothing. */
export interface CollectiveChange {
  add: readonly string[];
  remove: readonly string[];
}

/**
 * The push body: the sessions and the redaction level, and the collective
 * change only when it names one, so a push without one keeps the audience.
 */
export function pushRequestBody(
  sessionIds: string[],
  redactionLevel: SelectableRedactionLevel,
  change?: CollectiveChange,
): SyncPushRequest {
  const body: SyncPushRequest = { sessionIds, redactionLevel };
  if (change && (change.add.length || change.remove.length)) {
    body.collectives = {
      ...(change.add.length ? { add: [...change.add] } : {}),
      ...(change.remove.length ? { remove: [...change.remove] } : {}),
    };
  }
  return body;
}

/**
 * Publish the given sessions at the given redaction level with no change of
 * audience (the multi-session wizard). Throws with the server's error message
 * (for example the "run 'peasant village login' first" 401).
 */
export function runPush(
  sessionIds: string[],
  // Narrowed to the levels this version offers. The endpoint answers 400 for the
  // other two, so accepting them here only moved the refusal to a point where the
  // user has already committed to publishing.
  redactionLevel: SelectableRedactionLevel,
): Promise<SyncPushResponse> {
  return publishSessions(pushRequestBody(sessionIds, redactionLevel));
}
