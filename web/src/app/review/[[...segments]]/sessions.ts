/**
 * Project resolution for the Changes surface. Route
 * stays /review).
 *
 * The `sessions` WS channel is the frontend's only project index: rows carry
 * the display `project` name plus the opaque `projectHash` the REST
 * endpoints key on. Routes keep using the display name; the hash is resolved
 * client-side here. Home (`/`) is the project picker, so this
 * module only needs the distinct names — enough to auto-resolve a single
 * project and to word the no-project pointer.
 */

import type { SessionSummary } from '@/types/messages';
import { parseProjectHash, type ProjectHash } from '@/lib/navigation/projectRoutes';

/** Fallback bucket for sessions without a project name (mirrors the Map home). */
export const UNASSIGNED_PROJECT = 'Unassigned';

/** The distinct project names on the sessions channel. */
export function projectNames(sessions: SessionSummary[]): string[] {
  const names = new Set<string>();
  for (const s of sessions) names.add(s.project ?? UNASSIGNED_PROJECT);
  return [...names];
}

/**
 * Read `SessionSummary.projectHash` from the sessions-channel row. Empty-string
 * hashes (a server that predates the field) normalize to undefined.
 */
export function sessionProjectHash(s: SessionSummary): string | undefined {
  return s.projectHash || undefined;
}

/**
 * Resolve a display project name to its opaque projectHash from the sessions
 * channel. Returns null when no session of that project carries a hash (e.g.
 * a server that predates the field).
 */
export function resolveProjectHash(
  sessions: SessionSummary[],
  projectName: string,
): ProjectHash | null {
  for (const s of sessions) {
    if ((s.project ?? UNASSIGNED_PROJECT) !== projectName) continue;
    const hash = sessionProjectHash(s);
    const projectHash = parseProjectHash(hash);
    if (projectHash) return projectHash;
  }
  return null;
}
