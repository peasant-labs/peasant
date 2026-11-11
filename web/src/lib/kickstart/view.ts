/**
 * Projections of a served session into what a page draws.
 *
 * These are positions and lookups over the server's own answer, never rules:
 * the order of stops, which ones are visible, which option a field currently
 * holds, whether the connect offer is due and which row a conflict names are
 * all read from the session. A function here that needed to decide something
 * the server owns would be a defect in this file, not a feature of it.
 */

import type {
  KickstartConflict,
  KickstartField,
  KickstartOption,
  KickstartOverwrite,
  KickstartScan,
  KickstartSession,
  KickstartStop,
  KickstartTree,
  LocalSettingValue,
} from './model';
import { KickstartFieldKind, KickstartStopKind } from './values';

/** The stops the server shows, in the server's order. */
export function visibleStops(session: KickstartSession): readonly KickstartStop[] {
  return session.stops.filter((stop) => stop.visible);
}

/** The stop with this key, or undefined when the server no longer lists it. */
export function stopByKey(session: KickstartSession, key: string): KickstartStop | undefined {
  return session.stops.find((stop) => stop.key === key);
}

/** The first visible stop: where a session opens. */
export function firstVisibleStop(session: KickstartSession): KickstartStop | undefined {
  return visibleStops(session)[0];
}

/** The review stop, when the scope has one. */
export function reviewStop(session: KickstartSession): KickstartStop | undefined {
  return session.stops.find((stop) => stop.kind === KickstartStopKind.Review);
}

/** The last stop: the finish panel's place in the rail. */
export function ingestStop(session: KickstartSession): KickstartStop | undefined {
  return session.stops.find((stop) => stop.kind === KickstartStopKind.Ingest);
}

/** The position of a stop among the visible ones, for "step N of M". */
export function stepPosition(session: KickstartSession, key: string): { index: number; total: number } | null {
  const visible = visibleStops(session);
  const index = visible.findIndex((stop) => stop.key === key);
  return index < 0 ? null : { index, total: visible.length };
}

/** The visible stop after `key`, in the server's order. */
export function nextVisibleStop(session: KickstartSession, key: string): KickstartStop | undefined {
  const visible = visibleStops(session);
  const index = visible.findIndex((stop) => stop.key === key);
  return index < 0 ? undefined : visible[index + 1];
}

/** The visible stop before `key`, in the server's order. */
export function previousVisibleStop(session: KickstartSession, key: string): KickstartStop | undefined {
  const visible = visibleStops(session);
  const index = visible.findIndex((stop) => stop.key === key);
  return index <= 0 ? undefined : visible[index - 1];
}

/**
 * The first visible stop that follows `afterKey`, whether or not `afterKey`
 * itself is still visible. The server lists every stop in flow order, so
 * "after" is a position in that list.
 */
export function visibleStopAfter(session: KickstartSession, afterKey: string): KickstartStop | undefined {
  const index = session.stops.findIndex((stop) => stop.key === afterKey);
  if (index < 0) return undefined;
  return session.stops.slice(index + 1).find((stop) => stop.visible);
}

/**
 * The stop the village connect offer is drawn on: the visible stop after the
 * one the server named, and only while the server says the offer is on. The
 * page never evaluates the offer's conditions; `connect.offered` is the answer.
 */
export function connectOfferStopKey(session: KickstartSession): string | null {
  if (!session.connect.offered || session.connect.afterStop === null) return null;
  return visibleStopAfter(session, session.connect.afterStop)?.key ?? null;
}

/** Whether a choice field currently holds this option. A number or boolean value matches its token. */
export function isOptionSelected(field: KickstartField, option: KickstartOption): boolean {
  const value = field.value;
  if (value === null || typeof value === 'object') return false;
  return String(value) === option.value;
}

/** The label of the option a field currently holds, when it holds one. */
export function selectedOptionLabel(field: KickstartField): string | null {
  return field.options.find((option) => isOptionSelected(field, option))?.label ?? null;
}

/** A tree field's answer as the server holds it: the selected session-node ids. */
export class KickstartTreeValueError extends TypeError {
  constructor(key: string, received: string) {
    super(
      `peasant sent the answer of the tree field ${JSON.stringify(key)} as ${received}, not a list of session ids. ` +
      'the selection tree cannot show what is selected, so it is not drawn and nothing was changed. ' +
      'update peasant and its web assets to the same version, then reload.',
    );
    this.name = 'KickstartTreeValueError';
  }
}

export function selectedIds(field: KickstartField): readonly string[] {
  const value = field.value;
  if (value === null) return [];
  if (Array.isArray(value) && value.every((id): id is string => typeof id === 'string')) return value;
  throw new KickstartTreeValueError(field.key, Array.isArray(value) ? 'a list that holds a non-string' : typeof value);
}

/** The first tree field of a stop, if any. */
export function treeField(stop: KickstartStop): KickstartField | undefined {
  return stop.fields.find((field) => field.kind === KickstartFieldKind.Tree);
}

/** A field by key anywhere in the session. */
export function fieldByKey(session: KickstartSession, key: string): { stop: KickstartStop; field: KickstartField } | undefined {
  for (const stop of session.stops) {
    const field = stop.fields.find((candidate) => candidate.key === key);
    if (field !== undefined) return { stop, field };
  }
  return undefined;
}

/** How a conflict side reads on the page: the option's label when the field has one, else the plain value. */
export function describeValue(value: LocalSettingValue, field?: KickstartField): string {
  if (value === null) return 'not set';
  if (field !== undefined) {
    const option = field.options.find((candidate) => String(value) === candidate.value);
    if (option !== undefined) return option.label;
  }
  if (typeof value === 'boolean') return value ? 'on' : 'off';
  if (typeof value === 'string') return value === '' ? 'empty' : value;
  if (typeof value === 'number') return String(value);
  return JSON.stringify(value);
}

/** One conflict row's two sides, labelled for the page. */
export function conflictSides(session: KickstartSession, row: KickstartConflict): { mine: string; theirs: string } {
  const field = fieldByKey(session, row.field)?.field;
  return { mine: describeValue(row.mine, field), theirs: describeValue(row.theirs, field) };
}

/**
 * The notice a stale write earns: which fields it replaced and what they held.
 * The sentence restates the server's report; it decides nothing.
 */
export function overwriteNotices(session: KickstartSession, overwritten: readonly KickstartOverwrite[]): readonly string[] {
  return overwritten.map((entry) => {
    const found = fieldByKey(session, entry.key);
    const name = found?.field.label ?? entry.key;
    const previous = describeValue(entry.previous, found?.field);
    return `another tab changed ${name}, and your answer replaced ${previous}.`;
  });
}

/** Progress of a scan as a whole percentage, or null while its total is unknown. */
export function scanPercent(scan: KickstartScan): number | null {
  if (scan.total <= 0) return null;
  return Math.min(100, Math.round((scan.done / scan.total) * 100));
}

/** The rows of a tree, shaped for fairtrade's SelectionTree. State and lock are passed through untouched. */
export function treeRows(tree: KickstartTree): readonly {
  id: string;
  parentId?: string;
  kind: string;
  state: string;
  label: string;
  sessionId?: string;
  harness?: string;
  childCount?: number;
  locked: boolean;
}[] {
  return tree.nodes.map((node) => ({
    id: node.id,
    ...(node.parentId !== null ? { parentId: node.parentId } : {}),
    kind: node.kind.wire,
    state: node.state.wire,
    label: node.label,
    ...(node.sessionId !== null ? { sessionId: node.sessionId } : {}),
    ...(node.harness !== null ? { harness: node.harness } : {}),
    ...(node.childCount !== null ? { childCount: node.childCount } : {}),
    locked: node.locked,
  }));
}

/** The sentence that names rows the page could not draw, or null when none were dropped. */
export function droppedRowsNotice(tree: KickstartTree): string | null {
  const { unreadable, unknownKind, unknownState } = tree.dropped;
  const parts: string[] = [];
  if (unknownKind > 0) parts.push(`${unknownKind} with a kind this page does not know`);
  if (unknownState > 0) parts.push(`${unknownState} with a state this page does not know`);
  if (unreadable > 0) parts.push(`${unreadable} without an id or a name`);
  return parts.length === 0 ? null : `some rows are hidden: ${parts.join(', ')}. update peasant and reload to see them.`;
}
