import { prefilterTurns as packagePrefilterTurns } from '@peasant-labs/fairtrade/ui';
import type { TurnDetail, ToolCallDetail } from '@/types/messages';
import { ToolCallKind } from '@/types/messages';
import {
  mapHref,
  reviewHref,
  RouteOrigin,
  TranscriptScope,
  type ProjectHash,
  type TranscriptRouteQuery,
} from '@/lib/navigation/projectRoutes';

/**
 * Viewer scoping for task, file, and change deep links.
 *
 * Pure functions only — the SessionDetailV2 adapter parses the URL query
 * params and calls into this module; everything here is table-testable
 * without React or a router.
 *
 * Query-param contract:
 *   ?scope=task&scopeVal=<entryIndex>   — turns from that user turn until the
 *                                          next depth-0 user turn
 *   ?scope=file&scopeVal=<path>         — turns with a tool call on that file
 *                                          (edits count, reads included)
 *   ?scope=change&scopeVal=<i,i,…>      — union of the task slices at those
 *                                          entry indices (the change's tasks in
 *                                          this session; the Review surface
 *                                          supplies the indices)
 *   ?origin=Map|Review (+originNode / +originBranch) — origin breadcrumb
 */

// -- Typed query-param constants ----------------------------------------------

/** Query-param names owned by the scoping layer. */
export const ScopeParam = {
  Scope: 'scope',
  ScopeVal: 'scopeVal',
  Origin: 'origin',
  OriginNode: 'originNode',
  OriginBranch: 'originBranch',
  ReturnTo: 'returnTo',
} as const;

/**
 * Query param the Map page reads to focus/select a node:
 * `/map/{project}?node=<repo-relative path>`. The viewer is the link *source*
 * (touched-files panel rows, Map origin crumb), so the param is named and
 * documented here; the Map surface should import this constant when it wires
 * selection-restore from the URL instead of re-typing `'node'`. Values are
 * repo-relative paths matching `MapNode.id` (e.g. `internal/ingest`,
 * `web/src/lib/api.ts`) — see `relativizePath` / `collectFileTouches`.
 */
export const MapNodeParam = 'node';

/**
 * Query-param name for the viewer's turn deep link:
 * `/projects/{project}/{sessionId}?turn=<entryIndex>` — positions the viewer
 * on that turn WITHOUT filtering (the scope params filter; this one never
 * does). The viewer both reads it (initial position) and writes it (per-turn
 * copied anchors); the Map's task links emit it. One constant, no re-typed
 * 'turn' literals.
 */
export const TurnParam = 'turn';

// -- Path reconciliation ---------------------------------------------------------
//
// Map node ids are repo-relative (`internal/ingest/pipeline.go`) but tool-call
// `filePath`s are raw wire values — absolute on Claude Code data
// (`/Users/me/peasant/internal/ingest/pipeline.go`). Everything that crosses
// the viewer↔Map boundary goes through these two helpers so links carry
// node-matchable values and file-scope matching tolerates the split.

/**
 * Strip the session's working directory prefix from an absolute tool-call
 * path, yielding the repo-relative form Map nodes use. Paths outside the
 * working directory (or when none is known) are returned unchanged — the
 * suffix-tolerant `pathsMatch` is the fallback for those.
 */
export function relativizePath(filePath: string, workingDirectory?: string): string {
  if (!workingDirectory) return filePath;
  const wd = workingDirectory.endsWith('/') ? workingDirectory : `${workingDirectory}/`;
  return filePath.startsWith(wd) ? filePath.slice(wd.length) : filePath;
}

function isAbsoluteFilePath(filePath: string): boolean {
  return filePath.startsWith('/') || filePath.startsWith('\\\\') || /^[A-Za-z]:[\\/]/.test(filePath);
}

/**
 * True when two paths plausibly refer to the same file, tolerating the
 * absolute-vs-repo-relative split: exact equality, or the longer path ending
 * with `/` + the shorter (a whole-segment suffix, so `bar/api.ts` never
 * matches `sugar/api.ts`). Empty strings never match.
 */
export function pathsMatch(a: string, b: string): boolean {
  if (!a || !b) return false;
  if (a === b) return true;
  if (a.length > b.length) return a.endsWith(`/${b}`);
  return b.endsWith(`/${a}`);
}

// -- Touch classification ------------------------------------------------------

/**
 * Tool kinds that mutate a file. Mirrors the backend Touch rule:
 * edits are what attribution counts; everything else carrying a filePath is
 * context. Delete/move are file mutations, so they group with edits.
 */
const EDIT_KINDS: ReadonlySet<string> = new Set([
  ToolCallKind.Edit,
  ToolCallKind.Delete,
  ToolCallKind.Move,
]);

/** True when the tool call is a file mutation (edit/delete/move with a path). */
export function isEditTouch(tc: ToolCallDetail): boolean {
  return !!tc.filePath && tc.toolKind != null && EDIT_KINDS.has(tc.toolKind);
}

/** True when the tool call carries a file path but is not a mutation (read/search context). */
export function isReadTouch(tc: ToolCallDetail): boolean {
  return !!tc.filePath && !isEditTouch(tc);
}

// -- Scope filtering -----------------------------------------------------------

/**
 * Task scope: the slice starting at the turn whose entry index (turn.index)
 * equals `entryIndex`, up to (exclusive) the next depth-0 user turn. Unknown
 * anchors degrade to the whole session rather than a blank viewer.
 */
export function taskScopeTurns(turns: TurnDetail[], entryIndex: number): TurnDetail[] {
  const start = turns.findIndex((t) => t.index === entryIndex);
  if (start === -1) return turns;
  let end = turns.length;
  for (let i = start + 1; i < turns.length; i++) {
    const t = turns[i];
    if (t.role === 'user' && (t.depth ?? 0) === 0) {
      end = i;
      break;
    }
  }
  return turns.slice(start, end);
}

/**
 * Change scope: the union of the task slices for the given entry indices — the
 * change's tasks within THIS session. The Review surface knows the binding
 * (which of a session's tasks belong to a change) and passes their entry
 * indices as a comma-separated scopeVal, so the viewer needs no Review fetch.
 * Each index expands to its task range (taskScopeTurns); the ranges are unioned,
 * preserving session order and de-duplicating. Empty/unknown degrade to the
 * whole session rather than a blank viewer.
 */
export function changeScopeTurns(turns: TurnDetail[], entryIndices: number[]): TurnDetail[] {
  const wanted = entryIndices.filter((n) => Number.isFinite(n));
  if (wanted.length === 0) return turns;
  const include = new Set<number>();
  for (const idx of wanted) {
    for (const t of taskScopeTurns(turns, idx)) include.add(t.index);
  }
  if (include.size === 0) return turns;
  return turns.filter((t) => include.has(t.index));
}

/**
 * Parse a `change` scopeVal ("12,45,78") into entry indices, dropping blanks
 * and non-numbers.
 */
export function parseChangeScopeVal(scopeVal: string): number[] {
  return scopeVal
    .split(',')
    .map((s) => Number.parseInt(s.trim(), 10))
    .filter((n) => !Number.isNaN(n));
}

/**
 * File scope: turns with at least one tool call on that file. Edits count,
 * reads are included because reads are context, but the file-scoped view
 * shows both). Matching is suffix-tolerant (`pathsMatch`) because the Map
 * links in with repo-relative node ids while Claude wire data carries
 * absolute tool-call paths. Empty path degrades to the whole session.
 */
export function fileScopeTurns(turns: TurnDetail[], filePath: string): TurnDetail[] {
  if (!filePath) return turns;
  return turns.filter((t) =>
    (t.toolCalls ?? []).some((tc) => !!tc.filePath && pathsMatch(tc.filePath, filePath)),
  );
}

/**
 * Apply the active scope to a turn list. `change` unions the task slices named
 * in scopeVal (a comma-separated list of entry indices supplied by the Review
 * surface). No scope (null) is a passthrough: whole session, unchanged behavior.
 */
export function scopeTurns(
  turns: TurnDetail[],
  scope: TranscriptScope | null,
  scopeVal: string,
): TurnDetail[] {
  switch (scope) {
    case TranscriptScope.Task: {
      const entryIndex = Number.parseInt(scopeVal, 10);
      if (Number.isNaN(entryIndex)) return turns;
      return taskScopeTurns(turns, entryIndex);
    }
    case TranscriptScope.File:
      return fileScopeTurns(turns, scopeVal);
    case TranscriptScope.Change:
      return changeScopeTurns(turns, parseChangeScopeVal(scopeVal));
    default:
      return turns;
  }
}

/**
 * Serialize turns to Markdown for "Copy as Markdown" (roadmap 4.7) — paste a
 * conversation into an issue/PR/doc. Each turn becomes a role heading + its
 * text, with a compact bullet list of the tool calls it made (name + file). The
 * caller passes exactly what's on screen, so a focused/scoped copy matches the
 * view. No emoji (plain text), so it reads cleanly anywhere.
 */
export function turnsToMarkdown(turns: TurnDetail[]): string {
  const blocks: string[] = [];
  for (const t of turns) {
    const who =
      t.role === 'user'
        ? 'You'
        : t.role === 'assistant'
          ? t.agentName
            ? `Assistant (${t.agentName})`
            : 'Assistant'
          : t.role;
    const parts: string[] = [`## ${who}`];
    const text = t.content?.trim();
    if (text) parts.push('', text);
    const tools = (t.toolCalls ?? []).filter((tc) => tc.name || tc.filePath);
    if (tools.length > 0) {
      parts.push('');
      for (const tc of tools) {
        const label = [tc.name, tc.filePath].filter(Boolean).join(' · ');
        parts.push(`- tool: ${label}`);
      }
    }
    blocks.push(parts.join('\n'));
  }
  return `${blocks.join('\n\n')}\n`;
}

/**
 * The package SessionDetail's own turn prefilter+dedup, in peasant's
 * `TurnDetail` vocabulary. The package only applies it when the host omits
 * the `turns` prop; since scoping passes a pre-filtered list, the adapter
 * runs the real package filter first so scoped views hide exactly the noise
 * turns the whole-session view hides.
 */
export function prefilterTurns(turns: TurnDetail[]): TurnDetail[] {
  return packagePrefilterTurns(turns);
}

// -- Touched-files rollup (TouchedFilesRail data) -------------------------------

export interface TurnFileTouches {
  /** Entry index of the turn (turn.index — matches `#turn-{index}` anchors). */
  turnIndex: number;
  /** Deduped file paths mutated in this turn. */
  edits: string[];
  /** Deduped read/context paths, excluding paths already listed under edits. */
  reads: string[];
}

/**
 * Per-turn file touches for the touched-files panel. A file both edited and
 * read within the same turn appears once, under edits (edits are what
 * attribution counts). Turns with no file-bearing tool calls are omitted.
 *
 * Paths are relativized against `workingDirectory` (when known) so the rows
 * display — and link to the Map with — repo-relative node ids rather than raw
 * absolute wire paths.
 */
export function collectFileTouches(
  turns: TurnDetail[],
  workingDirectory?: string,
): TurnFileTouches[] {
  const out: TurnFileTouches[] = [];
  for (const t of turns) {
    const edits: string[] = [];
    const reads: string[] = [];
    for (const tc of t.toolCalls ?? []) {
      if (!tc.filePath) continue;
      const path = relativizePath(tc.filePath, workingDirectory);
      // A Map node id is always repository-relative. Without the canonical
      // top-level workingDirectory (or for a path outside it), an absolute
      // tool path cannot be linked honestly. Legacy nested git context must
      // never be consulted to manufacture that authority.
      if (isAbsoluteFilePath(path)) continue;
      if (isEditTouch(tc)) {
        if (!edits.includes(path)) edits.push(path);
      } else if (!reads.includes(path)) {
        reads.push(path);
      }
    }
    const contextOnly = reads.filter((p) => !edits.includes(p));
    if (edits.length > 0 || contextOnly.length > 0) {
      out.push({ turnIndex: t.index, edits, reads: contextOnly });
    }
  }
  return out;
}

// -- Breadcrumb + chip helpers ---------------------------------------------------

/** Minimal breadcrumb item, structurally compatible with the package's BreadcrumbItem. */
export interface CrumbItem {
  label: string;
  href?: string;
}

/**
 * Origin crumb for viewers entered from Map or Review. Prepended to the
 * default breadcrumb; null when there is no (valid) origin, leaving the
 * default breadcrumb unchanged.
 */
export function originCrumb(
  scope: Pick<TranscriptRouteQuery, 'origin' | 'originNode' | 'originBranch' | 'returnLocation'>,
  projectHash: ProjectHash,
): CrumbItem | null {
  if (scope.origin === RouteOrigin.Map) {
    const node = scope.originNode;
    const base = mapHref(projectHash);
    return {
      label: node ? `map · ${node}` : 'map',
      href: scope.returnLocation?.href || (node ? mapHref(projectHash, { node }) : base),
    };
  }
  if (scope.origin === RouteOrigin.Review) {
    const branch = scope.originBranch;
    // Route contract (ReviewRouter): change detail is /review/{project}?branch=…
    // and the project segment is required to resolve the change.
    const base = reviewHref(projectHash);
    return {
      // The surface is called "changes"; `Review` survives
      // only as the validated route value and the /review route.
      label: branch ? `changes · ${branch}` : 'changes',
      href: branch ? reviewHref(projectHash, { branch }) : base,
    };
  }
  return null;
}

/** Visible chip text naming the active scope, in plain words. */
export function scopeChipLabel(scope: TranscriptScope, scopeVal: string): string {
  switch (scope) {
    case TranscriptScope.Task:
      return scopeVal
        ? `Showing one request (#${scopeVal}) — clear to see the whole conversation`
        : 'Showing one request — clear to see the whole conversation';
    case TranscriptScope.File:
      return scopeVal
        ? `Showing only what touched ${scopeVal} — clear to see the whole conversation`
        : 'Showing one file — clear to see the whole conversation';
    case TranscriptScope.Change:
      return 'Showing only this change’s requests — clear to see the whole conversation';
  }
}

/**
 * Query string with the scope params removed (the chip's "x"). Origin params
 * are kept so the origin breadcrumb still offers the way back; the session
 * (pathname) is untouched. Returns '' or a '?'-prefixed string.
 */
export function clearScopeQuery(params: URLSearchParams): string {
  const next = new URLSearchParams(params);
  next.delete(ScopeParam.Scope);
  next.delete(ScopeParam.ScopeVal);
  const qs = next.toString();
  return qs ? `?${qs}` : '';
}
