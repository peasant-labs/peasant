/**
 * The root page's session rows and their publish state, computed from two
 * answers the page already receives: the flat sync list and the publications
 * read. Nothing here re-derives a server rule. Each function compares fields
 * the server returned:
 *
 * - Visibility: the publications read decides, through the same selection
 *   projection the lists use, whether the saved selection leaves a session out
 *   (`outsideSelection`). A sync row without a publication row cannot be
 *   checked, so it is left out too.
 * - Published or not: the publication `state`.
 * - Auto: the publication `autoPublish` flag.
 * - New turns: a published session whose sync status is `updated`, meaning the
 *   server recorded turns after its last publish.
 */

import type { LocalPublication, LocalSyncSummary } from '@/lib/api/publications';

/** The filters over the list, in the order the page shows them. */
export const PUBLISH_FILTERS = ['all', 'not-published', 'published', 'auto'] as const;
export type PublishFilter = (typeof PUBLISH_FILTERS)[number];

/** The lowercase chrome label of each filter. */
export const PUBLISH_FILTER_LABELS: Readonly<Record<PublishFilter, string>> = {
  all: 'all',
  'not-published': 'not published',
  published: 'published',
  auto: 'auto',
};

export function isPublishFilter(value: string): value is PublishFilter {
  return (PUBLISH_FILTERS as readonly string[]).includes(value);
}

/** The fairtrade publish state label a row shows. */
export type RowPublishState = 'not-published' | 'published' | 'new-turns' | 'auto-publish';

/** One session the root list may show: its sync row and its publication row. */
export interface RootSessionRow {
  sync: LocalSyncSummary;
  publication: LocalPublication;
}

/**
 * The sessions the root list shows, in the sync list's order (newest first):
 * each sync row the publications read returned and did not mark outside the
 * saved selection.
 */
export function visibleRootRows(
  sync: readonly LocalSyncSummary[],
  publications: readonly LocalPublication[],
): RootSessionRow[] {
  const byId = new Map(publications.map((publication) => [publication.sessionId, publication]));
  const rows: RootSessionRow[] = [];
  for (const session of sync) {
    const publication = byId.get(session.id);
    if (!publication || publication.outsideSelection) continue;
    rows.push({ sync: session, publication });
  }
  return rows;
}

export function matchesPublishFilter(row: RootSessionRow, filter: PublishFilter): boolean {
  switch (filter) {
    case 'all':
      return true;
    case 'not-published':
      return row.publication.state === 'unpublished';
    case 'published':
      return row.publication.state === 'published';
    case 'auto':
      return row.publication.autoPublish;
  }
}

/** How many rows each filter lists. */
export function publishFilterCounts(rows: readonly RootSessionRow[]): Record<PublishFilter, number> {
  const counts: Record<PublishFilter, number> = { all: 0, 'not-published': 0, published: 0, auto: 0 };
  for (const row of rows) {
    for (const filter of PUBLISH_FILTERS) {
      if (matchesPublishFilter(row, filter)) counts[filter] += 1;
    }
  }
  return counts;
}

/**
 * The state a row's label shows. A published session says so first, because
 * who can read it is the fact that matters; a session that is not published
 * yet but will be on the next push says auto-publish is on.
 */
export function rowPublishState(row: RootSessionRow): RowPublishState {
  if (row.publication.state === 'published') {
    return row.sync.syncStatus === 'updated' ? 'new-turns' : 'published';
  }
  return row.publication.autoPublish ? 'auto-publish' : 'not-published';
}
