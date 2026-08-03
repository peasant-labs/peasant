/**
 * Pure data helpers for the Map surface.
 *
 * Everything here is a pure function of its inputs — no fetching, no React.
 * The page (`MapPageClient.tsx`) wires these against the `sessions` WS
 * channel and the Map REST client (`@/lib/api/map`).
 *
 * Wire → datum mapping: the canvas library (`@/components/map`) deliberately
 * defines its own prop types that match the wire contract field-for-field;
 * the mappers below are the single adaptation point at the page boundary.
 */

import {
  type ActivityEdge,
  type CommitRef,
  type MapEdge,
  type MapNode,
} from '@peasant-labs/schema';
import type {
  EdgeViolationDatum,
  MapEdgeDatum,
  MapNodeDatum,
  TimeStripDay,
} from '@/components/map';
import type { SessionSummary } from '@/types/messages';
import { MapNodeParam } from '@/components/session-detail/v2/lib/scopeTurns';
import type { ProjectHash } from '@/lib/navigation/projectRoutes';
import type {
  DecodedEdgeViolation,
  DecodedMapGraphPayload,
  DecodedMapNode,
  DecodedTaskSummary,
} from '@/lib/api/map';

/**
 * Query-param names read by the Map page (`/map/{project}?node=`).
 * `Node` re-exports the viewer-owned `MapNodeParam` (scopeTurns.ts) — one
 * source of truth for the deep-link param that the touched-files panel and
 * the Map origin crumb emit. Values are repo-relative `MapNode.id` paths on
 * both ends. (The v2 `?lens=` param died with the lens switcher.)
 */
export const MapParam = {
  Project: 'project',
  Node: MapNodeParam,
  Mode: 'mode',
  Grain: 'grain',
  Expand: 'expand',
  Filter: 'filter',
  Focus: 'focus',
  Scale: 'scale',
  PanX: 'panX',
  PanY: 'panY',
} as const;

// ---------------------------------------------------------------------------
// Wire payload → canvas datum mapping
// ---------------------------------------------------------------------------

/** One wire node → one canvas datum (field-for-field). */
export function mapWireToDatum(node: DecodedMapNode): MapNodeDatum {
  return {
    id: node.id,
    parent: node.parent || undefined,
    kind: node.kind,
    name: node.name,
    language: node.language,
    layer: node.layer,
    order: node.order,
    loc: node.loc,
    fileCount: node.fileCount,
    recordedFiles: node.recordedFiles,
    totalFiles: node.totalFiles,
    touchCount: node.touchCount,
    effortDensity: node.effortDensity,
    agentEditedCount: node.agentEditedCount,
    readCount: node.readCount,
    readAttribution: node.readAttribution,
    readState: node.readState,
    changedRegionCount: node.changedRegionCount,
    attributedRegionCount: node.attributedRegionCount,
    reviewedRegionCount: node.reviewedRegionCount,
  };
}

/** Validate and adapt a schema edge-violation value for the canvas. */
export function mapViolationToDatum(v: DecodedEdgeViolation): EdgeViolationDatum {
  return {
    kind: v.kind,
    from: v.from,
    to: v.to,
  };
}

/**
 * The graph payload adapted into canvas prop arrays. Activity edges are NOT
 * part of the canvas data because the Map has one structural view — the payload's
 * `activityEdges` feed the node panel's "Often edited with" rows instead
 * (`coupledNodes`).
 */
export interface MapGraphData {
  nodes: MapNodeDatum[];
  structureEdges: MapEdgeDatum[];
  violations: EdgeViolationDatum[];
}

/** Adapt a decoded map response for `MapCanvas` (the page boundary mapper). */
export function mapGraphToData(payload: DecodedMapGraphPayload): MapGraphData {
  return {
    nodes: payload.nodes.map(mapWireToDatum),
    structureEdges: payload.structureEdges.map(
      (e: MapEdge): MapEdgeDatum => ({ from: e.from, to: e.to, count: e.count }),
    ),
    violations: payload.violations.map(mapViolationToDatum),
  };
}

// ---------------------------------------------------------------------------
// Sessions channel helpers (name → hash resolution, rail list, sparkline)
// ---------------------------------------------------------------------------

/**
 * Fallback bucket name for sessions without a project (the same bucket the
 * home picker groups under). Every helper that compares a session's project
 * against a route name MUST normalize through this constant — otherwise the
 * `/map/Unassigned` route can never match its own sessions.
 */
export const UNASSIGNED_PROJECT = 'Unassigned';

/**
 * Sessions of one project in reverse chronology. Every session
 * appears, including zero-touch ones — no filtering beyond the project).
 */
export function projectSessions(
  sessions: readonly SessionSummary[],
  projectHash: ProjectHash,
): SessionSummary[] {
  return sessions
    .filter((s) => s.projectHash === projectHash)
    .sort((a, b) => b.startTime.localeCompare(a.startTime));
}

/** Local `YYYY-MM-DD` key for a date (the TimeStrip day-key convention). */
function localDayKey(d: Date): string {
  const y = d.getFullYear();
  const m = String(d.getMonth() + 1).padStart(2, '0');
  const day = String(d.getDate()).padStart(2, '0');
  return `${y}-${m}-${day}`;
}

/**
 * Sessions/day sparkline data: `dayCount` consecutive local days ending at
 * `nowMs` (oldest → newest, the TimeStrip orientation). Days with no
 * sessions are present with 0 so the silhouette keeps its timeline.
 *
 * Iterates CALENDAR days (`setDate(getDate() - 1)`), not fixed 24h steps —
 * local days are 23/25 hours across DST transitions, and millisecond
 * stepping emits duplicate or skipped day keys there (duplicate React keys
 * in the TimeStrip, vanished days in the silhouette).
 */
export function sessionsPerDay(
  sessions: readonly SessionSummary[],
  dayCount: number,
  nowMs: number,
): TimeStripDay[] {
  const counts = new Map<string, number>();
  for (const s of sessions) {
    const t = new Date(s.startTime);
    if (Number.isNaN(t.getTime())) continue;
    const key = localDayKey(t);
    counts.set(key, (counts.get(key) ?? 0) + 1);
  }
  const keys: string[] = [];
  const cursor = new Date(nowMs);
  for (let i = 0; i < dayCount; i++) {
    keys.push(localDayKey(cursor));
    cursor.setDate(cursor.getDate() - 1);
  }
  keys.reverse();
  return keys.map((key) => ({ date: key, sessions: counts.get(key) ?? 0 }));
}

// ---------------------------------------------------------------------------
// Resolve a sparkline day to the default-branch
// commit whose tree the map graph should render as-of (`fetchMapGraph` ?commit=).
// ---------------------------------------------------------------------------

/** End-of-day local ms for a `YYYY-MM-DD` key — the scrub cutoff for that day. */
export function localDayEndMs(dateKey: string): number {
  const [y, m, d] = dateKey.split('-').map(Number);
  return new Date(y, m - 1, d, 23, 59, 59, 999).getTime();
}

/**
 * The most recent commit at or before `atMs`, or null when none qualifies
 * (every commit is newer — `atMs` predates the project's first commit).
 * Commits without a timestamp are skipped. Scrubbing a day maps it through
 * `localDayEndMs` to this commit, whose hash becomes the graph's `?commit=`.
 */
export function commitAtOrBefore(
  commits: readonly CommitRef[],
  atMs: number,
): CommitRef | null {
  let best: CommitRef | null = null;
  for (const c of commits) {
    if (c.timeMs == null) continue;
    if (c.timeMs <= atMs && (best === null || c.timeMs > (best.timeMs ?? 0))) {
      best = c;
    }
  }
  return best;
}

// ---------------------------------------------------------------------------
// Coverage: "N of M files recorded"
// ---------------------------------------------------------------------------

export interface Coverage {
  recorded: number;
  total: number;
}

/**
 * Project-level traceability coverage summed from the graph's ROOT nodes
 * only — `recordedFiles`/`totalFiles` already roll up the tree, so summing
 * every node would double-count files.
 */
export function projectCoverage(nodes: readonly MapNode[]): Coverage {
  let recorded = 0;
  let total = 0;
  for (const n of nodes) {
    if (n.parent) continue;
    recorded += n.recordedFiles;
    total += n.totalFiles;
  }
  return { recorded, total };
}

// ---------------------------------------------------------------------------
// Co-edit coupling for the node panel's "Often edited with" rows
// ---------------------------------------------------------------------------

/** One "Often edited with" row: the other endpoint of a co-edit observation. */
export interface CoupledNode {
  /** The OTHER node's id (repo-relative path). */
  id: string;
  /** Distinct tasks that edited both endpoints. */
  taskCount: number;
}

/** Row cap for the node panel's "Often edited with" list. */
export const MAX_COUPLED_NODES = 5;

/**
 * The nodes most often co-edited with `nodeId`, derived from the graph
 * payload's `activityEdges` (which the server aggregates per node pair).
 * Top `limit` by shared-task count, descending; the sort is stable so
 * payload order breaks ties. This replaces the v2 Activity lens — co-edit
 * coupling reads as plain rows, not canvas edges.
 */
export function coupledNodes(
  edges: readonly ActivityEdge[],
  nodeId: string,
  limit: number = MAX_COUPLED_NODES,
): CoupledNode[] {
  const rows: CoupledNode[] = [];
  for (const e of edges) {
    if (e.from === nodeId) rows.push({ id: e.to, taskCount: e.taskCount });
    else if (e.to === nodeId) rows.push({ id: e.from, taskCount: e.taskCount });
  }
  return rows.sort((a, b) => b.taskCount - a.taskCount).slice(0, limit);
}

// ---------------------------------------------------------------------------
// Touched modules (the task rows' "touched" line — which modules a task edited)
// ---------------------------------------------------------------------------

/** Module cap for a task row's "touched" line. */
export const MAX_TOUCHED_MODULES = 3;

/**
 * The top modules a task's edits touched, for the rail task rows' "touched"
 * line. Each edited file maps to its first-two-path-segment module
 * ("internal/codemap/review.go" → "internal/codemap"; root files keep their
 * single segment). Modules rank by edited-file count descending — the sort
 * is stable, so first-edit order breaks ties — capped at `limit`.
 */
export function touchedModules(
  editedFiles: readonly string[],
  limit: number = MAX_TOUCHED_MODULES,
): string[] {
  const counts = new Map<string, number>();
  for (const file of editedFiles) {
    const segments = file.split('/').filter(Boolean);
    if (segments.length === 0) continue;
    const mod = segments.slice(0, 2).join('/');
    counts.set(mod, (counts.get(mod) ?? 0) + 1);
  }
  return Array.from(counts.entries())
    .sort((a, b) => b[1] - a[1])
    .slice(0, limit)
    .map(([mod]) => mod);
}

// ---------------------------------------------------------------------------
// Node search for the toolbar combobox
// ---------------------------------------------------------------------------

/** Result cap for the toolbar's node-search dropdown. */
export const MAX_SEARCH_RESULTS = 8;

/**
 * Case-insensitive node search over ids (repo-relative paths) and display
 * names, capped at `limit` results in payload (= deterministic layout) order.
 * An empty/whitespace query matches nothing — the dropdown stays closed.
 */
export function searchMapNodes<T extends { id: string; name: string }>(
  nodes: readonly T[],
  query: string,
  limit: number = MAX_SEARCH_RESULTS,
): T[] {
  const q = query.trim().toLowerCase();
  if (!q) return [];
  const out: T[] = [];
  for (const node of nodes) {
    if (node.id.toLowerCase().includes(q) || node.name.toLowerCase().includes(q)) {
      out.push(node);
      if (out.length >= limit) break;
    }
  }
  return out;
}

// ---------------------------------------------------------------------------
// Interleave tasks and commits into one reverse-chronological "shaped by" list
// ---------------------------------------------------------------------------

export type ShapedByRow =
  | { kind: 'task'; task: DecodedTaskSummary; timeMs: number | null }
  | { kind: 'commit'; commit: CommitRef; timeMs: number | null };

/**
 * Merge the node's shaped-by tasks with its recent commits into one
 * reverse-chronological list. Rows without a timestamp sink to the bottom;
 * the sort is stable so payload order breaks ties.
 */
export function interleaveShapedBy(
  tasks: readonly DecodedTaskSummary[],
  commits: readonly CommitRef[],
): ShapedByRow[] {
  const rows: ShapedByRow[] = [
    ...tasks.map((task): ShapedByRow => ({ kind: 'task', task, timeMs: task.startMs ?? null })),
    ...commits.map(
      (commit): ShapedByRow => ({ kind: 'commit', commit, timeMs: commit.timeMs ?? null }),
    ),
  ];
  // Array.prototype.sort is stable; null timestamps order last.
  return rows.sort((a, b) => {
    if (a.timeMs === null && b.timeMs === null) return 0;
    if (a.timeMs === null) return 1;
    if (b.timeMs === null) return -1;
    return b.timeMs - a.timeMs;
  });
}

/** Distinct session IDs behind a list of tasks, payload order preserved. */
export function shapedBySessionIds(tasks: readonly DecodedTaskSummary[]): string[] {
  const out: string[] = [];
  const seen = new Set<string>();
  for (const t of tasks) {
    if (seen.has(t.sessionId)) continue;
    seen.add(t.sessionId);
    out.push(t.sessionId);
  }
  return out;
}

// ---------------------------------------------------------------------------
// Links (viewer / Contribute / Review) — exact query-param contracts
// ---------------------------------------------------------------------------

/**
 * Evidence-set Contribute link: `/share?sessions=<id,id>` — the
 * wizard opens Choose FILTERED to these sessions, never preselected.
 */
export function contributeSessionsHref(sessionIds: readonly string[]): string {
  const distinct = Array.from(new Set(sessionIds));
  return `/share?sessions=${distinct.map(encodeURIComponent).join(',')}`;
}
