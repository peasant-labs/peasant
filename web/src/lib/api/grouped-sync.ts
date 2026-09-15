/**
 * Decoders for the /share chooser's grouped sync list and its scoped helper
 * member pages.
 *
 * The wire shapes are owned by the generated `@peasant-labs/schema` contract.
 * Both responses are validated against the canonical payload schemas at the
 * trust boundary, so a malformed list or member page (an unknown item arm, a
 * member without its transcript, a missing page/limit/total) stops the chooser
 * with an actionable error instead of being rendered as a guessed row or a
 * fabricated count.
 */

import {
  zLocalHelperMembersPayload,
  zLocalSessionListPayload,
  type LocalHelperMembersPayload,
  type LocalSessionListPayload,
} from '@peasant-labs/schema';

/** One issue rendered from a schema validation failure. */
function describeIssues(issues: readonly { path: PropertyKey[]; message: string }[]): string {
  return issues
    .slice(0, 4)
    .map((issue) => `${issue.path.map((segment) => String(segment)).join('.') || '<root>'}: ${issue.message}`)
    .join('; ');
}

function actionableDecodeError(operation: string, path: string, reason: string): string {
  return `The share chooser could not read the grouped sessions response from GET ${path} because ${reason} in ${operation} after the Peasant API returned it. No rows or counts were shown, because a guessed list could offer a session the server did not authorize or misstate the chooser totals. Confirm the Peasant server and @peasant-labs/schema contract versions match, then reload the share chooser.`;
}

/**
 * Decode the grouped sync list (GET /api/v1/sync/sessions?view=grouped) against
 * the canonical contract. A body that names no valid item arm, drops a required
 * count or carries an unknown closed-set value is refused rather than folded
 * into an empty or complete-looking group.
 */
export function decodeGroupedSyncList(
  body: unknown,
  path = '/api/v1/sync/sessions?view=grouped',
): LocalSessionListPayload {
  const parsed = zLocalSessionListPayload.safeParse(body);
  if (!parsed.success) {
    throw new Error(actionableDecodeError('decodeGroupedSyncList', path, describeIssues(parsed.error.issues)));
  }
  return parsed.data;
}

/**
 * Decode one scoped helper member page
 * (GET /api/v1/session-groups/{groupId}/members) against the canonical
 * contract. Paging values are required by the contract, so a page that omits
 * them fails here instead of receiving a fabricated page or total.
 */
export function decodeHelperMembers(
  body: unknown,
  path = '/api/v1/session-groups/{groupId}/members',
): LocalHelperMembersPayload {
  const parsed = zLocalHelperMembersPayload.safeParse(body);
  if (!parsed.success) {
    throw new Error(actionableDecodeError('decodeHelperMembers', path, describeIssues(parsed.error.issues)));
  }
  return parsed.data;
}
