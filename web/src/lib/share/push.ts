/**
 * Real Contribute push client. POSTs to /api/v1/sync/push, which runs the SAME
 * push pipeline as `peasant village push` (redact → upload to the configured
 * village/commons), and returns real per-session results. This replaces a
 * simulated push whose fake progress, hardcoded 504, and fake success could
 * incorrectly tell the user that publication succeeded.
 *
 * Honest failure: when the user isn't signed in, the server returns 401 with
 * "not authenticated — run 'peasant village login' first" — surfaced verbatim,
 * not faked.
 */

import { getApiBaseUrl } from '@/lib/api/base';
import type { SelectableRedactionLevel } from '@/lib/share/redactions';

export type PushSessionStatus = 'new' | 'updated' | 'skipped' | 'error';

export interface PushSessionResult {
  sessionId: string;
  status: PushSessionStatus;
  title?: string;
  error?: string;
}

export interface PushResult {
  new: number;
  updated: number;
  skipped: number;
  errors: number;
  sessions: PushSessionResult[];
}

/**
 * Run the real push for the given sessions at the given redaction level. Throws
 * with the server's error message (e.g. the "run 'peasant village login' first"
 * 401) on failure.
 */
export async function runPush(
  sessionIds: string[],
  // Narrowed to the levels this version offers. The endpoint answers 400 for the
  // other two, so accepting them here only moved the refusal to a point where the
  // user has already committed to publishing.
  redactionLevel: SelectableRedactionLevel,
): Promise<PushResult> {
  const resp = await fetch(`${getApiBaseUrl()}/api/v1/sync/push`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      sessionIds,
      redactionLevel,
      // The commons is public by design (the wizard frames it so).
      visibility: 'public',
    }),
  });
  if (!resp.ok) {
    const body = (await resp.json().catch(() => null)) as { error?: string } | null;
    throw new Error(body?.error || `push failed (${resp.status})`);
  }
  return (await resp.json()) as PushResult;
}
