/**
 * Pure derivations for the Changes surface's lifecycle badge and the
 * change-detail signal band. No React, no fetching, no verdict
 * language — these summarize facts already in the wire payloads.
 */

import type { ChangeSummary, FileChange } from '@peasant-labs/schema';
import type { DecodedChangeDetailPayload } from '@/lib/api/map';

type ChangeSession = DecodedChangeDetailPayload['work'][number];

// ---------------------------------------------------------------------------
// File-first changed-files view (the git diff leads; transcripts are helpers).
// Each changed file is paired with the recorded conversations that edited it,
// then grouped by top-level directory for a scannable, accurate file list.
// ---------------------------------------------------------------------------

/** One changed file plus the recorded conversations that edited its exact path. */
export interface FileWithConversations {
  file: FileChange;
  /** Sessions whose tasks edited this path (the transcript helper link). */
  sessions: ChangeSession[];
}

/** A directory group of changed files (top-level path segment). */
export interface FileGroup {
  /** Top-level directory, or "(root)" for files with no directory. */
  dir: string;
  files: FileWithConversations[];
}

/**
 * The git path's module group — its first two path segments, matching the
 * caption's module grouping ("internal/ingest/x.go" → "internal/ingest";
 * "web/src/lib/a.ts" → "web/src"). Root files (no directory) group under "(root)".
 */
function topDir(path: string): string {
  const segs = path.split('/').filter(Boolean);
  if (segs.length <= 1) return '(root)';
  return segs.slice(0, 2).join('/');
}

/** Pair every changed file with the conversations that edited its exact path. */
export function filesWithConversations(
  payload: DecodedChangeDetailPayload,
): FileWithConversations[] {
  return payload.files.map((file) => ({
    file,
    sessions: payload.work.filter((ws) =>
      ws.tasks.some((t) => t.editedFiles.includes(file.path)),
    ),
  }));
}

/**
 * Changed files grouped by top-level directory, directories sorted by file
 * count (descending, then name); files within a group keep payload order.
 */
export function groupChangedFiles(payload: DecodedChangeDetailPayload): FileGroup[] {
  const byDir = new Map<string, FileWithConversations[]>();
  for (const fwc of filesWithConversations(payload)) {
    const dir = topDir(fwc.file.path);
    const list = byDir.get(dir);
    if (list) list.push(fwc);
    else byDir.set(dir, [fwc]);
  }
  return Array.from(byDir.entries())
    .map(([dir, files]) => ({ dir, files }))
    .sort((a, b) => b.files.length - a.files.length || a.dir.localeCompare(b.dir));
}

// ---------------------------------------------------------------------------
// Change-detail signal summary: activity tiles plus neutral friction tiles.
// ---------------------------------------------------------------------------

export interface ChangeSignalSummary {
  /** Recorded sessions behind this change (bound + candidate). */
  sessions: number;
  /** Total tasks (user requests) across those sessions. */
  tasks: number;
  /** Tasks that hit an error/retry streak — a neutral friction signal. */
  retryLoops: number;
  /** NEW layering violations this change introduced. */
  violations: number;
  /** Most recent task start across the work, or null when none is dated. */
  lastActivityMs: number | null;
}

/** Fold a change's recorded work into the detail signal summary. */
export function summarizeChange(payload: DecodedChangeDetailPayload): ChangeSignalSummary {
  let tasks = 0;
  let retryLoops = 0;
  let lastActivityMs: number | null = null;
  for (const ws of payload.work) {
    for (const t of ws.tasks) {
      tasks++;
      if (t.retryLoop) retryLoops++;
      if (t.startMs != null && (lastActivityMs === null || t.startMs > lastActivityMs)) {
        lastActivityMs = t.startMs;
      }
    }
  }
  return {
    sessions: payload.work.length,
    tasks,
    retryLoops,
    violations: payload.violations.length,
    lastActivityMs,
  };
}

// ---------------------------------------------------------------------------
// Open-change lifecycle badge.
// ---------------------------------------------------------------------------

export type ChangeLifecycle = 'active' | 'idle' | 'stale';

export interface LifecycleBadge {
  key: ChangeLifecycle;
  label: string;
}

const DAY_MS = 86_400_000;
/** ≤3 days since last work → active. */
const ACTIVE_MAX_MS = 3 * DAY_MS;
/** ≤14 days → idle; beyond → stale. */
const IDLE_MAX_MS = 14 * DAY_MS;

/**
 * Recency lifecycle for an OPEN change, from its last recorded work (falling
 * back to the branch tip commit time). Returns null when the change carries no
 * timestamp at all — the badge is then omitted rather than guessed. Merged
 * changes are not classified here (they render as "folded in" chips), and
 * "new"/"reverted" are deliberately not inferred: the wire payload carries no
 * branch-creation time or revert marker, so claiming them would be a guess.
 */
export function openChangeLifecycle(
  change: ChangeSummary,
  nowMs: number,
): LifecycleBadge | null {
  const ms = change.lastWorkMs ?? change.tipCommitMs ?? null;
  if (ms === null) return null;
  const age = Math.max(0, nowMs - ms);
  if (age <= ACTIVE_MAX_MS) return { key: 'active', label: 'active' };
  if (age <= IDLE_MAX_MS) return { key: 'idle', label: 'idle' };
  return { key: 'stale', label: 'stale' };
}
