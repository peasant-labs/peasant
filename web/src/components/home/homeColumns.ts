import { closed } from '@peasant-labs/fairtrade/closed-value';

/**
 * The grouping the saved view applies to the Home session table. It is the one
 * saved-view field the URL carries; the visible columns and the open groups are
 * device state under `peasant.home` (see `deviceView.ts`).
 *
 * `project` is the host default, so an absent or unknown URL value takes it.
 */
export const HomeGroupBy = Object.freeze({
  Project: closed('project', 'project'),
  None: closed('none', 'none'),
} as const);
export type HomeGroupBy = (typeof HomeGroupBy)[keyof typeof HomeGroupBy];

export const ALL_HOME_GROUP_BYS = Object.freeze([
  HomeGroupBy.Project,
  HomeGroupBy.None,
] as const);

type MissingHomeGroupBy = Exclude<HomeGroupBy, (typeof ALL_HOME_GROUP_BYS)[number]>;
const _allHomeGroupBys: MissingHomeGroupBy extends never ? true : never = true;

export function parseHomeGroupBy(raw: unknown): HomeGroupBy {
  return ALL_HOME_GROUP_BYS.find((member) => member.wire === raw) ?? HomeGroupBy.Project;
}

/** The table's column ids. Order is the column order. */
export const HOME_COLUMN_IDS = ['session', 'project', 'turns', 'duration', 'when', 'state'] as const;
export type HomeColumnId = (typeof HOME_COLUMN_IDS)[number];

export function isHomeColumnId(value: unknown): value is HomeColumnId {
  return typeof value === 'string' && (HOME_COLUMN_IDS as readonly string[]).includes(value);
}

/**
 * The one saved view the Home table receives: the URL-owned grouping composed
 * with the device-owned columns and open groups. The table is controlled; the
 * host owns the URL and the storage.
 */
export interface HomeSavedView {
  readonly groupBy: HomeGroupBy;
  readonly visibleColumns: readonly HomeColumnId[];
  readonly open: readonly string[];
}
