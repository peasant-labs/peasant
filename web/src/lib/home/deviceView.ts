import { HOME_COLUMN_IDS, isHomeColumnId, type HomeColumnId } from '@/components/home/homeColumns';

/**
 * The device half of the saved view: the visible columns and the open groups.
 * It lives in `localStorage` under `peasant.home`, never in the URL. A copied
 * URL therefore reproduces the data view and the grouping, not the sender's
 * columns.
 */
export interface HomeDeviceView {
  /** The columns the reader can see, in table order. `null` means the default set. */
  readonly visibleColumns: readonly HomeColumnId[] | null;
  /** The qualified group ids whose rows are open. */
  readonly open: readonly string[];
}

export const HOME_DEVICE_KEY = 'peasant.home';

const DEFAULT_DEVICE_VIEW: HomeDeviceView = Object.freeze({ visibleColumns: null, open: [] });

type ReadableStorage = Pick<Storage, 'getItem'>;
type WritableStorage = Pick<Storage, 'getItem' | 'setItem'>;

function storageOrNull(): Storage | null {
  return typeof window === 'undefined' ? null : window.localStorage;
}

/** Read the device view, discarding anything that is not the shape this app writes. */
export function readHomeDeviceView(storage: ReadableStorage | null = storageOrNull()): HomeDeviceView {
  if (storage === null) return DEFAULT_DEVICE_VIEW;
  const raw = storage.getItem(HOME_DEVICE_KEY);
  if (raw === null) return DEFAULT_DEVICE_VIEW;
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return DEFAULT_DEVICE_VIEW;
  }
  if (typeof parsed !== 'object' || parsed === null) return DEFAULT_DEVICE_VIEW;
  const record = parsed as { visibleColumns?: unknown; open?: unknown };
  const columns = Array.isArray(record.visibleColumns) && record.visibleColumns.every(isHomeColumnId)
    ? (record.visibleColumns as HomeColumnId[])
    : null;
  const open = Array.isArray(record.open) && record.open.every((id) => typeof id === 'string')
    ? (record.open as string[])
    : [];
  return Object.freeze({ visibleColumns: columns === null ? null : Object.freeze(columns), open: Object.freeze(open) });
}

/** Write the device view. A storage failure is not fatal: the next read falls back to the default. */
export function writeHomeDeviceView(view: HomeDeviceView, storage: WritableStorage | null = storageOrNull()): void {
  if (storage === null) return;
  try {
    storage.setItem(HOME_DEVICE_KEY, JSON.stringify({ visibleColumns: view.visibleColumns, open: view.open }));
  } catch {
    // Quota or private-mode failure: the view is a convenience, not a requirement.
  }
}

/**
 * A group-by change resets the collapsed set: the groups a reader opened under
 * one grouping do not name groups under another. The URL owns the grouping; the
 * device keeps the open set, so this clears only the device half.
 */
export function clearHomeOpenGroups(storage: WritableStorage | null = storageOrNull()): void {
  const view = readHomeDeviceView(storage);
  writeHomeDeviceView({ visibleColumns: view.visibleColumns, open: [] }, storage);
}

/** The column ids in their default order, for the first render before a reader hides one. */
export const DEFAULT_HOME_COLUMNS: readonly HomeColumnId[] = HOME_COLUMN_IDS;
