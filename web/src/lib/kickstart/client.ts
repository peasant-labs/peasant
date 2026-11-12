/**
 * The kickstart HTTP client: one function per route of the Local API contract.
 *
 *   GET   /api/v1/kickstart/status                         first-run redirect hook
 *   POST  /api/v1/kickstart/sessions                       open or join the session for a scope
 *   GET   /api/v1/kickstart/sessions/{id}                  the reconnect snapshot
 *   PATCH /api/v1/kickstart/sessions/{id}                  answers, selected ids, resolutions, connect
 *   GET   /api/v1/kickstart/sessions/{id}/tree             the whole forest
 *   GET   /api/v1/kickstart/sessions/{id}/tree/{n}/preview bounded typed preview lines
 *   POST  /api/v1/kickstart/sessions/{id}/scan             rescan
 *   POST  /api/v1/kickstart/sessions/{id}/commit           save, then start the import
 *
 * Every body is read through `model.ts`, so a caller never sees a token it
 * cannot name. A refused request throws a {@link KickstartRequestError} that
 * says what the server said and carries the closed code that names the refusal;
 * a 409 on commit throws the {@link KickstartCommitRefused} subclass with the
 * rows that blocked the save. The refusal bodies are read by one parser, so a
 * code the page does not know throws the contract error instead.
 */

import { getApiBaseUrl } from '@/lib/api/base';
import {
  KickstartContractError,
  parseKickstartCommitRefusal,
  parseKickstartCommitted,
  parseKickstartPatchResult,
  parseKickstartPreview,
  parseKickstartErrorBody,
  parseKickstartScan,
  parseKickstartSession,
  parseKickstartStatus,
  parseKickstartTree,
  parseSyncIngestStatus,
  type KickstartCommitted,
  type KickstartConflict,
  type KickstartLine,
  type LocalKickstartErrorCode,
  type KickstartPatchResult,
  type KickstartScan,
  type KickstartSession,
  type KickstartStatus,
  type KickstartTree,
  type LocalSettingValue,
  type SyncIngestStatus,
} from './model';
import { refusalAdvice } from './copy';
import type { KickstartConnectAnswer, KickstartResolution, KickstartScope } from './values';

export const KICKSTART_PATH = '/api/v1/kickstart';
const INGEST_PATH = '/api/v1/sync/ingest';
const INGEST_STATUS_PATH = '/api/v1/sync/ingest/status';

const NO_REASON = 'the server gave no reason; retry, then check the peasant server log';

/** A request the server refused or that could not be completed. `message` is the reason to show. */
export class KickstartRequestError extends Error {
  readonly status: number;
  readonly path: string;
  /**
   * The closed code the server named for the refusal. Null for a failure the
   * kickstart contract has no code for: the page could not reach peasant, or
   * something between them answered.
   */
  readonly code: LocalKickstartErrorCode | null;

  constructor(path: string, status: number, message: string, code: LocalKickstartErrorCode | null = null) {
    super(message);
    this.name = 'KickstartRequestError';
    this.path = path;
    this.status = status;
    this.code = code;
  }
}

/** A commit the server refused with mine/theirs rows. Nothing was written. */
export class KickstartCommitRefused extends KickstartRequestError {
  readonly conflicts: readonly KickstartConflict[];

  constructor(path: string, message: string, conflicts: readonly KickstartConflict[]) {
    super(path, 409, message, null);
    this.name = 'KickstartCommitRefused';
    this.conflicts = conflicts;
  }
}

/** The text of a refusal that is not the contract's: the server's `error` when it sent a sentence, else the body. */
function plainReason(text: string): string {
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    return text.trim() || NO_REASON;
  }
  if (parsed !== null && typeof parsed === 'object') {
    const envelope = parsed as { error?: unknown };
    // An envelope with no usable reason: its JSON is not a sentence to show.
    return typeof envelope.error === 'string' && envelope.error.trim() !== '' ? envelope.error.trim() : NO_REASON;
  }
  return text.trim() || NO_REASON;
}

/**
 * The statuses on which a kickstart route answers with the contract's closed
 * refusal body. Others (403 for a request that is not from this computer, a
 * gateway's 502) are not the server's kickstart refusals and are shown as sent.
 *
 * This restates a table the schema owns: its `testdata/local-api/kickstart_errors.yaml`
 * lists each route's statuses and, in `code_status`, the one status each
 * `LocalKickstartErrorCode` answers with (`LocalKickstartErrorCode.HTTPStatus()`
 * in Go). The TypeScript package does not export it yet. When the next schema
 * release does, read the statuses from it instead of listing them here.
 */
const CONTRACT_REFUSAL_STATUSES: ReadonlySet<number> = new Set([400, 404, 500, 503]);

/**
 * The error a refused kickstart request throws. On a status the contract gives
 * a closed body, the body is read by the one boundary parser: the server's own
 * sentence (plus the one sentence the page knows to add for a few codes), and
 * its code. A body that is not that, or a code outside the set, throws the
 * contract error. Any other status is shown as the server sent it.
 */
function refused(method: string, path: string, status: number, text: string): KickstartRequestError | KickstartContractError {
  if (!CONTRACT_REFUSAL_STATUSES.has(status)) return new KickstartRequestError(path, status, plainReason(text));
  const where = `${method} ${path}`;
  let raw: unknown;
  try {
    raw = JSON.parse(text);
  } catch {
    const read = text.trim().slice(0, 120);
    return new KickstartContractError(where, `a refusal (status ${status}) is not JSON${read === '' ? ' and is empty' : `; it reads ${JSON.stringify(read)}`}`);
  }
  try {
    const { error, code } = parseKickstartErrorBody(raw, where);
    const sentence = error.trim() === '' ? NO_REASON : error.trim();
    const advice = refusalAdvice(code);
    return new KickstartRequestError(path, status, advice === null ? sentence : `${sentence} ${advice}`, code);
  } catch (failure) {
    if (failure instanceof KickstartContractError) return failure;
    throw failure;
  }
}

/** The answer's JSON, or the failure that explains why there is none. */
async function exchange(method: string, path: string, body?: unknown): Promise<{ status: number; text: string }> {
  let response: Response;
  try {
    response = await fetch(`${getApiBaseUrl()}${path}`, {
      method,
      headers: body === undefined ? undefined : { 'Content-Type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  } catch (error) {
    const detail = error instanceof Error ? error.message : String(error);
    throw new KickstartRequestError(path, 0, `peasant did not answer (${detail}). check that peasant is running, then retry.`);
  }
  return { status: response.status, text: await response.text() };
}

function jsonOf(path: string, status: number, text: string): unknown {
  try {
    return JSON.parse(text);
  } catch {
    throw new KickstartRequestError(path, status, `peasant answered ${path} with something that is not JSON. retry; if it repeats, report it.`);
  }
}

async function call<T>(method: string, path: string, read: (raw: unknown) => T, body?: unknown): Promise<T> {
  const { status, text } = await exchange(method, path, body);
  if (status < 200 || status >= 300) throw refused(method, path, status, text);
  return read(jsonOf(path, status, text));
}

const sessionPath = (id: string) => `${KICKSTART_PATH}/sessions/${encodeURIComponent(id)}`;

/** Whether setup has been saved. The first-run redirect reads only this. */
export function fetchKickstartStatus(): Promise<KickstartStatus> {
  return call('GET', `${KICKSTART_PATH}/status`, parseKickstartStatus);
}

/** Open the session for `scope`, or join the one already open for it. The answer is the snapshot. */
export function openKickstartSession(scope: KickstartScope): Promise<KickstartSession> {
  return call('POST', `${KICKSTART_PATH}/sessions`, (raw) => parseKickstartSession(raw, 'POST /api/v1/kickstart/sessions'), { scope });
}

/** The snapshot of one session, for a reconnect. */
export function fetchKickstartSession(id: string): Promise<KickstartSession> {
  return call('GET', sessionPath(id), (raw) => parseKickstartSession(raw, 'GET /api/v1/kickstart/sessions/{id}'));
}

/**
 * One PATCH body. `revision` is the session revision the caller last rendered.
 * `values` are per-field answers, `selectedIds` is the tree's answer,
 * `resolutions` pick a side of a conflict row, and `connectAnswer` answers the
 * village connect offer. At least one of the four must be present.
 */
export interface KickstartAnswers {
  readonly revision: number;
  readonly values?: Readonly<Record<string, LocalSettingValue>>;
  readonly selectedIds?: readonly string[];
  readonly resolutions?: Readonly<Record<string, KickstartResolution>>;
  readonly connectAnswer?: KickstartConnectAnswer;
}

/** Apply answers. A stale write is applied and reported in the result, not refused. */
export function patchKickstartSession(id: string, answers: KickstartAnswers): Promise<KickstartPatchResult> {
  return call('PATCH', sessionPath(id), parseKickstartPatchResult, answers);
}

/** The whole forest for one session. */
export function fetchKickstartTree(id: string): Promise<KickstartTree> {
  return call('GET', `${sessionPath(id)}/tree`, parseKickstartTree);
}

/** The bounded typed preview lines of one forest row. */
export function fetchKickstartPreview(id: string, nodeId: string): Promise<readonly KickstartLine[]> {
  return call('GET', `${sessionPath(id)}/tree/${encodeURIComponent(nodeId)}/preview`, parseKickstartPreview);
}

/** Rescan the filesystem. Progress is pushed on the topic; the answer is the scan at accept. */
export function scanKickstartSession(id: string): Promise<KickstartScan> {
  return call('POST', `${sessionPath(id)}/scan`, parseKickstartScan);
}

/**
 * Save the draft and start the import. A 409 throws {@link KickstartCommitRefused}
 * with the mine/theirs rows; nothing was written.
 */
export async function commitKickstartSession(id: string): Promise<KickstartCommitted> {
  const path = `${sessionPath(id)}/commit`;
  const { status, text } = await exchange('POST', path);
  if (status === 409) {
    const refusal = parseKickstartCommitRefusal(jsonOf(path, status, text));
    throw new KickstartCommitRefused(path, refusal.error, refusal.conflicts);
  }
  if (status < 200 || status >= 300) throw refused('POST', path, status, text);
  return parseKickstartCommitted(jsonOf(path, status, text));
}

/**
 * Start the import on the existing route (POST /api/v1/sync/ingest) when the
 * commit reported that an earlier import still runs. It answers 409 while one
 * is running; that is reported, not retried. Progress arrives on the topic.
 */
export async function startKickstartImport(): Promise<{ started: boolean }> {
  const { status, text } = await exchange('POST', INGEST_PATH);
  if (status === 409) return { started: false };
  if (status === 503) throw importUnavailable(text);
  if (status < 200 || status >= 300) throw new KickstartRequestError(INGEST_PATH, status, plainReason(text));
  return { started: true };
}

/**
 * How the last import this server ran stands. The panel asks once after it starts
 * an import and once when its socket comes back, to learn an end it may not have
 * been pushed; it never asks in a loop, because the topic pushes every change.
 */
export async function fetchSyncIngestStatus(): Promise<SyncIngestStatus> {
  // Not a kickstart route: a refusal is the server's plain sentence, not the
  // kickstart contract's closed body.
  const { status, text } = await exchange('GET', INGEST_STATUS_PATH);
  if (status < 200 || status >= 300) throw new KickstartRequestError(INGEST_STATUS_PATH, status, plainReason(text));
  return parseSyncIngestStatus(jsonOf(INGEST_STATUS_PATH, status, text), `GET ${INGEST_STATUS_PATH}`);
}

/**
 * The refusal of an import that peasant has no settings to run under. The
 * server's own words are a status line ("config not available"); on the panel
 * the person needs what failed, that nothing was imported, and what to do.
 */
function importUnavailable(text: string): KickstartRequestError {
  return new KickstartRequestError(
    INGEST_PATH,
    503,
    `the import did not start: peasant answered "${plainReason(text)}", so it has no saved settings to import under. ` +
      'nothing was imported and your saved setup is unchanged. check that peasant\'s settings file is in place and can be read, then choose import now again.',
  );
}
