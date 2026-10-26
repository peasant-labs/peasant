import { getApiBaseUrl } from './base';

export interface LocalContract<T> {
  safeParse(value: unknown):
    | { success: true; data: T }
    | { success: false; error: { issues: readonly { message: string }[] } };
}

/** A refusal is not a local outage. Only status zero means no local HTTP answer. */
export class LocalRequestError extends Error {
  constructor(readonly path: string, readonly status: number, message: string) {
    super(message);
    this.name = 'LocalRequestError';
  }
}

export interface LocalAnswer {
  status: number;
  ok: boolean;
  body: unknown;
}

/** Read the body BEFORE deciding success: some Local API errors retain committed results. */
export async function readLocalAnswer(path: string, init?: RequestInit): Promise<LocalAnswer> {
  let response: Response;
  try {
    response = await fetch(`${getApiBaseUrl()}${path}`, { cache: 'no-store', ...init });
  } catch (error) {
    if (init?.signal?.aborted) throw error;
    throw new LocalRequestError(path, 0,
      `peasant did not answer ${path}. check that the local peasant server is running, then retry.`);
  }
  let body: unknown;
  try {
    body = await response.json();
  } catch (error) {
    if (init?.signal?.aborted) throw error;
    throw new LocalRequestError(path, response.status,
      `peasant returned unreadable JSON for ${path} (HTTP ${response.status}). retry; if it repeats, check the local server log.`);
  }
  return { status: response.status, ok: response.ok, body };
}

export function decodeLocalAnswer<T>(path: string, answer: LocalAnswer, contract: LocalContract<T>): T {
  const parsed = contract.safeParse(answer.body);
  if (!parsed.success) {
    throw new LocalRequestError(path, answer.status,
      `peasant's response to ${path} breaks the Local API contract: ${parsed.error.issues[0]?.message ?? 'unrecognized response'}. prior data is retained; update peasant and retry.`);
  }
  return parsed.data;
}

export function localRefusal(path: string, answer: LocalAnswer): LocalRequestError {
  const body = answer.body;
  const reason = body && typeof body === 'object' && 'error' in body && typeof body.error === 'string'
    ? body.error : 'the local server gave no reason; check its log and retry';
  return new LocalRequestError(path, answer.status, `${path} was refused (HTTP ${answer.status}): ${reason}`);
}
