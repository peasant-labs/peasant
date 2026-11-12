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

/**
 * One side of a conflict row, ready to draw. A scalar is a phrase; an object or
 * an array is the server's value written out as indented key and value lines.
 * Exactly one of the two is set.
 */
export interface ConflictValue {
  /** The phrase for a scalar, or null for a structured value. */
  readonly phrase: string | null;
  /** The structured value as YAML-style lines, in the order the server sent it; empty for a scalar. */
  readonly lines: readonly string[];
}

/** A structured value longer than this many lines is drawn collapsed. */
export const CONFLICT_BLOCK_OPEN_LINES = 8;

const PLAIN_KEY = /^[A-Za-z0-9_][A-Za-z0-9_./@-]*$/;
// A string that would read as another type, or that needs its quotes to be read as it is.
const AMBIGUOUS_STRING = /^(?:true|false|null|~|[-+]?\d+(?:\.\d+)?)$|^\s|\s$|[\n\r\t]|: | #|^[-?:,[\]{}#&*!|>'"%@`]/i;

function scalarText(value: unknown): string {
  if (value === null) return 'null';
  if (typeof value === 'string') return value === '' || AMBIGUOUS_STRING.test(value) ? JSON.stringify(value) : value;
  if (typeof value === 'number' || typeof value === 'boolean') return String(value);
  return JSON.stringify(value) ?? String(value);
}

function isStructured(value: unknown): value is Record<string, unknown> | readonly unknown[] {
  return value !== null && typeof value === 'object';
}

function isEmpty(value: Record<string, unknown> | readonly unknown[]): boolean {
  return Array.isArray(value) ? value.length === 0 : Object.keys(value).length === 0;
}

/** A scalar's text, or `{}` or `[]` for an empty structure: what stands after `key:` or `-` on one line. */
function inlineText(value: unknown): string {
  if (!isStructured(value)) return scalarText(value);
  return Array.isArray(value) ? '[]' : '{}';
}

function emit(value: Record<string, unknown> | readonly unknown[], indent: string, out: string[]): void {
  if (Array.isArray(value)) {
    for (const item of value) {
      if (!isStructured(item) || isEmpty(item)) {
        out.push(`${indent}- ${inlineText(item)}`);
        continue;
      }
      // The first line of a nested structure shares the dash line, as YAML writes it.
      const nested: string[] = [];
      emit(item, `${indent}  `, nested);
      out.push(`${indent}- ${nested[0].slice(indent.length + 2)}`, ...nested.slice(1));
    }
    return;
  }
  for (const [key, entry] of Object.entries(value)) {
    const name = PLAIN_KEY.test(key) ? key : JSON.stringify(key);
    if (!isStructured(entry) || isEmpty(entry)) {
      out.push(`${indent}${name}: ${inlineText(entry)}`);
      continue;
    }
    out.push(`${indent}${name}:`);
    emit(entry, `${indent}  `, out);
  }
}

/**
 * An object or an array as readable lines, in the order the server sent it:
 * `key: value`, nested keys indented two spaces, list items led by `- `. It is
 * display formatting only. Nothing is counted, reordered, merged or reread as a
 * selection rule.
 */
export function structuredLines(value: Record<string, unknown> | readonly unknown[]): readonly string[] {
  if (isEmpty(value)) return [inlineText(value)];
  const out: string[] = [];
  emit(value, '', out);
  return out;
}

/** How one side of a conflict row is drawn: a scalar as a phrase, a structure as lines. */
export function conflictValue(value: LocalSettingValue, field?: KickstartField): ConflictValue {
  if (isStructured(value)) return { phrase: null, lines: structuredLines(value) };
  return { phrase: describeValue(value, field), lines: [] };
}

/** One conflict row's two sides, ready to draw. */
export function conflictSides(session: KickstartSession, row: KickstartConflict): { mine: ConflictValue; theirs: ConflictValue } {
  const field = fieldByKey(session, row.field)?.field;
  return { mine: conflictValue(row.mine, field), theirs: conflictValue(row.theirs, field) };
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
