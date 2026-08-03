/**
 * Change-detail caption assembly: the deterministic sentence
 * built client-side from payload facts —
 *
 *   "+2 import edges (1 wrong-way ⚠) · 14 files in ingest, api ·
 *    3 sessions, 21 tasks · retry loops in 2 tasks"
 *
 * Every fragment carries the anchor of its evidence section; no adjectives
 * anywhere. The warning glyph is NOT part of the text — the renderer places a
 * Lucide TriangleAlert after the edges fragment when `wrongWay > 0` (icons
 * are Lucide-only per the design system).
 */

import type { FileChange } from '@peasant-labs/schema';
import type { DecodedChangeDetailPayload } from '@/lib/api/map';
import { MINUS, plural } from './format';

/** Evidence-section anchors inside the change-detail page. */
export type CaptionAnchor = 'review-slice' | 'review-files' | 'review-work';

export interface CaptionFragment {
  key: 'edges' | 'files' | 'work' | 'retry';
  /** The fragment's plain text — facts only, no adjectives, no glyphs. */
  text: string;
  /** The evidence section this fragment scrolls/drills to. */
  anchor: CaptionAnchor;
  /** Wrong-way violation count (edges fragment only) — renderer adds the glyph. */
  wrongWay?: number;
}

/**
 * The top modules touched by the changed files: each file is grouped by its
 * module — the first two path segments of its containing directory
 * ("internal/codemap/sub/x.go" → "internal/codemap"; a top-level directory
 * like "ingest/x.go" → "ingest", which keeps the compact form "14 files in
 * ingest, api" producible) — and the top `cap` modules by file count win
 * (ties break alphabetically for determinism). Deeper subdirectories collapse
 * into their module, so leaf noise like "[[...segments]]" or "v2" never shows.
 * Root-level files count in the file total but name no module.
 */
export function topModules(files: readonly Pick<FileChange, 'path'>[], cap = 2): string[] {
  const counts = new Map<string, number>();
  for (const f of files) {
    const slash = f.path.lastIndexOf('/');
    if (slash <= 0) continue;
    const dir = f.path.slice(0, slash);
    const module = dir.split('/').slice(0, 2).join('/');
    counts.set(module, (counts.get(module) ?? 0) + 1);
  }
  return [...counts.entries()]
    .sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
    .slice(0, cap)
    .map(([module]) => module);
}

/** Count of tasks flagged with a retry loop across all work sessions. */
export function retryTaskCount(payload: DecodedChangeDetailPayload): number {
  let n = 0;
  for (const ws of payload.work) {
    for (const t of ws.tasks) if (t.retryLoop) n++;
  }
  return n;
}

/**
 * Assemble the caption fragments from payload facts. Omission rules (facts
 * only, no zero-noise): the retry fragment is omitted when no task has a
 * retry loop; the sessions/tasks fragment always renders (zero recorded work
 * is itself the fact that matters).
 */
export function buildCaption(payload: DecodedChangeDetailPayload): CaptionFragment[] {
  const fragments: CaptionFragment[] = [];

  // Edges — new/removed import-edge deltas plus wrong-way violations.
  const added = payload.newEdges.length;
  const removed = payload.removedEdges.length;
  const wrongWay = payload.violations.filter((v) => v.kind === 'wrong_way').length;
  let edgesText: string;
  if (added > 0 && removed > 0) {
    edgesText = `+${added}/${MINUS}${removed} connections`;
  } else if (added > 0) {
    edgesText = `+${plural(added, 'connection')}`;
  } else if (removed > 0) {
    edgesText = `${MINUS}${plural(removed, 'connection')}`;
  } else {
    edgesText = 'no connection changes';
  }
  if (wrongWay > 0) edgesText += ` (${plural(wrongWay, 'rule break')})`;
  fragments.push({ key: 'edges', text: edgesText, anchor: 'review-slice', wrongWay });

  // Files — count + the top-2 modules they live in.
  const fileCount = payload.files.length;
  if (fileCount === 0) {
    fragments.push({ key: 'files', text: 'no file changes', anchor: 'review-files' });
  } else {
    const modules = topModules(payload.files);
    fragments.push({
      key: 'files',
      text: modules.length
        ? `${plural(fileCount, 'file')} in ${modules.join(', ')}`
        : plural(fileCount, 'file'),
      anchor: 'review-files',
    });
  }

  // Work — sessions and tasks behind the change (always stated).
  const sessionCount = payload.work.length;
  const taskCount = payload.work.reduce((sum, ws) => sum + ws.tasks.length, 0);
  fragments.push({
    key: 'work',
    text: `${plural(sessionCount, 'conversation')}, ${plural(taskCount, 'request')}`,
    anchor: 'review-work',
  });

  // Retry loops — only when at least one task is flagged.
  const retries = retryTaskCount(payload);
  if (retries > 0) {
    fragments.push({
      key: 'retry',
      text: `${plural(retries, 'request')} took several attempts`,
      anchor: 'review-work',
    });
  }

  return fragments;
}

/** The caption as one plain string (fragments joined with " · "). */
export function captionText(payload: DecodedChangeDetailPayload): string {
  return buildCaption(payload)
    .map((f) => f.text)
    .join(' · ');
}
