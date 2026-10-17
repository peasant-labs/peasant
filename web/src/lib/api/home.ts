import { zLocalHomeSessionsPage } from '@peasant-labs/schema';
import type { paths } from '@peasant-labs/schema/local-api';
import { getApiBaseUrl } from './base';
import { parseDiscoveryError } from './errors';

export type { LocalHomeSessionRow, LocalHomeSessionsPage } from '@peasant-labs/schema';
export type LocalHomeSessionsQuery = NonNullable<paths['/api/v1/home/sessions']['get']['parameters']['query']>;

/** Read one server-selected Home page, never the full sync library. */
export async function fetchHomeSessions(query: LocalHomeSessionsQuery, signal: AbortSignal) {
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(query)) {
    if (value !== undefined) params.set(key, String(value));
  }
  const path = '/api/v1/home/sessions';
  const response = await fetch(`${getApiBaseUrl()}${path}?${params}`, { signal });
  if (!response.ok) throw parseDiscoveryError(path, response.status, await response.text());
  const page = zLocalHomeSessionsPage.parse(await response.json());
  if (page.offset !== (query.offset ?? 0) || page.limit !== (query.limit ?? 25)
    || page.rows.length > page.limit || new Set(page.rows.map(row => row.sync.id)).size !== page.rows.length
    || (page.nextOffset !== undefined && (page.rows.length !== page.limit || page.nextOffset !== page.offset + page.limit))
    || page.rows.some(row => row.sync.id !== row.publication.sessionId || row.publication.outsideSelection)) {
    throw new Error('Home received inconsistent page bounds or session identities from /api/v1/home/sessions. Retry; if this persists, update the server and schema together.');
  }
  return page;
}
