import { zLocalSyncSessionPage, zLocalSyncProjectOptions } from '@peasant-labs/schema';
import type { paths } from '@peasant-labs/schema/local-api';
import { getApiBaseUrl } from './base';
import { parseDiscoveryError } from './errors';

export type { LocalSyncSessionRow, LocalSyncSessionPage, LocalSyncProjectOption, PageToken } from '@peasant-labs/schema';
export type SyncSessionsQuery = NonNullable<paths['/api/v1/sync/sessions']['get']['parameters']['query']>;
export const SYNC_HOME_PATH = '/api/v1/sync/sessions';

/** Read one server-selected Home page, never the full sync library. */
export async function fetchHomeSessions(query: SyncSessionsQuery, signal: AbortSignal) {
  const params = new URLSearchParams({ view: 'home' });
  for (const [key, value] of Object.entries(query)) {
    if (value !== undefined) params.set(key, String(value));
  }
  const path = SYNC_HOME_PATH;
  const response = await fetch(`${getApiBaseUrl()}${path}?${params}`, { signal });
  if (!response.ok) throw parseDiscoveryError(path, response.status, await response.text());
  const page = zLocalSyncSessionPage.parse(await response.json());
  // `outsideSelection` is ingestion and push metadata. It never narrows what a
  // stored, already-indexed session may show here, so it is not checked.
  if (page.items.length > Math.min(query.maxItems || 25, 100)
    || new Set(page.items.map(row => row.sync.id)).size !== page.items.length
    || page.items.some(row => row.sync.id !== row.publication.sessionId)) {
    throw new Error('Home received inconsistent session identities or an oversized sync page. Retry; if this persists, update the server and schema together.');
  }
  return page;
}

/** Complete root-project metadata, deliberately independent of the current filter. */
export async function fetchHomeProjects(signal: AbortSignal) {
  const response = await fetch(`${getApiBaseUrl()}${SYNC_HOME_PATH}?view=project-options`, { signal });
  if (!response.ok) throw parseDiscoveryError(SYNC_HOME_PATH, response.status, await response.text());
  return zLocalSyncProjectOptions.parse(await response.json()).projects;
}
