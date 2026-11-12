/**
 * The kickstart wire, read once at the boundary.
 *
 * Every body from `/api/v1/kickstart/*` and every event on the `kickstart`
 * topic enters here. It is checked against the generated Local API contract,
 * then each closed-set token becomes its member from `values.ts`. What leaves
 * this module is typed and total: a page that holds a {@link KickstartSession}
 * never re-checks a token and never meets a string it cannot name.
 *
 * Nothing here decides anything. Order, visibility, options, defaults,
 * validity, consent copy and the connect offer are the server's; this module
 * only carries them.
 */

import {
  AllLocalKickstartErrorCodes,
  zLocalKickstartCommitRefusal,
  zLocalKickstartCommitted,
  zLocalKickstartErrorResponse,
  zLocalKickstartEvent,
  zLocalKickstartPatchResult,
  zLocalKickstartPreview,
  zLocalKickstartScan,
  zLocalKickstartSession,
  zLocalKickstartStatus,
  zLocalKickstartTree,
  type LocalKickstartCommitted as WireCommitted,
  type LocalKickstartErrorCode,
  type LocalKickstartErrorResponse as WireErrorBody,
  type LocalKickstartIngest as WireIngest,
  type LocalKickstartNode,
  type LocalKickstartScan as WireScan,
  type LocalKickstartSession as WireSession,
  type LocalKickstartTree,
  type LocalSettingValue,
} from '@peasant-labs/schema';
import {
  KickstartCloseReason,
  KickstartEventKind,
  KickstartScanState,
  parseKickstartCloseReason,
  parseKickstartDiffLineType,
  parseKickstartEventKind,
  parseKickstartFieldKind,
  parseKickstartIngestState,
  parseKickstartLineKind,
  parseKickstartNextStep,
  parseKickstartScanState,
  parseKickstartScope,
  parseKickstartStopKind,
  type KickstartDiffLineType,
  type KickstartFieldKind,
  type KickstartIngestState,
  type KickstartLineKind,
  type KickstartNextStep,
  type KickstartScanState as KickstartScanStateMember,
  type KickstartScope,
  type KickstartStopKind,
} from './values';

export type { LocalKickstartErrorCode, LocalSettingValue };

/** A body the generated contract refuses. It is never read as a guess. */
export class KickstartContractError extends Error {
  readonly where: string;

  constructor(where: string, detail: string) {
    super(
      `peasant's answer to ${where} breaks the Local API contract, so it is not shown: ${detail}. ` +
      'nothing was changed. check that peasant and its web assets are the same version, then reload.',
    );
    this.name = 'KickstartContractError';
    this.where = where;
  }
}

interface Issue {
  readonly path: PropertyKey[];
  readonly message: string;
}

interface Decoder<T> {
  safeParse(value: unknown): { success: true; data: T } | { success: false; error: { issues: readonly Issue[] } };
}

const SHOWN_ISSUES = 3;

function decode<T>(schema: Decoder<T>, raw: unknown, where: string, describe?: (issues: readonly Issue[]) => string): T {
  const parsed = schema.safeParse(raw);
  if (parsed.success) return parsed.data;
  const { issues } = parsed.error;
  const detail =
    describe?.(issues) ??
    issues
      .slice(0, SHOWN_ISSUES)
      .map((issue) => `${issue.path.map(String).join('.') || '<root>'}: ${issue.message}`)
      .join('; ');
  throw new KickstartContractError(where, detail || 'unknown shape');
}

// ── the model ────────────────────────────────────────────────────────────

export interface KickstartOption {
  readonly value: string;
  readonly label: string;
  readonly description: string | null;
}

export interface KickstartLine {
  readonly kind: KickstartLineKind;
  readonly text: string;
}

/** The server's refusal of one field's current answer. */
export interface KickstartRefusal {
  readonly key: string;
  readonly error: string;
}

export interface KickstartField {
  readonly key: string;
  readonly kind: KickstartFieldKind;
  readonly label: string;
  readonly description: string | null;
  readonly value: LocalSettingValue;
  readonly options: readonly KickstartOption[];
  readonly example: readonly KickstartLine[];
  readonly refusal: KickstartRefusal | null;
}

export interface KickstartDiffLine {
  readonly type: KickstartDiffLineType;
  readonly oldNo: number | null;
  readonly newNo: number | null;
  readonly text: string;
}

export interface KickstartHunk {
  readonly header: string;
  readonly lines: readonly KickstartDiffLine[];
}

export interface KickstartDiff {
  readonly file: string;
  readonly hunks: readonly KickstartHunk[];
  readonly truncated: boolean;
}

export interface KickstartSummary {
  readonly values: readonly string[];
  readonly effects: readonly string[];
}

export interface KickstartStop {
  readonly key: string;
  readonly kind: KickstartStopKind;
  readonly title: string;
  readonly visible: boolean;
  readonly intro: string | null;
  readonly hints: readonly string[];
  readonly fields: readonly KickstartField[];
  readonly summary: KickstartSummary | null;
  readonly diff: KickstartDiff | null;
}

/** One answered field whose saved value moved. `mine` is the draft; `theirs` is the file. */
export interface KickstartConflict {
  readonly field: string;
  readonly stop: string;
  readonly label: string;
  readonly mine: LocalSettingValue;
  readonly theirs: LocalSettingValue;
}

/** The village connect offer, already evaluated by the server. */
export interface KickstartConnect {
  readonly offered: boolean;
  readonly afterStop: string | null;
  readonly prompt: string | null;
  readonly skip: string | null;
}

export interface KickstartScan {
  readonly state: KickstartScanStateMember;
  readonly phase: string | null;
  readonly done: number;
  readonly total: number;
  readonly error: string | null;
  readonly lockedNodes: number;
  readonly droppedSelections: number;
  readonly droppedLabels: readonly string[];
}

export interface KickstartVillage {
  readonly connected: boolean;
  readonly url: string | null;
  readonly userId: string | null;
}

export interface KickstartIngest {
  readonly state: KickstartIngestState;
  readonly note: string | null;
  readonly error: string | null;
  readonly new: number;
  readonly updated: number;
}

export interface KickstartSession {
  readonly id: string;
  readonly scope: KickstartScope;
  readonly revision: number;
  readonly openedAt: string;
  readonly stops: readonly KickstartStop[];
  readonly conflicts: readonly KickstartConflict[];
  readonly connect: KickstartConnect;
  readonly scan: KickstartScan;
  readonly village: KickstartVillage;
}

/** The finish body of a commit: what was saved and what to do next. */
export interface KickstartCommitted {
  readonly configPath: string;
  readonly ingest: KickstartIngest;
  readonly nextSteps: readonly KickstartNextStep[];
  readonly diagnostics: readonly string[];
}

export interface KickstartOverwrite {
  readonly key: string;
  readonly previous: LocalSettingValue;
  readonly revision: number;
}

export interface KickstartPatchResult {
  readonly revision: number;
  readonly stale: boolean;
  readonly overwritten: readonly KickstartOverwrite[];
  readonly view: KickstartSession | null;
}

export interface KickstartStatus {
  readonly configured: boolean;
  readonly activeSessionId: string | null;
}

/**
 * One row of the selection forest, as the contract serves it. The page does
 * not model a row: fairtrade's selection tree owns the kind and the state of a
 * row, so the validated wire rows are handed to it as they came.
 */
export type KickstartTreeNode = LocalKickstartNode;

/** The whole forest: a list of rows and nothing else. */
export type KickstartTree = LocalKickstartTree;

// ── wire → model ─────────────────────────────────────────────────────────

function lines(raw: readonly { kind: string; text: string }[] | undefined, where: string): readonly KickstartLine[] {
  return (raw ?? []).map((line) => ({ kind: parseKickstartLineKind(line.kind, where), text: line.text }));
}

function decodeIngest(raw: WireIngest, where: string): KickstartIngest {
  return {
    state: parseKickstartIngestState(raw.state, where),
    note: raw.note ?? null,
    error: raw.error ?? null,
    new: raw.new,
    updated: raw.updated,
  };
}

function decodeScan(raw: WireScan, where: string): KickstartScan {
  return {
    state: parseKickstartScanState(raw.state, where),
    phase: raw.phase ?? null,
    done: raw.done,
    total: raw.total,
    error: raw.error ?? null,
    lockedNodes: raw.lockedNodes ?? 0,
    droppedSelections: raw.droppedSelections ?? 0,
    droppedLabels: raw.droppedLabels ?? [],
  };
}

function decodeCommitted(raw: WireCommitted, where: string): KickstartCommitted {
  return {
    configPath: raw.configPath,
    ingest: decodeIngest(raw.ingest, where),
    nextSteps: raw.nextSteps.map((step) => parseKickstartNextStep(step, where)),
    diagnostics: raw.diagnostics ?? [],
  };
}

function decodeSession(raw: WireSession, where: string): KickstartSession {
  return {
    id: raw.id,
    scope: parseKickstartScope(raw.scope, where),
    revision: raw.revision,
    openedAt: raw.openedAt,
    stops: raw.stops.map((stop) => ({
      key: stop.key,
      kind: parseKickstartStopKind(stop.kind, where),
      title: stop.title,
      visible: stop.visible,
      intro: stop.guide.intro ?? null,
      hints: stop.guide.hints ?? [],
      fields: stop.fields.map((field) => ({
        key: field.key,
        kind: parseKickstartFieldKind(field.kind, where),
        label: field.label,
        description: field.description ?? null,
        value: field.value,
        options: (field.options ?? []).map((option) => ({
          value: option.value,
          label: option.label,
          description: option.description ?? null,
        })),
        example: lines(field.example, where),
        refusal: field.refusal ? { key: field.refusal.key, error: field.refusal.error } : null,
      })),
      summary: stop.summary ? { values: stop.summary.values, effects: stop.summary.effects } : null,
      diff: stop.diff
        ? {
            file: stop.diff.file,
            truncated: stop.diff.truncated === true,
            hunks: stop.diff.hunks.map((hunk) => ({
              header: hunk.header,
              lines: hunk.lines.map((line) => ({
                type: parseKickstartDiffLineType(line.type, where),
                oldNo: line.oldNo ?? null,
                newNo: line.newNo ?? null,
                text: line.text,
              })),
            })),
          }
        : null,
    })),
    conflicts: raw.conflicts.map((row) => ({ field: row.field, stop: row.stop, label: row.label, mine: row.mine, theirs: row.theirs })),
    connect: {
      offered: raw.connect.offered,
      afterStop: raw.connect.afterStop ?? null,
      prompt: raw.connect.prompt ?? null,
      skip: raw.connect.skip ?? null,
    },
    scan: decodeScan(raw.scan, where),
    village: { connected: raw.village.connected, url: raw.village.url ?? null, userId: raw.village.userId ?? null },
  };
}

/** GET /kickstart/status. */
export function parseKickstartStatus(raw: unknown): KickstartStatus {
  const status = decode(zLocalKickstartStatus, raw, 'GET /api/v1/kickstart/status');
  return { configured: status.configured, activeSessionId: status.activeSessionId ?? null };
}

/** The session snapshot a POST or GET of a session answers, or an event carries. */
export function parseKickstartSession(raw: unknown, where: string): KickstartSession {
  return decodeSession(decode(zLocalKickstartSession, raw, where), where);
}

/** PATCH /kickstart/sessions/{id}. */
export function parseKickstartPatchResult(raw: unknown): KickstartPatchResult {
  const where = 'PATCH /api/v1/kickstart/sessions/{id}';
  const result = decode(zLocalKickstartPatchResult, raw, where);
  return {
    revision: result.revision,
    stale: result.stale,
    overwritten: (result.overwritten ?? []).map((entry) => ({ key: entry.key, previous: entry.previous, revision: entry.revision })),
    view: result.view ? decodeSession(result.view, where) : null,
  };
}

/** POST /kickstart/sessions/{id}/commit. */
export function parseKickstartCommitted(raw: unknown): KickstartCommitted {
  const where = 'POST /api/v1/kickstart/sessions/{id}/commit';
  return decodeCommitted(decode(zLocalKickstartCommitted, raw, where), where);
}

/** What a refused kickstart request says: the server's sentence and the closed code that names the refusal. */
export interface KickstartErrorBody {
  readonly error: string;
  readonly code: LocalKickstartErrorCode;
}

/**
 * What is wrong with a refusal that breaks the contract: the code it carries,
 * or that it carries none, and what the page does know. The codes are a closed
 * set, so an unknown one is never shown as if it meant something else.
 */
const CODE_FIELD: keyof WireErrorBody = 'code';
const SENTENCE_FIELD: keyof WireErrorBody = 'error';

function describeRefusalIssues(raw: unknown, issues: readonly Issue[]): string {
  const fields = typeof raw === 'object' && raw !== null && !Array.isArray(raw) ? (raw as Record<string, unknown>) : {};
  const said = issues.slice(0, SHOWN_ISSUES).map((issue) => {
    const [field] = issue.path;
    if (field === CODE_FIELD) {
      const held = CODE_FIELD in fields ? `names the code ${JSON.stringify(fields[CODE_FIELD])}` : 'names no code';
      return `the refusal ${held}, which is not one of the ${AllLocalKickstartErrorCodes.length} codes this page knows, so what went wrong cannot be told`;
    }
    if (field === SENTENCE_FIELD) return 'the refusal has no sentence saying what went wrong';
    return `${issue.path.map(String).join('.') || 'the refusal'}: ${issue.message}`;
  });
  return said.join('; ');
}

/**
 * The body every kickstart route answers on 400, 404, 500 and 503: `{error,
 * code}`, the code from the closed set. One parser for all of them, so a code
 * the page does not know fails closed here with the contract message, and no
 * caller compares a string.
 */
export function parseKickstartErrorBody(raw: unknown, where: string): KickstartErrorBody {
  const body = decode(zLocalKickstartErrorResponse, raw, where, (issues) => describeRefusalIssues(raw, issues));
  return { error: body.error, code: body.code };
}

/** The 409 body of a commit: the rows that blocked the save. Nothing was written. */
export function parseKickstartCommitRefusal(raw: unknown): { error: string; conflicts: readonly KickstartConflict[] } {
  const refusal = decode(zLocalKickstartCommitRefusal, raw, 'the commit refusal');
  return {
    error: refusal.error,
    conflicts: refusal.conflicts.map((row) => ({ field: row.field, stop: row.stop, label: row.label, mine: row.mine, theirs: row.theirs })),
  };
}

/** POST /kickstart/sessions/{id}/scan. */
export function parseKickstartScan(raw: unknown): KickstartScan {
  const where = 'POST /api/v1/kickstart/sessions/{id}/scan';
  return decodeScan(decode(zLocalKickstartScan, raw, where), where);
}

/** GET …/tree/{nodeId}/preview. */
export function parseKickstartPreview(raw: unknown): readonly KickstartLine[] {
  const where = 'GET /api/v1/kickstart/sessions/{id}/tree/{nodeId}/preview';
  return lines(decode(zLocalKickstartPreview, raw, where).lines, where);
}

/**
 * What is wrong with a forest, said row by row: which row (by its place in the
 * list and its name), which field, what it held and what the contract allows.
 * An issue that is not about a row is said by its path.
 */
function describeForestIssues(raw: unknown, issues: readonly Issue[]): string {
  const rows = typeof raw === 'object' && raw !== null && Array.isArray((raw as { nodes?: unknown }).nodes) ? (raw as { nodes: unknown[] }).nodes : [];
  const said = issues.slice(0, SHOWN_ISSUES).map((issue) => {
    const [head, index, field] = issue.path;
    if (head !== 'nodes' || typeof index !== 'number') return `${issue.path.map(String).join('.') || 'the list'}: ${issue.message}`;
    const row = rows[index];
    const fields = typeof row === 'object' && row !== null ? (row as Record<string, unknown>) : {};
    const name = typeof fields.label === 'string' && fields.label !== '' ? `"${fields.label}"` : typeof fields.id === 'string' && fields.id !== '' ? `id ${fields.id}` : 'with no name';
    if (typeof field !== 'string') return `row ${index + 1} (${name}): ${issue.message}`;
    const held = field in fields ? `holds ${JSON.stringify(fields[field])}` : 'has none';
    return `row ${index + 1} (${name}) ${held} for ${field}: ${issue.message}`;
  });
  const more = issues.length > SHOWN_ISSUES ? `; and ${issues.length - SHOWN_ISSUES} more` : '';
  return `the list of sessions has rows the contract does not allow, so none of it is drawn rather than drawing the rest from rows it had to guess at: ${said.join('; ')}${more}`;
}

/**
 * GET …/tree: the whole forest. It is checked against the generated contract
 * like every other body, so a row outside it (an id or a label missing, a kind
 * or a state the contract does not list) refuses the whole forest with the
 * actionable contract error: which rows, why, that nothing was changed and what
 * to do. The forest is not drawn from rows the page had to guess at, and no row
 * is relabelled as another kind or state.
 */
export function parseKickstartTree(raw: unknown): KickstartTree {
  return decode(zLocalKickstartTree, raw, 'GET /api/v1/kickstart/sessions/{id}/tree', (issues) => describeForestIssues(raw, issues));
}

// ── events ───────────────────────────────────────────────────────────────

const snapshotBrand: unique symbol = Symbol('kickstartSnapshotEvent');
const scanBrand: unique symbol = Symbol('kickstartScanEvent');
const ingestBrand: unique symbol = Symbol('kickstartIngestEvent');
const closedBrand: unique symbol = Symbol('kickstartClosedEvent');

/** `session`, `answers` or `conflicts`: the whole view of one session. */
export class KickstartSnapshotEvent {
  private readonly [snapshotBrand] = true;
  constructor(
    readonly kind: KickstartEventKind,
    readonly sessionId: string,
    readonly revision: number,
    readonly view: KickstartSession,
  ) {}
}

/** The discovery job's state. The forest is not here: the page loads it from the tree route. */
export class KickstartScanEvent {
  private readonly [scanBrand] = true;
  constructor(readonly sessionId: string, readonly revision: number, readonly scan: KickstartScan) {}
}

/** The import's state. It keeps arriving after the session closes. */
export class KickstartIngestEvent {
  private readonly [ingestBrand] = true;
  constructor(readonly sessionId: string, readonly revision: number, readonly ingest: KickstartIngest) {}
}

/** The session ended. A commit carries the finish body, so every tab can show it. */
export class KickstartClosedEvent {
  private readonly [closedBrand] = true;
  constructor(
    readonly sessionId: string,
    readonly revision: number,
    readonly reason: KickstartCloseReason,
    readonly committed: KickstartCommitted | null,
    readonly view: KickstartSession | null,
  ) {}
}

export type KickstartEvent = KickstartSnapshotEvent | KickstartScanEvent | KickstartIngestEvent | KickstartClosedEvent;

/** One method per event class, so a consumer is exhaustive by construction and compares no token. */
export interface KickstartEventVisitor<R> {
  snapshot(event: KickstartSnapshotEvent): R;
  scan(event: KickstartScanEvent): R;
  ingest(event: KickstartIngestEvent): R;
  closed(event: KickstartClosedEvent): R;
}

export function acceptKickstartEvent<R>(event: KickstartEvent, visitor: KickstartEventVisitor<R>): R {
  if (event instanceof KickstartSnapshotEvent) return visitor.snapshot(event);
  if (event instanceof KickstartScanEvent) return visitor.scan(event);
  if (event instanceof KickstartIngestEvent) return visitor.ingest(event);
  return visitor.closed(event);
}

function carries(where: string, kind: string, fields: Readonly<Record<string, boolean>>, allowed: readonly string[]): void {
  const extra = Object.entries(fields).filter(([name, present]) => present && !allowed.includes(name)).map(([name]) => name);
  if (extra.length > 0) {
    throw new KickstartContractError(where, `a ${kind} event carries ${extra.join(', ')}, which belongs to another kind of event`);
  }
}

/**
 * One message on the `kickstart` topic. The contract's cross-field rules (one
 * session id, a view for a snapshot, the finish body only on a commit) are
 * checked here, because the generated validator cannot state them.
 */
export function parseKickstartEvent(raw: unknown): KickstartEvent {
  const where = 'a kickstart topic message';
  const wire = decode(zLocalKickstartEvent, raw, where);
  const kind = parseKickstartEventKind(wire.kind, where);
  const present = {
    view: wire.view !== undefined,
    scan: wire.scan !== undefined,
    ingest: wire.ingest !== undefined,
    reason: wire.reason !== undefined,
    committed: wire.committed !== undefined,
  };
  const view = wire.view ? decodeSession(wire.view, where) : null;
  if (view !== null && view.id !== wire.sessionId) {
    throw new KickstartContractError(where, `the view is session ${view.id} but the event names ${wire.sessionId}; one event carries one session`);
  }
  if (view !== null && view.revision !== wire.revision) {
    throw new KickstartContractError(where, `the view is at revision ${view.revision} but the event says ${wire.revision}`);
  }

  if (kind === KickstartEventKind.Session || kind === KickstartEventKind.Answers || kind === KickstartEventKind.Conflicts) {
    carries(where, kind.label, present, ['view']);
    if (view === null) throw new KickstartContractError(where, `a ${kind.label} event carries no view`);
    return new KickstartSnapshotEvent(kind, wire.sessionId, wire.revision, view);
  }
  if (kind === KickstartEventKind.Scan) {
    carries(where, kind.label, present, ['scan']);
    if (!wire.scan) throw new KickstartContractError(where, 'a scan event carries no scan');
    return new KickstartScanEvent(wire.sessionId, wire.revision, decodeScan(wire.scan, where));
  }
  if (kind === KickstartEventKind.Ingest) {
    carries(where, kind.label, present, ['ingest']);
    if (!wire.ingest) throw new KickstartContractError(where, 'an ingest event carries no ingest');
    return new KickstartIngestEvent(wire.sessionId, wire.revision, decodeIngest(wire.ingest, where));
  }
  carries(where, kind.label, present, ['view', 'reason', 'committed']);
  const reason = parseKickstartCloseReason(wire.reason, where);
  const committed = wire.committed ? decodeCommitted(wire.committed, where) : null;
  if ((reason === KickstartCloseReason.Committed) !== (committed !== null)) {
    throw new KickstartContractError(where, 'the finish body belongs to a committed closure and to no other');
  }
  return new KickstartClosedEvent(wire.sessionId, wire.revision, reason, committed, view);
}

/** Whether a scan has finished loading its forest. */
export function scanIsDone(scan: KickstartScan): boolean {
  return scan.state === KickstartScanState.Done;
}
