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
  type HelperGroupSummary,
  type LocalHelperMembersPayload,
  type LocalSessionListItem,
  type LocalSessionListPayload,
  type LocalSessionRow,
  type SearchResult,
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

/**
 * The originating route scope for a grouped session list.
 *
 * A project scope is sent to the SERVER as the route's existing `project`
 * filter, so the candidate set, the ordinary/helper counts and every issued
 * member scope are all limited to that project before the response is built.
 * The client never folds a global list into a project heading, because a
 * client-side fold would still show the server's cross-project totals and
 * could widen a member expansion past the project.
 */
export interface GroupedLocalSessionsScope {
  /** Opaque project hash; omit for the cross-project list. */
  projectHash?: string;
}

/** GET /api/v1/sessions?view=grouped[&project=<projectHash>] */
export async function fetchGroupedLocalSessions(
  scope: GroupedLocalSessionsScope = {},
): Promise<LocalSessionListPayload> {
  const params = new URLSearchParams({ view: 'grouped' });
  if (scope.projectHash) params.set('project', scope.projectHash);
  const path = `/api/v1/sessions?${params.toString()}`;
  const response = await fetch(`${getApiBaseUrl()}${path}`);
  if (!response.ok) {
    const body = await response.text().catch(() => '');
    throw parseDiscoveryError(path, response.status, body);
  }
  return decodeGroupedLocalList(await response.json(), path);
}

/**
 * Refuse a grouped response that claims one project but carries another
 * project's rows.
 *
 * The project filter is applied by the SERVER. A server that does not implement
 * the grouped project filter answers with the cross-project list; rendering
 * that under a project heading would show other projects' sessions and print
 * the cross-project counts. This stops the surface with an actionable error
 * instead, and never folds or re-sorts the rows on the client. A row without a
 * recorded project hash is not a contradiction and is allowed through.
 */
export function assertGroupedProjectScope(
  payload: LocalSessionListPayload,
  projectHash: string,
): void {
  for (const item of payload.items) {
    const row = item.transcript;
    if (!row) continue;
    const rowHash = row.session.projectHash;
    if (rowHash !== undefined && rowHash !== projectHash) {
      throw new Error(
        `The grouped list at /api/v1/sessions?view=grouped&project=${projectHash} included session ${row.session.id} from project ${rowHash} in assertGroupedProjectScope. No project-scoped list was rendered, because a response built without the grouped project filter carries other projects' sessions and their cross-project counts, and showing it under one project heading would misstate that project. Confirm the Peasant server applies the grouped project filter, then retry.`,
      );
    }
  }
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

/** The match rows a grouped search payload carries directly on its items. */
export function groupedSearchMatches(payload: LocalSessionListPayload): SearchResult[] {
  return payload.items.flatMap((item) => item.transcript?.matches ?? []);
}

/**
 * Members requested per member page while flattening search hits. This mirrors
 * the server's member page cap so one group needs as few round trips as
 * possible; paging still follows the server-reported total, so a group larger
 * than one page is never truncated.
 */
export const SEARCH_MEMBER_PAGE_LIMIT = 50;

/**
 * One navigable search hit's identity. A hit is unique per (session, entry), so
 * the same saved helper reached through two owners, or through a nested group
 * plus its own top-level item, becomes exactly one palette row.
 */
function searchMatchKey(match: SearchResult): string {
  return `${match.sessionId}:${match.entryIndex}`;
}

/**
 * Flatten a grouped search into the individual transcript-hit rows the command
 * palette navigates.
 *
 * Every item's own matches are included first, in the server's relevance
 * order. Then every saved helper group is expanded from its exact issued scope,
 * breadth-first through the NESTED groups the server returns under a member:
 * the server suppresses a group from the top level when its owner is also a
 * candidate, so a matching helper G2 owned by a matching helper G1 is reachable
 * only through G1's member payload. All required member pages are followed, and
 * an already-visited (group, scope) pair is never fetched twice, so a shared
 * owner or a cycle cannot loop or duplicate hits.
 *
 * An expired scope omits THAT group's helper hits only: the ordinary hits and
 * every other reachable group stay, and the query is never widened.
 */
export async function fetchGroupedSearchMatches(
  query: string,
  limit?: number,
): Promise<SearchResult[]> {
  const payload = await fetchGroupedLocalSearch(query, limit);
  const rows = groupedSearchMatches(payload);
  const seenMatches = new Set(rows.map(searchMatchKey));

  const visitedGroups = new Set<string>();
  const pending: HelperGroupSummary[] = [];
  for (const item of payload.items) {
    pending.push(...(item.helperGroups ?? []));
  }

  while (pending.length > 0) {
    const group = pending.shift();
    if (group === undefined) break;
    const groupKey = `${group.groupId}\u0000${group.memberScope}`;
    if (visitedGroups.has(groupKey)) continue;
    visitedGroups.add(groupKey);

    try {
      for (let page = 1; ; ) {
        const members = await fetchHelperGroupMembers({
          groupId: group.groupId,
          scope: group.memberScope,
          page,
          limit: SEARCH_MEMBER_PAGE_LIMIT,
        });
        for (const member of members.members) {
          for (const match of member.transcript?.matches ?? []) {
            const key = searchMatchKey(match);
            if (!seenMatches.has(key)) {
              seenMatches.add(key);
              rows.push(match);
            }
          }
          pending.push(...(member.helperGroups ?? []));
        }
        const servedThrough = members.page * members.limit;
        if (members.members.length === 0 || servedThrough >= members.total) break;
        page = members.page + 1;
      }
    } catch (cause) {
      if (!isGroupScopeExpired(cause)) throw cause;
    }
  }

  return rows;
}
