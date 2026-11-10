import { closed } from '@peasant-labs/fairtrade/closed-value';

/**
 * The grouping the saved view applies to the Home session table. It is the one
 * saved-view field the URL carries; the visible columns and the open groups are
 * device state under `peasant.home` (see `deviceView.ts`).
 *
 * `project` is the host default, so an absent or unknown URL value takes it.
 * The members are closed values: the shared table keys grouping by `.wire` and
 * compares members nominally, so the host never compares a raw string.
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

/**
 * The table's column ids, in declaration order. The first column is the row
 * identity: the shared table always shows it, and its picker disables it. The
 * saved view stores these members; `deviceView.ts` persists only their wires.
 */
export const HomeColumn = Object.freeze({
  Session: closed('session', 'session'),
  Project: closed('project', 'project'),
  Turns: closed('turns', 'turns'),
  Duration: closed('duration', 'duration'),
  When: closed('when', 'when'),
  State: closed('state', 'status'),
} as const);
export type HomeColumn = (typeof HomeColumn)[keyof typeof HomeColumn];

export const ALL_HOME_COLUMNS = Object.freeze([
  HomeColumn.Session,
  HomeColumn.Project,
  HomeColumn.Turns,
  HomeColumn.Duration,
  HomeColumn.When,
  HomeColumn.State,
] as const);

export type HomeColumnWire = HomeColumn['wire'];

const HOME_COLUMN_WIRES: readonly HomeColumnWire[] = ALL_HOME_COLUMNS.map((column) => column.wire);

export function isHomeColumnWire(value: unknown): value is HomeColumnWire {
  return typeof value === 'string' && (HOME_COLUMN_WIRES as readonly string[]).includes(value);
}

/** Resolve a stored wire back to its closed-set member, or null when unknown. */
export function parseHomeColumn(value: unknown): HomeColumn | null {
  return ALL_HOME_COLUMNS.find((column) => column.wire === value) ?? null;
}

/**
 * The one saved view the Home table receives: the URL-owned grouping composed
 * with the device-owned columns and open groups. The table is controlled; the
 * host owns the URL and the storage.
 */
export interface HomeSavedView {
  readonly groupBy: HomeGroupBy;
  readonly visibleColumns: readonly HomeColumn[];
  /** Qualified collapsed group ids; an empty list means nothing is collapsed. */
  readonly collapsed: readonly string[];
}
