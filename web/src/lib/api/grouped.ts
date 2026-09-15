/**
 * REST client for the opt-in grouped local session list, grouped search, and
 * the scoped helper-member operation.
 *
 * The wire shapes come from the generated `@peasant-labs/schema` contract.
 * Decoding validates the closed sets (item kind, purpose, owner status) so a
 * grouped response that names an unknown value stops the surface instead of
 * being rendered as an approximating fold.
 *
 * The grouped list is the SAME authorized/selected route set the flat route
 * serves; the member operation replays that route predicate from an opaque
 * short-lived scope. A scope that is missing, expired or no longer matches is
 * refused with an actionable 409 (`group_scope_expired`) and must refresh the
 * originating list — it is never broadened to all helpers.
 */

import {
  isSessionListItemKind,
  zLocalHelperMembersPayload,
  zLocalSessionListPayload,
  type LocalHelperMembersPayload,
  type LocalSessionListItem,
  type LocalSessionListPayload,
  type LocalSessionRow,
} from '@peasant-labs/schema';
import { getApiBaseUrl } from './base';
import { parseDiscoveryError } from './errors';

export type { LocalHelperMembersPayload, LocalSessionListItem, LocalSessionListPayload, LocalSessionRow };

/** A member row is the same typed item the grouped list renders. */
export type GroupedMemberRow = LocalSessionListItem;

/** The originating route a grouped response was produced from. */
export type GroupedLocalVariant = 'sessions' | 'search';

/** Error code the server returns when a member scope can no longer be replayed. */
export const GROUP_SCOPE_EXPIRED_CODE = 'group_scope_expired';

/**
 * The typed refusal for a member scope that is missing, expired, restarted,
 * group-mismatched, selection-changed, or no longer matching the originating
 * predicate. The only safe recovery is to refresh the originating list; the
 * member set is never widened to stand in for it.
 */
export class GroupScopeExpiredError extends Error {
  readonly groupId: string;
  readonly path: string;
  readonly status = 409;
  readonly code = GROUP_SCOPE_EXPIRED_CODE;

  constructor(path: string, groupId: string, message: string) {
    super(message);
    this.name = 'GroupScopeExpiredError';
    this.groupId = groupId;
    this.path = path;
  }
}

export function isGroupScopeExpired(error: unknown): error is GroupScopeExpiredError {
  return error instanceof GroupScopeExpiredError;
}

function actionableDecodeError(operation: string, path: string, reason: string): string {
  return `The grouped local response from GET ${path} could not be decoded because ${reason} in ${operation} after the Peasant API returned it. This surface has stopped rather than render a list whose groups, counts or members cannot be trusted. Confirm the Peasant server and @peasant-labs/schema contract versions match, then reload the surface.`;
}

/** Decode a grouped local list/search payload against the generated contract. */
export function decodeGroupedLocalList(
  body: unknown,
  path = '/api/v1/sessions?view=grouped',
): LocalSessionListPayload {
  const parsed = zLocalSessionListPayload.safeParse(body);
  if (!parsed.success) {
    const detail = parsed.error.issues
      .map((issue) => `${issue.path.join('.') || '<root>'}: ${issue.message}`)
      .join('; ');
    throw new Error(actionableDecodeError('decodeGroupedLocalList', path, detail));
  }
  for (const [index, item] of parsed.data.items.entries()) {
    if (!isSessionListItemKind(item.kind)) {
      throw new Error(
        actionableDecodeError(
          'decodeGroupedLocalList',
          path,
          `items[${index}].kind names unknown list item kind ${JSON.stringify(item.kind)}`,
        ),
      );
    }
  }
  return parsed.data;
}

/** Decode one scoped helper-member page against the generated contract. */
export function decodeGroupedMembers(
  body: unknown,
  path = '/api/v1/session-groups/{groupId}/members',
): LocalHelperMembersPayload {
  const parsed = zLocalHelperMembersPayload.safeParse(body);
  if (!parsed.success) {
    const detail = parsed.error.issues
      .map((issue) => `${issue.path.join('.') || '<root>'}: ${issue.message}`)
      .join('; ');
    throw new Error(actionableDecodeError('decodeGroupedMembers', path, detail));
  }
  return parsed.data;
}

/** GET /api/v1/sessions?view=grouped */
export async function fetchGroupedLocalSessions(): Promise<LocalSessionListPayload> {
  const path = '/api/v1/sessions?view=grouped';
  const response = await fetch(`${getApiBaseUrl()}${path}`);
  if (!response.ok) {
    const body = await response.text().catch(() => '');
    throw parseDiscoveryError(path, response.status, body);
  }
  return decodeGroupedLocalList(await response.json(), path);
}

/** GET /api/v1/search?q=...&view=grouped */
export async function fetchGroupedLocalSearch(
  query: string,
  limit?: number,
): Promise<LocalSessionListPayload> {
  const params = new URLSearchParams({ q: query, view: 'grouped' });
  if (limit) params.set('limit', String(limit));
  const path = `/api/v1/search?${params.toString()}`;
  const response = await fetch(`${getApiBaseUrl()}${path}`);
  if (!response.ok) {
    const body = await response.text().catch(() => '');
    throw parseDiscoveryError(path, response.status, body);
  }
  return decodeGroupedLocalList(await response.json(), path);
}

/**
 * GET /api/v1/session-groups/{groupId}/members?scope=...&page=...&limit=...
 *
 * Only the opaque scope and paging are sent: the prefix route already fixed
 * every search/project/selection filter, so an extra narrowing or widening
 * filter would be refused by the server rather than guessed here.
 */
export async function fetchHelperGroupMembers(args: {
  groupId: string;
  scope: string;
  page?: number;
  limit?: number;
}): Promise<LocalHelperMembersPayload> {
  const params = new URLSearchParams({ scope: args.scope });
  if (args.page !== undefined) params.set('page', String(args.page));
  if (args.limit !== undefined) params.set('limit', String(args.limit));
  const path = `/api/v1/session-groups/${encodeURIComponent(args.groupId)}/members?${params.toString()}`;
  const response = await fetch(`${getApiBaseUrl()}${path}`);
  if (!response.ok) {
    const body = await response.text().catch(() => '');
    if (response.status === 409) {
      const code = errorCode(body);
      if (code === GROUP_SCOPE_EXPIRED_CODE) {
        throw new GroupScopeExpiredError(
          path,
          args.groupId,
          `The saved helpers for group ${args.groupId} could not be expanded because their member scope expired or no longer matches the originating list in fetchHelperGroupMembers. No members were loaded and the request was not widened to every helper. Refresh the originating grouped list and expand the group again.`,
        );
      }
    }
    throw parseDiscoveryError(path, response.status, body);
  }
  return decodeGroupedMembers(await response.json(), path);
}

function errorCode(body: string): string | undefined {
  try {
    const envelope = JSON.parse(body) as { code?: unknown };
    return typeof envelope.code === 'string' ? envelope.code : undefined;
  } catch {
    return undefined;
  }
}
