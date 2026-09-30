import {
  zAutoPublishRemovalResponse,
  zAutoPublishRepository,
  zAutoPublishRule,
  zLocalSetting,
  zLocalSettingRefusal,
  zLocalSettingsResponse,
  zLocalVillageCollectivesResponse,
  zSyncAuthResponse,
  zSyncLogoutResponse,
  type AutoPublishRemovalResponse,
  type AutoPublishRepository,
  type AutoPublishRule,
  type AutoPublishRuleRequest,
  type LocalSetting,
  type LocalSettingsResponse,
  type LocalVillageCollectivesResponse,
  type SyncAuthResponse,
  type SyncLogoutResponse,
} from '@peasant-labs/schema';
import { getApiBaseUrl } from './base';

/**
 * The settings page's calls: read every setting and auto-publish rule, change
 * one setting, save, remove or install one rule, and read or end the Village
 * sign-in. Every answer is checked against the Local API contract before the
 * page uses it; an answer that breaks it is an error, never a guess.
 */

/** A request the server refused or that could not be completed. `message` is the reason to show. */
export class SettingsRequestError extends Error {
  readonly status: number;
  readonly path: string;

  constructor(path: string, status: number, message: string) {
    super(message);
    this.name = 'SettingsRequestError';
    this.path = path;
    this.status = status;
  }
}

const NO_DETAIL = 'the server gave no reason; retry, then check the peasant server log.';

/** The reason a refused request names: a LocalSettingRefusal, an `{error}` envelope, or the text. */
function reasonOf(body: string): string {
  let parsed: unknown;
  try {
    parsed = JSON.parse(body);
  } catch {
    return body.trim() || NO_DETAIL;
  }
  const refusal = zLocalSettingRefusal.safeParse(parsed);
  if (refusal.success) return refusal.data.error;
  if (parsed && typeof parsed === 'object' && typeof (parsed as { error?: unknown }).error === 'string') {
    const text = ((parsed as { error: string }).error).trim();
    if (text) return text;
  }
  return body.trim() || NO_DETAIL;
}

/** The part of a generated contract validator this module uses. */
interface Contract<T> {
  safeParse(value: unknown):
    | { success: true; data: T }
    | { success: false; error: { issues: readonly { message: string }[] } };
}

async function call<T>(path: string, schema: Contract<T>, init?: RequestInit): Promise<T> {
  let response: Response;
  try {
    response = await fetch(`${getApiBaseUrl()}${path}`, {
      ...init,
      headers: init?.body ? { 'Content-Type': 'application/json' } : undefined,
    });
  } catch (error) {
    const detail = error instanceof Error ? error.message : String(error);
    throw new SettingsRequestError(path, 0, `peasant did not answer (${detail}). check that peasant is running, then retry.`);
  }
  const text = await response.text();
  if (!response.ok) throw new SettingsRequestError(path, response.status, reasonOf(text));
  let body: unknown;
  try {
    body = JSON.parse(text);
  } catch {
    throw new SettingsRequestError(path, response.status, `peasant answered ${path} with something that is not JSON. retry; if it repeats, report it.`);
  }
  const parsed = schema.safeParse(body);
  if (!parsed.success) {
    throw new SettingsRequestError(path, response.status, `peasant's answer to ${path} breaks the Local API contract, so it is not shown: ${parsed.error.issues[0]?.message ?? 'unknown shape'}.`);
  }
  return parsed.data;
}

export const SETTINGS_PATH = '/api/v1/settings';
export const AUTO_PUBLISH_PATH = '/api/v1/settings/auto-publish';
export const SYNC_AUTH_PATH = '/api/v1/sync/auth';
export const SYNC_LOGOUT_PATH = '/api/v1/sync/logout';
export const VILLAGE_COLLECTIVES_PATH = '/api/v1/village/collectives';

/** GET /api/v1/settings: every setting and every auto-publish rule. */
export function fetchSettings(): Promise<LocalSettingsResponse> {
  return call(SETTINGS_PATH, zLocalSettingsResponse);
}

/**
 * PATCH /api/v1/settings: change one key. `null` unsets it, so its default
 * applies. Resolves with the saved row; a refusal rejects with its reason.
 */
export function updateSetting(key: string, value: unknown): Promise<LocalSetting> {
  return call(SETTINGS_PATH, zLocalSetting, {
    method: 'PATCH',
    body: JSON.stringify({ key, value: value === undefined ? null : value }),
  });
}

function rulePath(id: string): string {
  return `${AUTO_PUBLISH_PATH}/${encodeURIComponent(id)}`;
}

/** PUT /api/v1/settings/auto-publish/{id}: create or replace one rule. It installs nothing. */
export function saveAutoPublishRule(id: string, request: AutoPublishRuleRequest): Promise<AutoPublishRule> {
  return call(rulePath(id), zAutoPublishRule, {
    method: 'PUT',
    body: JSON.stringify(request),
  });
}

/** DELETE /api/v1/settings/auto-publish/{id}: remove one rule. It changes no hook. */
export function removeAutoPublishRule(id: string): Promise<AutoPublishRemovalResponse> {
  return call(rulePath(id), zAutoPublishRemovalResponse, { method: 'DELETE' });
}

/** POST /api/v1/settings/auto-publish/{id}/install: install one rule's hooks in one recorded repository. */
export function installAutoPublishRule(id: string, path: string): Promise<AutoPublishRepository> {
  return call(`${rulePath(id)}/install`, zAutoPublishRepository, {
    method: 'POST',
    body: JSON.stringify({ path }),
  });
}

/** GET /api/v1/sync/auth: whether this computer is signed in to Village, and as whom. */
export function fetchVillageAuth(): Promise<SyncAuthResponse> {
  return call(SYNC_AUTH_PATH, zSyncAuthResponse);
}

/** POST /api/v1/sync/logout: remove this computer's stored Village credential. */
export function logOutOfVillage(): Promise<SyncLogoutResponse> {
  return call(SYNC_LOGOUT_PATH, zSyncLogoutResponse, { method: 'POST' });
}

/** GET /api/v1/village/collectives: the collectives the signed-in user belongs to. */
export function fetchVillageCollectives(): Promise<LocalVillageCollectivesResponse> {
  return call(VILLAGE_COLLECTIVES_PATH, zLocalVillageCollectivesResponse);
}
