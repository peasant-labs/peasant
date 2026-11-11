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
 * says what the server said and how to retry; a 409 on commit throws the
 * {@link KickstartCommitRefused} subclass with the rows that blocked the save.
 */

import { getApiBaseUrl } from '@/lib/api/base';
import {
  parseKickstartCommitRefusal,
  parseKickstartCommitted,
  parseKickstartPatchResult,
  parseKickstartPreview,
  parseKickstartScan,
  parseKickstartSession,
  parseKickstartStatus,
  parseKickstartTree,
  type KickstartCommitted,
  type KickstartConflict,
  type KickstartLine,
  type KickstartPatchResult,
  type KickstartScan,
  type KickstartSession,
  type KickstartStatus,
  type KickstartTree,
  type LocalSettingValue,
} from './model';
import type { KickstartConnectAnswer, KickstartResolution, KickstartScope } from './values';

export const KICKSTART_PATH = '/api/v1/kickstart';
const INGEST_PATH = '/api/v1/sync/ingest';

const NO_REASON = 'the server gave no reason; retry, then check the peasant server log';

/** A request the server refused or that could not be completed. `message` is the reason to show. */
export class KickstartRequestError extends Error {
  readonly status: number;
  readonly path: string;

  constructor(path: string, status: number, message: string) {
    super(message);
    this.name = 'KickstartRequestError';
    this.path = path;
    this.status = status;
  }
}

/** A commit the server refused with mine/theirs rows. Nothing was written. */
export class KickstartCommitRefused extends KickstartRequestError {
  readonly conflicts: readonly KickstartConflict[];

  constructor(path: string, message: string, conflicts: readonly KickstartConflict[]) {
    super(path, 409, message);
    this.name = 'KickstartCommitRefused';
    this.conflicts = conflicts;
  }
}

function reasonOf(text: string): string {
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    return text.trim() || NO_REASON;
  }
  if (parsed !== null && typeof parsed === 'object' && typeof (parsed as { error?: unknown }).error === 'string') {
    const reason = ((parsed as { error: string }).error).trim();
    if (reason) return reason;
  }
  return text.trim() || NO_REASON;
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
  if (status < 200 || status >= 300) throw new KickstartRequestError(path, status, reasonOf(text));
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
  if (status < 200 || status >= 300) throw new KickstartRequestError(path, status, reasonOf(text));
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
  if (status < 200 || status >= 300) throw new KickstartRequestError(INGEST_PATH, status, reasonOf(text));
  return { started: true };
}
