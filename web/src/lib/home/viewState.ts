import { closed } from '@peasant-labs/fairtrade/closed-value';
import {
  isPageSize,
  newProjectHash,
  PageSize,
  type PageSize as PageSizeValue,
  type ProjectHash,
} from '@peasant-labs/schema';
import {
  ALL_HOME_GROUP_BYS,
  HomeGroupBy,
  parseHomeGroupBy,
} from '@/components/home/homeColumns';
import {
  isPublishFilter,
  PUBLISH_FILTER_LABELS,
  PUBLISH_FILTERS,
  type PublishFilter,
} from './publishState';

/**
 * What Home lists. `sessions` is the session table; the others are the retained
 * browse flows (the project picker, sessions with their helper threads, the flat
 * filterable list), reached from the same `browse by` menu.
 */
export const BrowseView = Object.freeze({
  Sessions: closed('sessions', 'sessions'),
  Projects: closed('projects', 'project'),
  Grouped: closed('grouped', 'helper threads'),
  Flat: closed('flat', 'every session'),
} as const);
export type BrowseView = (typeof BrowseView)[keyof typeof BrowseView];

export const ALL_BROWSE_VIEWS = Object.freeze([
  BrowseView.Sessions,
  BrowseView.Projects,
  BrowseView.Grouped,
  BrowseView.Flat,
] as const);

type MissingBrowseView = Exclude<BrowseView, (typeof ALL_BROWSE_VIEWS)[number]>;
const _allBrowseViews: MissingBrowseView extends never ? true : never = true;

export function parseBrowseView(raw: unknown): BrowseView {
  return ALL_BROWSE_VIEWS.find((member) => member.wire === raw) ?? BrowseView.Sessions;
}

/** The filters over the list, re-exported from the one generated-name alias. */
export { PUBLISH_FILTERS, PUBLISH_FILTER_LABELS, isPublishFilter };
export type { PublishFilter };

/**
 * The Home view the URL carries. It owns the data view (`browse`,
 * `publication`, `project`, `query`), the page (`pageSize`, `page`), and the
 * grouping (`groupBy`). It never carries `visibleColumns` or `open`: those are
 * device state in `localStorage`, so a copied URL reproduces the data view and
 * the grouping, not the sender's columns.
 */
export interface HomeViewState {
  readonly browse: BrowseView;
  readonly publication: PublishFilter;
  readonly project: ProjectHash | null;
  readonly query: string;
  readonly pageSize: PageSizeValue;
  readonly page: number;
  readonly groupBy: HomeGroupBy;
}

const DEFAULT_PAGE_SIZE: PageSizeValue = PageSize.P25;

function positiveInteger(raw: string | null): number {
  if (raw === null) return 1;
  const value = Number(raw);
  return Number.isSafeInteger(value) && value > 0 ? value : 1;
}

function projectHash(raw: string | null): ProjectHash | null {
  if (raw === null || !/^[0-9a-f]{64}$/.test(raw)) return null;
  try {
    return newProjectHash(raw);
  } catch {
    return null;
  }
}

/** Parse the URL boundary into typed values. Unknown or absent values take the declared defaults. */
export function decodeHomeViewState(search: string | URLSearchParams): HomeViewState {
  const params = typeof search === 'string'
    ? new URLSearchParams(search.startsWith('?') ? search.slice(1) : search)
    : search;
  const rawSize = params.get('perPage');
  const size = rawSize === null ? Number.NaN : Number(rawSize);
  const publication = params.get('publication') ?? '';
  return Object.freeze({
    browse: parseBrowseView(params.get('browse')),
    publication: isPublishFilter(publication) ? publication : 'all',
    project: projectHash(params.get('project')),
    query: params.get('q') ?? '',
    pageSize: isPageSize(size) ? size : DEFAULT_PAGE_SIZE,
    page: positiveInteger(params.get('page')),
    groupBy: parseHomeGroupBy(params.get('groupBy')),
  });
}

/** Serialize the view into a query string without the leading `?`. Defaults are omitted. */
export function encodeHomeViewState(state: HomeViewState): string {
  const params = new URLSearchParams();
  if (state.browse !== BrowseView.Sessions) params.set('browse', state.browse.wire);
  if (state.publication !== 'all') params.set('publication', state.publication);
  if (state.project !== null) params.set('project', state.project);
  if (state.query !== '') params.set('q', state.query);
  if (state.pageSize !== DEFAULT_PAGE_SIZE) params.set('perPage', String(state.pageSize));
  if (state.page !== 1) params.set('page', String(state.page));
  if (state.groupBy !== HomeGroupBy.Project) params.set('groupBy', state.groupBy.wire);
  return params.toString();
}

/**
 * The tab-cache identity: the account and the view scope, the page size, and
 * the page number. It deliberately omits the server revision; the cached page
 * value carries that, and a revision mismatch clears the cache.
 *
 * This is a `string` alias, not a branded type: it names the key's role in
 * signatures and adds no runtime identity, so two equal cache keys are equal
 * strings and no parser or constructor is needed.
 */
export type HomeCacheKey = string;

const KEY_SEPARATOR = '\u0000';

export function homeCacheKey(state: HomeViewState, account: string): HomeCacheKey {
  return [
    account,
    state.browse.wire,
    state.publication,
    state.project ?? '',
    state.query,
    String(state.pageSize),
    String(state.page),
  ].join(KEY_SEPARATOR);
}

/** The grouping options, named for the menu. */
export const HOME_GROUP_BYS = ALL_HOME_GROUP_BYS;
