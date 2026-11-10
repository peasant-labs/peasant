import {
  isHomeColumnWire,
  type HomeColumnWire,
} from '@/components/home/homeColumns';

/**
 * The device half of the saved view: the visible columns and the open groups.
 * It lives in `localStorage` under `peasant.home`, never in the URL. A copied
 * URL therefore reproduces the data view and the grouping, not the sender's
 * columns.
 *
 * The stored columns are the closed-set members' `wire` values, so the stored
 * shape is plain strings; the table receives members composed in `page.tsx`.
 */
export interface HomeDeviceView {
  /** The columns the reader can see, in table order. `null` means the default set. */
  readonly visibleColumns: readonly HomeColumnWire[] | null;
  /**
   * The qualified group ids whose rows are collapsed. `null` or an empty array
   * means nothing is collapsed; a group renders open unless its qualified id is
   * listed. It is a global set, not a page-local allow-list.
   */
  readonly collapsed: readonly string[] | null;
}

export const HOME_DEVICE_KEY = 'peasant.home';

const DEFAULT_DEVICE_VIEW: HomeDeviceView = Object.freeze({ visibleColumns: null, collapsed: null });

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
  const record = parsed as { visibleColumns?: unknown; collapsed?: unknown };
  const columns = Array.isArray(record.visibleColumns) && record.visibleColumns.every(isHomeColumnWire)
    ? (record.visibleColumns as HomeColumnWire[])
    : null;
  const collapsed = Array.isArray(record.collapsed) && record.collapsed.every((id) => typeof id === 'string')
    ? (record.collapsed as string[])
    : null;
  return Object.freeze({
    visibleColumns: columns === null ? null : Object.freeze(columns),
    collapsed: collapsed === null ? null : Object.freeze(collapsed),
  });
}

/** Write the device view. A storage failure is not fatal: the next read falls back to the default. */
export function writeHomeDeviceView(view: HomeDeviceView, storage: WritableStorage | null = storageOrNull()): void {
  if (storage === null) return;
  try {
    storage.setItem(
      HOME_DEVICE_KEY,
      JSON.stringify({ visibleColumns: view.visibleColumns, collapsed: view.collapsed }),
    );
  } catch {
    // Quota or private-mode failure: the view is a convenience, not a requirement.
  }
}

/**
 * A group-by change resets the collapsed set: the groups a reader opened under
 * one grouping do not name groups under another. The URL owns the grouping; the
 * device keeps the open set, so this clears only the device half.
 */
export function clearHomeCollapsedGroups(storage: WritableStorage | null = storageOrNull()): void {
  const view = readHomeDeviceView(storage);
  writeHomeDeviceView({ visibleColumns: view.visibleColumns, collapsed: [] }, storage);
}
