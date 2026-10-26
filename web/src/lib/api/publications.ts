/**
 * REST client for the two reads the root page's session list is built from:
 * the flat sync list (GET /api/v1/sync/sessions) and the publication state of
 * named sessions (GET /api/v1/publications).
 *
 * Both answers are decoded against the generated `@peasant-labs/schema`
 * contract. A body the contract refuses stops the list with an actionable
 * error instead of being rendered as a guessed row or count.
 *
 * The flat sync list does not apply the saved selection; the publications read
 * reports, for every named session, whether the selection leaves it out
 * (`outsideSelection`). The root list shows only what both reads agree is
 * visible, so the saved selection is decided on the server and only compared
 * here.
 */

import {
  zLocalPublicationsResponse,
  zLocalSyncSessionsPayload,
  type LocalPublication,
  type LocalSyncSummary,
} from '@peasant-labs/schema';
import { getApiBaseUrl } from './base';
import { parseDiscoveryError } from './errors';

export type { LocalPublication, LocalSyncSummary };

export const SYNC_SESSIONS_PATH = '/api/v1/sync/sessions';
export const PUBLICATIONS_PATH = '/api/v1/publications';

/**
 * How many session IDs one publications request names. The IDs travel in the
 * query string, so a large install reads its publication state in batches
 * rather than in one URL of every ID.
 */
export const PUBLICATIONS_BATCH_SIZE = 100;

/** How many publications batches are in flight at once. */
const PUBLICATIONS_CONCURRENCY = 3;

function describeIssues(issues: readonly { path: PropertyKey[]; message: string }[]): string {
  return issues
    .slice(0, 4)
    .map((issue) => `${issue.path.map((segment) => String(segment)).join('.') || '<root>'}: ${issue.message}`)
    .join('; ');
}

function decodeError(path: string, operation: string, reason: string): Error {
  return new Error(
    `The session list could not read GET ${path} because ${reason} in ${operation}. No rows or counts were shown, because a guessed list could misstate what is published. Confirm the Peasant server and @peasant-labs/schema contract versions match, then reload the page.`,
  );
}

/**
 * GET `path` and return its JSON body. `route` names the request in an error:
 * the publications query string lists session IDs, including ones the saved
 * selection hides, so an error names the route and never the IDs.
 */
async function getBody(path: string, route = path): Promise<unknown> {
  const response = await fetch(`${getApiBaseUrl()}${path}`);
  if (!response.ok) {
    const body = await response.text().catch(() => '');
    throw parseDiscoveryError(route, response.status, body);
  }
  return response.json();
}

/** Every pushable session on this computer, newest first, with its sync status. */
export async function fetchSyncSessions(): Promise<LocalSyncSummary[]> {
  const body = await getBody(SYNC_SESSIONS_PATH);
  const parsed = zLocalSyncSessionsPayload.safeParse(body);
  if (!parsed.success) {
    throw decodeError(SYNC_SESSIONS_PATH, 'fetchSyncSessions', describeIssues(parsed.error.issues));
  }
  return parsed.data.sessions;
}

function publicationsPath(ids: readonly string[], audience: boolean): string {
  const params = new URLSearchParams({ sessionIds: ids.join(',') });
  if (audience) params.set('include', 'audience');
  return `${PUBLICATIONS_PATH}?${params.toString()}`;
}

async function fetchPublicationBatch(ids: readonly string[], audience: boolean): Promise<LocalPublication[]> {
  const body = await getBody(publicationsPath(ids, audience), PUBLICATIONS_PATH);
  const parsed = zLocalPublicationsResponse.safeParse(body);
  if (!parsed.success) {
    throw decodeError(PUBLICATIONS_PATH, 'fetchPublications', describeIssues(parsed.error.issues));
  }
  return parsed.data.publications;
}

/**
 * The publication state of the named sessions, read in batches. A session the
 * server does not know is left out of the answer. `audience` asks Village which
 * collectives can read each published transcript.
 */
export async function fetchPublications(
  ids: readonly string[],
  options: { audience?: boolean } = {},
): Promise<LocalPublication[]> {
  const unique = [...new Set(ids)];
  if (unique.length === 0) return [];
  const batches: string[][] = [];
  for (let start = 0; start < unique.length; start += PUBLICATIONS_BATCH_SIZE) {
    batches.push(unique.slice(start, start + PUBLICATIONS_BATCH_SIZE));
  }
  const results: LocalPublication[][] = new Array(batches.length);
  let next = 0;
  const worker = async () => {
    while (next < batches.length) {
      const index = next;
      next += 1;
      results[index] = await fetchPublicationBatch(batches[index], options.audience === true);
    }
  };
  await Promise.all(
    Array.from({ length: Math.min(PUBLICATIONS_CONCURRENCY, batches.length) }, () => worker()),
  );
  return results.flat();
}
