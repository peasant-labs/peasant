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
  zLocalKickstartCommitRefusal,
  zLocalKickstartCommitted,
  zLocalKickstartEvent,
  zLocalKickstartPatchResult,
  zLocalKickstartPreview,
  zLocalKickstartScan,
  zLocalKickstartSession,
  zLocalKickstartStatus,
  type LocalKickstartCommitted as WireCommitted,
  type LocalKickstartIngest as WireIngest,
  type LocalKickstartScan as WireScan,
  type LocalKickstartSession as WireSession,
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
  parseKickstartNodeKind,
  parseKickstartNodeState,
  parseKickstartScanState,
  parseKickstartScope,
  parseKickstartStopKind,
  type KickstartDiffLineType,
  type KickstartFieldKind,
  type KickstartIngestState,
  type KickstartLineKind,
  type KickstartNextStep,
  type KickstartNodeKind,
  type KickstartNodeState,
  type KickstartScanState as KickstartScanStateMember,
  type KickstartScope,
  type KickstartStopKind,
} from './values';

export type { LocalSettingValue };

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

interface Decoder<T> {
  safeParse(value: unknown):
    | { success: true; data: T }
    | { success: false; error: { issues: readonly { path: PropertyKey[]; message: string }[] } };
}

function decode<T>(schema: Decoder<T>, raw: unknown, where: string): T {
  const parsed = schema.safeParse(raw);
  if (parsed.success) return parsed.data;
  const detail = parsed.error.issues
    .slice(0, 3)
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

export interface KickstartTreeNode {
  readonly id: string;
  readonly parentId: string | null;
  readonly kind: KickstartNodeKind;
  readonly state: KickstartNodeState;
  readonly label: string;
  readonly sessionId: string | null;
  readonly harness: string | null;
  readonly childCount: number | null;
  readonly locked: boolean;
}

/** Rows this page could not name, counted by why. They are never relabelled. */
export interface KickstartTreeDrops {
  readonly unreadable: number;
  readonly unknownKind: number;
  readonly unknownState: number;
}

export interface KickstartTree {
  readonly nodes: readonly KickstartTreeNode[];
  readonly dropped: KickstartTreeDrops;
}

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

function record(value: unknown): Record<string, unknown> | null {
  return value !== null && typeof value === 'object' && !Array.isArray(value) ? (value as Record<string, unknown>) : null;
}

/**
 * GET …/tree: the whole forest. A row the page cannot name (a kind or a state
 * outside the closed sets, or no id or label) is counted and left out, so one
 * bad row cannot blank the tree. Nothing is relabelled as another kind or state.
 */
export function parseKickstartTree(raw: unknown): KickstartTree {
  const where = 'GET /api/v1/kickstart/sessions/{id}/tree';
  const body = record(raw);
  if (body === null || !Array.isArray(body.nodes)) {
    throw new KickstartContractError(where, 'nodes: expected a list of rows');
  }
  const extra = Object.keys(body).filter((key) => key !== 'nodes');
  if (extra.length > 0) {
    throw new KickstartContractError(where, `unexpected field ${extra.join(', ')}; the tree has no page, cursor or total`);
  }
  const nodes: KickstartTreeNode[] = [];
  let unreadable = 0;
  let unknownKind = 0;
  let unknownState = 0;
  for (const entry of body.nodes as unknown[]) {
    const row = record(entry);
    if (row === null || typeof row.id !== 'string' || row.id === '' || typeof row.label !== 'string' || row.label === '') {
      unreadable += 1;
      continue;
    }
    let kind: KickstartNodeKind;
    let state: KickstartNodeState;
    try {
      kind = parseKickstartNodeKind(row.kind, where);
    } catch {
      unknownKind += 1;
      continue;
    }
    try {
      state = parseKickstartNodeState(row.state, where);
    } catch {
      unknownState += 1;
      continue;
    }
    nodes.push({
      id: row.id,
      parentId: typeof row.parentId === 'string' && row.parentId !== '' ? row.parentId : null,
      kind,
      state,
      label: row.label,
      sessionId: typeof row.sessionId === 'string' && row.sessionId !== '' ? row.sessionId : null,
      harness: typeof row.harness === 'string' && row.harness !== '' ? row.harness : null,
      childCount: typeof row.childCount === 'number' && Number.isInteger(row.childCount) && row.childCount >= 0 ? row.childCount : null,
      locked: row.locked === true,
    });
  }
  return { nodes, dropped: { unreadable, unknownKind, unknownState } };
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
