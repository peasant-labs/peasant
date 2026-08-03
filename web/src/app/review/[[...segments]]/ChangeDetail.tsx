'use client';

/* DEPRECATED — prior-version review surface (the richer change detail (treemap + WorkSection + structure-impact + signals + diffs)).
   SUPERSEDED by the lifted <Changes>/<ChangeDetail> from @peasant-labs/fairtrade/graph,
   mounted via ReviewSurface.tsx — the route no longer renders this. Retained DORMANT
   (not imported by the new /review path) as a deprecation candidate; its tests still
   exercise it. Do not extend; remove once the lifted surface is settled (tracked non-blocking, deprecation candidate). */

import { useId, useMemo, useState } from 'react';
import Link from 'next/link';
import { useRouter } from 'next/navigation';
import { ArrowRight, ChevronDown, ChevronRight, Copy, GitBranch, RotateCw, TriangleAlert } from 'lucide-react';
import {
  MapCanvas,
  edgeKey,
  type EdgeDelta,
  type NodeDelta,
} from '@/components/map';
import type { MapEdge } from '@peasant-labs/schema';
import type { FileChangeStatus } from '@peasant-labs/fairtrade/graph';
import {
  fetchChangeDiff,
  type DecodedChangeDetailPayload,
  type DecodedChangeDiffPayload,
} from '@/lib/api/map';
import type { ProjectHash } from '@/lib/navigation/projectRoutes';
import { DiffView } from './DiffView';
import { displayProject } from '@/lib/quality/utils';
import { formatTokens } from '@/lib/sessions/columns';
import { Term } from '@/components/Term';
import { Tooltip, Skeleton } from '@/lib/ft-ui';
import { Explainer, type ExplainerState } from '@/components/Explainer';
import { buildCaption, topModules, type CaptionAnchor } from './caption';
import { MINUS, plural, shortHash, shortSessionId } from './format';
import { groupChangedFiles, summarizeChange, type FileWithConversations } from './signals';
import { ChangeTreemap, fileRowAnchor } from './ChangeTreemap';
import { buildChangeRecap } from './changeRecap';
import { mapViolationToDatum, mapWireToDatum } from '@/app/map/lib/mapData';

type ChangeSession = DecodedChangeDetailPayload['work'][number];
type TaskSummary = ChangeSession['tasks'][number];

/**
 * Session "titles" are raw first-message text — often agent prompts full of
 * markup ("You are the INTEGRATION agent…", "<command-name>…"). Strip tags,
 * collapse whitespace, cut at a sentence boundary, and fall back to a dated
 * label when what's left is still machine noise, so the work list reads like
 * conversations, not config.
 */
export function sanitizeTitle(raw: string, fallback: string): string {
  const stripped = raw
    .replace(/<[^>]*>/g, ' ') // angle-bracket tags / markup
    .replace(/[`*_#>]+/g, ' ') // markdown noise
    .replace(/\s+/g, ' ')
    .trim();
  // Machine-prompt tells: "You are the …", or nothing readable left.
  const looksMachine = stripped.length < 4 || /^you are (the |an? )/i.test(stripped);
  if (looksMachine) return fallback;
  // Cut at the first sentence boundary if there is one early.
  const sentenceEnd = stripped.search(/[.!?](\s|$)/);
  const cut = sentenceEnd > 12 && sentenceEnd < 80 ? stripped.slice(0, sentenceEnd + 1) : stripped;
  return cut.length > 90 ? `${cut.slice(0, 89).trimEnd()}…` : cut;
}

/**
 * Change detail leads with a deterministic caption, then the changed portion
 * of the map as a delta-rendered visual anchor, followed by the recorded
 * sessions and tasks behind the change. Each item links on hover or focus to
 * the nodes it edited. The surface shows evidence; it does not certify it.
 */
/** Detail href for a branch (branch names contain slashes → query param). */
function branchHref(projectName: string, branch: string): string {
  return `/review/${encodeURIComponent(projectName)}?branch=${encodeURIComponent(branch)}`;
}

/**
 * Branch switcher: a labeled native select over every open line of work,
 * so the detail view isn't pinned to one branch. Changing it routes to that
 * branch's change detail (branch names contain slashes → query param). Kept to
 * a native control: square, hairline, focus-mono — accessible and zero new
 * dependencies. The currently-viewed branch may not be in `branches` (e.g. a
 * merged branch opened by deep link), so it is added as the selected option.
 */
function BranchSwitcher({
  projectName,
  branch,
  branches,
}: {
  projectName: string;
  branch: string;
  branches: string[];
}) {
  const router = useRouter();
  const options = branches.includes(branch) ? branches : [branch, ...branches];
  return (
    <label className="inline-flex items-center gap-2 self-start text-[12px] text-ink-3">
      <GitBranch size={14} aria-hidden className="shrink-0 text-ink-4" />
      <span className="shrink-0">Viewing line of work</span>
      <span className="relative inline-flex items-center">
        <select
          aria-label="Switch to another line of work"
          value={branch}
          onChange={(e) => router.push(branchHref(projectName, e.target.value))}
          className="max-w-[18rem] truncate appearance-none border border-rule bg-surface py-1 pl-2 pr-7 font-mono text-[12px] text-ink hover:bg-surface-hover focus-mono cursor-pointer"
        >
          {options.map((b) => (
            <option key={b} value={b}>
              {b}
            </option>
          ))}
        </select>
        <ChevronDown
          size={13}
          aria-hidden
          className="pointer-events-none absolute right-2 text-ink-4"
        />
      </span>
    </label>
  );
}

export function ChangeDetail({
  projectName,
  projectHash,
  branch,
  payload,
  navBranches = [],
  explainer,
}: {
  projectName: string;
  /** Opaque project hash for the lazy per-file diff fetch. */
  projectHash: ProjectHash;
  branch: string;
  payload: DecodedChangeDetailPayload;
  /** Open-branch order for the branch switcher. */
  navBranches?: string[];
  /** Shared explainer state — the "?" toggle lives in the page title. */
  explainer: ExplainerState;
}) {
  const [structureOpen, setStructureOpen] = useState(false);
  // Work ↔ slice linkage: the files a hovered/focused session (or task row)
  // edited. MapCanvas lifts ids hidden at the current zoom to their nearest
  // visible ancestor, so raw file paths are the right grain to pass.
  const [highlightedIds, setHighlightedIds] = useState<ReadonlySet<string> | null>(null);

  const scrollToId = (id: string) => {
    const el = document.getElementById(id);
    const reduced =
      typeof window !== 'undefined' &&
      typeof window.matchMedia === 'function' &&
      window.matchMedia('(prefers-reduced-motion: reduce)').matches;
    el?.scrollIntoView?.({ behavior: reduced ? 'auto' : 'smooth', block: 'start' });
  };
  // The changed-files section (id="review-files") is always expanded now, so the
  // file-count fragment just scrolls to it.
  const scrollToAnchor = (anchor: CaptionAnchor) => scrollToId(anchor);
  // A treemap tile jumps to that exact file's row in the list.
  const scrollToFile = (path: string) => scrollToId(fileRowAnchor(path));

  // The slice's structure edges plus the delta edges: removed edges no longer
  // exist in the head graph, so they are merged in to render dashed/removed;
  // new edges are deduped in case the slice already carries them.
  const structureEdges = useMemo<MapEdge[]>(() => {
    const seen = new Set<string>();
    const all: MapEdge[] = [];
    for (const e of [
      ...payload.slice.structureEdges,
      ...payload.newEdges,
      ...payload.removedEdges,
    ]) {
      const key = edgeKey(e.from, e.to);
      if (seen.has(key)) continue;
      seen.add(key);
      all.push(e);
    }
    return all;
  }, [payload]);

  // Structure-family deltas only: newEdges/removedEdges come from the parsed
  // import-graph diff, so activity edges between the same node pairs must not
  // inherit the delta styling (per-family props supersede legacy edgeDeltas).
  const structureEdgeDeltas = useMemo<Record<string, EdgeDelta>>(() => {
    const deltas: Record<string, EdgeDelta> = {};
    for (const e of payload.removedEdges) deltas[edgeKey(e.from, e.to)] = 'removed';
    for (const e of payload.newEdges) deltas[edgeKey(e.from, e.to)] = 'new';
    return deltas;
  }, [payload]);

  const nodeDeltas = useMemo<Record<string, NodeDelta>>(() => {
    const deltas: Record<string, NodeDelta> = {};
    for (const id of payload.removedNodes) deltas[id] = 'removed';
    for (const id of payload.newNodes) deltas[id] = 'new';
    return deltas;
  }, [payload]);

  // Evidence set for the Contribute exit: bound AND candidate sessions. Entry
  // into the wizard is filtered, not preselected, so the Choose
  // step is the safety net — the user still opts in per session. The exit
  // only disappears when there is no recorded work at all.
  const workSessionIds = payload.work.map((ws) => ws.sessionId);

  return (
    // data-tour: the first-run tour's "A change" step spotlights this surface.
    <div className="flex flex-col gap-6" data-tour="change-detail">
      {/* Branch switcher — jump to any open line of work without going
          back to the list. Only shown when there's more than one. */}
      {navBranches.length > 1 && (
        <BranchSwitcher
          projectName={projectName}
          branch={branch}
          branches={navBranches}
        />
      )}

      {/* Header cluster (one tight group): the numbers caption, the on-demand
          explainer box (its "?" toggle lives in the page title), then any
          friction signals. The Explainer renders full-width so the open box
          spans the column. */}
      <div className="flex flex-col gap-3">
        <Caption payload={payload} onDrill={scrollToAnchor} />
        <Explainer explainer={explainer} title="What am I looking at?">
          <p>
            One <Term k="change">line of work</Term>. The picture below is the
            part of the codebase it rewired; the list under it is the recorded{' '}
            <Term k="session">AI conversations</Term> that did the work.
          </p>
          <p>
            Hover a conversation to light up the files it changed in the list
            above (and on the structure picture, when you open it); click a
            request to read that part of the transcript.
          </p>
        </Explainer>
        <SignalBand payload={payload} />
      </div>

      {/* WHERE IT LANDED — churn-area overview (grayscale; no cost/verdict); a
          picture of the change's footprint that leads the literal file list. */}
      <ChangeTreemap payload={payload} onSelectFile={scrollToFile} />

      {/* LEAD — the actual changed files from git, grouped by directory, each
          paired with the conversation(s) that touched it (transcripts as the
          helper layer). The concrete "what changed", accurate from the diff. */}
      <ChangedFiles
        payload={payload}
        projectName={projectName}
        projectHash={projectHash}
        branch={branch}
        highlightedIds={highlightedIds}
      />

      {/* THE WORK — the recorded conversations behind the change. Hovering one
          lights its edits on the structure view below (when expanded). */}
      <WorkSection
        projectName={projectName}
        branch={branch}
        payload={payload}
        onHighlight={setHighlightedIds}
      />

      {/* STRUCTURE IMPACT — the dependency-level map of what got rewired. It is
          the abstract view, so it is demoted to an opt-in disclosure under the
          concrete file list rather than leading the page. */}
      <section id="review-structure" className="border border-rule bg-surface">
        <button
          type="button"
          aria-expanded={structureOpen}
          aria-label={`${structureOpen ? 'Collapse' : 'Expand'} the structure impact view`}
          onClick={() => setStructureOpen((v) => !v)}
          className="flex w-full items-center gap-2 px-5 py-3 hover:bg-surface-hover focus-mono cursor-pointer"
        >
          {structureOpen ? (
            <ChevronDown size={12} aria-hidden className="text-ink-3" />
          ) : (
            <ChevronRight size={12} aria-hidden className="text-ink-3" />
          )}
          <span className="v2-eyebrow">structure impact</span>
          <span className="text-[12px] text-ink-3">how the change rewired the code map</span>
        </button>
        {structureOpen && (
          <>
            <div className="border-t border-rule px-5 py-1.5 text-[11px] text-ink-3">
              <span className="text-ink-2">added</span> ·{' '}
              <span className="text-ink-2">removed (dashed)</span> ·{' '}
              <span className="text-ink-2">highlighted = touched by the hovered conversation</span>
            </div>
            <div className="h-[440px] border-t border-rule">
              <MapCanvas
                nodes={payload.slice.nodes.map(mapWireToDatum)}
                structureEdges={structureEdges}
                activityEdges={payload.slice.activityEdges}
                violations={payload.violations.map(mapViolationToDatum)}
                nodeDeltas={nodeDeltas}
                structureEdgeDeltas={structureEdgeDeltas}
                highlightedIds={highlightedIds ?? undefined}
                ariaLabel={`Changed slice of ${displayProject(projectName)} on ${branch}`}
              />
            </div>
          </>
        )}
      </section>

      <Footnotes payload={payload} />

      <Exits
        payload={payload}
        projectName={projectName}
        branch={branch}
        defaultBranch={payload.defaultBranch}
        sessionIds={workSessionIds}
      />

      {/* Boundary line: what this surface is, and is not. */}
      <p className="text-xs text-ink-3">
        Shows what changed and the recorded work behind it — not whether the change is
        correct or secure.
      </p>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Signal band: a neutral friction readout, never a grade or verdict.
// The activity counts (conversations / requests) live in the caption above, so
// this band shows ONLY friction signals (retry loops, rule breaks) and renders
// NOTHING on the happy path — no "no signals" filler, no duplication.
// ---------------------------------------------------------------------------

function SignalBand({ payload }: { payload: DecodedChangeDetailPayload }) {
  const s = summarizeChange(payload);
  const unusual = payload.unusual ?? [];
  const frictions = payload.frictions ?? [];
  if (s.retryLoops === 0 && s.violations === 0 && unusual.length === 0 && frictions.length === 0)
    return null;
  return (
    <section
      aria-label="Change signals"
      className="flex flex-col gap-1.5 border border-rule bg-surface px-5 py-2"
    >
      <div className="flex flex-wrap items-center gap-x-5 gap-y-1.5">
        <span className="v2-eyebrow">signals</span>
        {s.retryLoops > 0 && (
          <span className="text-[13px] text-ink-2 tabular-nums">
            <span className="font-mono text-ink">{s.retryLoops}</span>{' '}
            {s.retryLoops === 1 ? 'retry loop' : 'retry loops'}
          </span>
        )}
        {s.violations > 0 && (
          <span className="inline-flex items-center gap-1 text-[13px] text-danger tabular-nums">
            <TriangleAlert size={13} aria-hidden />
            <span className="font-mono">{s.violations}</span>{' '}
            {s.violations === 1 ? 'rule break' : 'rule breaks'}
          </span>
        )}
      </div>
      {/* What's unusual (4.4): neutral rate-elevation vs the project baseline —
          a fact for orientation, never a grade. */}
      {unusual.map((u) => (
        <p key={u.kind} className="text-[12px] text-ink-3">
          {u.label} —{' '}
          <span className="font-mono tabular-nums text-ink-2">{u.perChange.toFixed(1)}</span>
          {' vs '}
          <span className="font-mono tabular-nums">{u.perProject.toFixed(1)}</span>
          {' typical for this project'}
        </p>
      ))}
      {/* Recurring friction (5.1): which files this friction kept landing on —
          neutral counts ("N times across M conversations"), never a verdict. */}
      {frictions.map((f) => (
        <p key={`${f.kind}:${f.file}`} className="text-[12px] text-ink-3">
          <span className="font-mono tabular-nums text-ink-2">{f.file}</span>
          {' · '}
          {f.label}{' '}
          <span className="font-mono tabular-nums text-ink-2">{f.count}</span>
          {f.count === 1 ? ' time' : ' times'}
          {' across '}
          <span className="font-mono tabular-nums text-ink-2">{f.sessions}</span>
          {f.sessions === 1 ? ' conversation' : ' conversations'}
        </p>
      ))}
    </section>
  );
}

// ---------------------------------------------------------------------------
// Caption — the deterministic sentence; every fragment drills to evidence.
// ---------------------------------------------------------------------------

function Caption({
  payload,
  onDrill,
}: {
  payload: DecodedChangeDetailPayload;
  onDrill: (anchor: CaptionAnchor) => void;
}) {
  const fragments = buildCaption(payload);
  return (
    <div className="flex flex-col gap-0.5 min-w-0">
      {/* Lead with the numbers (the payload); the affordance hint is demoted. */}
      <p className="text-sm text-ink leading-relaxed" aria-label="Change caption">
        {fragments.map((f, i) => (
          <span key={f.key}>
            {i > 0 && <span className="text-ink-4"> · </span>}
            <button
              type="button"
              onClick={() => onDrill(f.anchor)}
              title="click to jump to the proof below"
              className="font-mono text-[13px] tabular-nums text-ink border-b border-dotted border-ink-4 hover:border-ink focus-mono cursor-pointer"
            >
              {f.text}
              {(f.wrongWay ?? 0) > 0 && (
                <TriangleAlert size={12} aria-hidden className="inline-block ml-1 text-danger align-[-1px]" />
              )}
            </button>
          </span>
        ))}
      </p>
      <p className="text-[11px] text-ink-4">Click any number to jump to its proof below.</p>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Changed files (file-first) — the actual git diff leads, grouped by directory,
// each file paired with the conversation(s) that touched it. Consistent, fixed
// columns: status · path · conversations (so the rows align, full width).
// ---------------------------------------------------------------------------

const STATUS_LABEL: Record<FileChangeStatus, string> = {
  M: 'changed',
  A: 'added',
  D: 'deleted',
  R: 'renamed',
};

function fileChangeStatus(value: string): FileChangeStatus {
  if (value === 'A' || value === 'M' || value === 'D' || value === 'R') return value;
  throw new Error(
    `Change detail could not be rendered because file.status contained unknown value ${JSON.stringify(value)} in FileRow while adapting the REST response; rendering has stopped to avoid mislabeling the change. Regenerate @peasant-labs/schema and update this view for the new status.`,
  );
}

function ChangedFiles({
  payload,
  projectName,
  projectHash,
  branch,
  highlightedIds,
}: {
  payload: DecodedChangeDetailPayload;
  projectName: string;
  projectHash: ProjectHash;
  branch: string;
  /** Paths to highlight (a hovered conversation's edited files) → lit rows. */
  highlightedIds?: ReadonlySet<string> | null;
}) {
  const groups = useMemo(() => groupChangedFiles(payload), [payload]);
  const hasLines = payload.linesAdded > 0 || payload.linesRemoved > 0;
  return (
    <section id="review-files" aria-label="Files changed" className="border border-rule bg-surface">
      <div className="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1 border-b border-rule px-5 py-3">
        <span className="text-sm font-medium text-ink">Files changed</span>
        <span className="font-mono text-xs tabular-nums text-ink-3">
          {plural(payload.files.length, 'file')}
          {hasLines && (
            <>
              {' · '}
              <span className="text-ink-2">+{payload.linesAdded}</span>/
              <span className="text-ink-2">
                {MINUS}
                {payload.linesRemoved}
              </span>
            </>
          )}
        </span>
      </div>
      {payload.files.length === 0 ? (
        <p className="px-5 py-4 text-[13px] text-ink-3">No files changed.</p>
      ) : (
        groups.map((g) => (
          <div key={g.dir}>
            <div className="flex items-center gap-2 border-b border-rule px-5 py-1.5">
              <span className="font-mono text-[11px] text-ink-2">{g.dir}</span>
              <span className="font-mono text-[11px] tabular-nums text-ink-4">{g.files.length}</span>
            </div>
            <ul className="divide-y divide-rule">
              {g.files.map((fwc) => (
                <FileRow
                  key={fwc.file.path}
                  fwc={fwc}
                  dir={g.dir}
                  projectName={projectName}
                  projectHash={projectHash}
                  branch={branch}
                  highlighted={highlightedIds?.has(fwc.file.path) ?? false}
                />
              ))}
            </ul>
          </div>
        ))
      )}
    </section>
  );
}

function FileRow({
  fwc,
  dir,
  projectName,
  projectHash,
  branch,
  highlighted,
}: {
  fwc: FileWithConversations;
  dir: string;
  projectName: string;
  projectHash: ProjectHash;
  branch: string;
  highlighted: boolean;
}) {
  const { file, sessions } = fwc;
  // Path relative to its group directory (the prefix is the group header).
  const rel = dir === '(root)' || !file.path.startsWith(`${dir}/`)
    ? file.path
    : file.path.slice(dir.length + 1);
  const display = file.oldPath ? `${file.oldPath} → ${file.path}` : rel;

  // Lazy per-file diff (the #1 mission gap): fetch only when the row is opened.
  const [open, setOpen] = useState(false);
  const [diff, setDiff] = useState<DecodedChangeDiffPayload | null>(null);
  const [diffError, setDiffError] = useState<string | null>(null);
  const [diffLoading, setDiffLoading] = useState(false);
  // The conversations that touched this file are shown in place rather than
  // navigating away. The evidence is expanded by default, not hidden as an extra.
  const [peekOpen, setPeekOpen] = useState(true);

  const toggle = () => {
    const next = !open;
    setOpen(next);
    if (next && diff === null && !diffLoading) {
      setDiffLoading(true);
      setDiffError(null);
      fetchChangeDiff(projectHash, branch, file.path)
        .then(setDiff)
        .catch((e: unknown) => setDiffError(e instanceof Error ? e.message : String(e)))
        .finally(() => setDiffLoading(false));
    }
  };

  return (
    <li
      id={fileRowAnchor(file.path)}
      data-highlighted={highlighted || undefined}
      // Left ink accent (reserved transparent on every row → no hover jitter) so
      // the "hovered conversation lit these files" link reads clearly in mono.
      className={`border-l-2 transition-colors ${
        highlighted ? 'border-ink bg-surface-hover' : 'border-transparent'
      }`}
    >
      <div className="grid grid-cols-[4.25rem_minmax(0,1fr)_auto] items-center gap-3 px-5 py-1.5">
        {/* The plain word, not the git letter — meaning on screen, not a tooltip. */}
        <span className="font-mono text-[10px] uppercase tracking-wide text-ink-3">
          {STATUS_LABEL[fileChangeStatus(file.status)]}
        </span>
        {/* The path is the diff toggle — click to see the actual changed lines. */}
        <button
          type="button"
          onClick={toggle}
          aria-expanded={open}
          aria-label={`${open ? 'Hide' : 'Show'} the changed lines of ${file.path}`}
          className="flex min-w-0 items-center gap-1 text-left focus-mono cursor-pointer"
        >
          {open ? (
            <ChevronDown size={12} aria-hidden className="shrink-0 text-ink-4" />
          ) : (
            <ChevronRight size={12} aria-hidden className="shrink-0 text-ink-4" />
          )}
          <span className="truncate font-mono text-xs text-ink-2 hover:text-ink" title={file.path}>
            {display}
          </span>
        </button>
        {sessions.length > 0 ? (
          // Opening the cell expands the conversations inline so
          // the reader stays on the page; the viewer is one deliberate click
          // further in, from each conversation's "Read transcript" link.
          <button
            type="button"
            onClick={() => setPeekOpen((v) => !v)}
            aria-expanded={peekOpen}
            aria-label={`${peekOpen ? 'Hide' : 'Show'} the ${plural(
              sessions.length,
              'conversation',
            )} that touched ${file.path}`}
            className="inline-flex items-center justify-end gap-1 whitespace-nowrap text-right font-mono text-[11px] tabular-nums text-ink-3 hover:text-ink focus-mono cursor-pointer"
          >
            {peekOpen ? (
              <ChevronDown size={11} aria-hidden className="shrink-0 text-ink-4" />
            ) : (
              <ChevronRight size={11} aria-hidden className="shrink-0 text-ink-4" />
            )}
            {plural(sessions.length, 'conversation')}
          </button>
        ) : (
          <span className="whitespace-nowrap text-right font-mono text-[11px] text-ink-4">
            no conversation
          </span>
        )}
      </div>

      {peekOpen && sessions.length > 0 && (
        <ConversationPeek
          sessions={sessions}
          filePath={file.path}
          projectName={projectName}
          branch={branch}
        />
      )}

      {open && (
        <div className="px-5 pb-2">
          {diffLoading ? (
            <Skeleton avatar={false} lines={3} label="Loading diff" />
          ) : diffError ? (
            <p className="font-mono text-[12px] text-danger">Couldn&rsquo;t load this diff.</p>
          ) : diff ? (
            <DiffView
              hunks={diff.hunks}
              binary={diff.binary}
              truncated={diff.truncated}
              projectName={projectName}
              branch={branch}
            />
          ) : null}
        </div>
      )}
    </li>
  );
}

/**
 * Inline conversation peek: the conversations that touched
 * one changed file, expanded IN PLACE rather than navigating straight off. Each
 * conversation shows its title, harness, binding, and the specific requests
 * that edited this exact file — every request links into the viewer (scoped to
 * that request), and a "Read transcript" link opens the whole conversation.
 *
 * Full transcript rendering remains in the dedicated viewer; each row links to
 * that production surface rather than duplicating its renderer here.
 */
function ConversationPeek({
  sessions,
  filePath,
  projectName,
  branch,
}: {
  sessions: ChangeSession[];
  filePath: string;
  projectName: string;
  branch: string;
}) {
  return (
    // No tint: the top border alone sets the peek apart, so the
    // conversation block reads cleanly on the row's own surface.
    <div className="border-t border-rule bg-surface px-5 py-2.5">
      <ul className="flex flex-col gap-2.5">
        {sessions.map((s) => {
          // The requests in this conversation that edited THIS file (the rest of
          // the conversation isn't shown here — the reader peeks at what's
          // relevant to the file, then reads the full transcript if they want).
          const touchingTasks = s.tasks.filter((t) => t.editedFiles.includes(filePath));
          const title = sanitizeTitle(s.title, shortSessionId(s.sessionId));
          return (
            <li key={s.sessionId} className="flex flex-col gap-1">
              <div className="flex flex-wrap items-center gap-x-2 gap-y-0.5">
                <span className="min-w-0 truncate text-[13px] font-medium text-ink" title={s.title}>
                  {title}
                </span>
                <span className="shrink-0 font-mono text-[11px] text-ink-3">{s.harness}</span>
                <BindingTag candidate={s.binding === 'candidate'} />
              </div>
              {touchingTasks.length > 0 && (
                <ul className="flex flex-col gap-0.5 pl-2 border-l border-rule">
                  {touchingTasks.map((t) => (
                    <li key={t.entryIndex}>
                      <Link
                        href={taskHref(projectName, t.sessionId, t.entryIndex, branch)}
                        className="inline-flex items-center gap-1 text-[12px] text-ink-2 hover:text-ink hover:underline focus-mono cursor-pointer"
                      >
                        <span className="truncate">
                          {sanitizeTitle(t.title, `Request #${t.entryIndex} in this conversation`)}
                        </span>
                      </Link>
                    </li>
                  ))}
                </ul>
              )}
              <Link
                href={changeViewerHref(
                  projectName,
                  s.sessionId,
                  s.tasks.map((t) => t.entryIndex),
                  branch,
                )}
                className="inline-flex w-fit items-center gap-1 font-mono text-[11px] text-ink-3 hover:text-ink hover:underline focus-mono cursor-pointer"
              >
                Read transcript
                <ArrowRight size={11} aria-hidden />
              </Link>
            </li>
          );
        })}
      </ul>
    </div>
  );
}

// ---------------------------------------------------------------------------
// The work — sessions (linked / possibly related), tasks, unrecorded commits.
// Full width below the slice: the recorded story explains what shaped the
// slice, and hover/focus lights each session's edits on it.
// ---------------------------------------------------------------------------

/** Hover/focus → the file ids to light on the slice; null clears. */
type HighlightHandler = (ids: ReadonlySet<string> | null) => void;

function WorkSection({
  projectName,
  branch,
  payload,
  onHighlight,
}: {
  projectName: string;
  branch: string;
  payload: DecodedChangeDetailPayload;
  onHighlight: HighlightHandler;
}) {
  const hasWork = payload.work.length > 0 || payload.unrecordedCommits.length > 0;
  return (
    <section id="review-work" aria-label="The work" className="border border-rule bg-surface">
      <div className="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1 border-b border-rule px-5 py-3">
        <span className="text-sm font-medium text-ink">The conversations behind it</span>
        <span className="font-mono text-xs tabular-nums text-ink-3">
          {plural(payload.work.length, 'conversation')} — click a request to read it
        </span>
      </div>

      <div className="divide-y divide-rule">
        {!hasWork && (
          <p className="px-5 py-4 text-[13px] text-ink-3">No recorded work for this branch.</p>
        )}

        {payload.work.map((ws) => (
          <WorkSession
            key={ws.sessionId}
            projectName={projectName}
            branch={branch}
            session={ws}
            onHighlight={onHighlight}
          />
        ))}

        {payload.unrecordedCommits.length > 0 && (
          <div className="px-5 py-4">
            <div className="v2-eyebrow mb-1">Updates without a saved conversation</div>
            <p className="mb-2 text-[11px] text-ink-4">
              Saved updates with no AI conversation captured — likely written by
              hand, or recorded outside this tool.
            </p>
            <ul className="flex flex-col gap-1.5">
              {payload.unrecordedCommits.map((c) => (
                <li key={c.hash} className="flex items-center gap-2">
                  <span className="font-mono text-xs text-ink-3 shrink-0">{shortHash(c.hash)}</span>
                  <span className="text-xs text-ink-2 truncate" title={c.subject}>
                    {c.subject}
                  </span>
                </li>
              ))}
            </ul>
          </div>
        )}
      </div>
    </section>
  );
}

/** Task rows shown per session before the "Show all" expander. */
const TASK_PREVIEW_COUNT = 3;

/** Shared-prefix length at which a task title counts as repeating the session title. */
const TITLE_PREFIX_OVERLAP = 40;

/**
 * True when a task row's title merely repeats the session title — exactly, or
 * via a shared prefix of ≥40 characters (agent prompts often restate the
 * session title verbatim with a trailing elaboration). Such rows render as
 * "task @ turn {entryIndex}" instead of repeating the prompt.
 */
export function repeatsSessionTitle(taskTitle: string, sessionTitle: string): boolean {
  if (taskTitle === sessionTitle) return true;
  if (taskTitle.length < TITLE_PREFIX_OVERLAP || sessionTitle.length < TITLE_PREFIX_OVERLAP) {
    return false;
  }
  return taskTitle.slice(0, TITLE_PREFIX_OVERLAP) === sessionTitle.slice(0, TITLE_PREFIX_OVERLAP);
}

/**
 * The conversation→change binding, in plain language with its meaning on hover
 * right at the tag (no separate legend, no floating native title). "linked" =
 * confidently part of the change; "possibly related" = one signal only.
 */
function BindingTag({ candidate }: { candidate: boolean }) {
  const tipId = useId();
  return (
    <Tooltip
      id={tipId}
      content={
        candidate
          ? 'Only one signal matched (a commit or an edited file), so this conversation may be unrelated; shown so nothing is hidden.'
          : 'Both its commits and its edited files match this change; confidently part of it.'
      }
    >
      <span
        className={`ml-auto shrink-0 cursor-help border px-1.5 py-0.5 text-[10px] uppercase tracking-wide ${
          candidate ? 'border-rule text-ink-3' : 'border-rule-strong text-ink-2'
        }`}
      >
        {candidate ? 'possibly related' : 'linked'}
      </span>
    </Tooltip>
  );
}

function WorkSession({
  projectName,
  branch,
  session,
  onHighlight,
}: {
  projectName: string;
  branch: string;
  session: ChangeSession;
  onHighlight: HighlightHandler;
}) {
  const candidate = session.binding === 'candidate';
  const [tasksExpanded, setTasksExpanded] = useState(false);

  // The distinct files this session edited across all its tasks — the hover
  // highlight set, and the source of the per-session TOUCHED summary.
  const editedFiles = useMemo(() => {
    const files = new Set<string>();
    for (const t of session.tasks) for (const f of t.editedFiles) files.add(f);
    return files as ReadonlySet<string>;
  }, [session]);

  // WHERE the session worked: top modules by edited-file count (same
  // first-two-path-segment grouping as the caption), capped at 3.
  const touchedModules = useMemo(
    () => topModules([...editedFiles].map((path) => ({ path, status: 'M' as const })), 3),
    [editedFiles],
  );

  const visibleTasks = tasksExpanded ? session.tasks : session.tasks.slice(0, TASK_PREVIEW_COUNT);
  const hiddenTaskCount = session.tasks.length - visibleTasks.length;

  const cleanTitle = sanitizeTitle(
    session.title,
    `${shortSessionId(session.sessionId)} · ${plural(session.tasks.length, 'request')}`,
  );

  return (
    <div
      data-testid={`work-session-${session.sessionId}`}
      onMouseEnter={() => onHighlight(editedFiles)}
      onMouseLeave={() => onHighlight(null)}
      // React's onFocus/onBlur propagate from the focusable children (task
      // links, the expander) — a task row narrows the set and stops
      // propagation; anything else highlights the whole session.
      onFocus={() => onHighlight(editedFiles)}
      onBlur={(e) => {
        if (!e.currentTarget.contains(e.relatedTarget as Node | null)) onHighlight(null);
      }}
      className="px-5 py-4"
    >
      {/* Session header — the title links to this conversation filtered to the
          change (its tasks only); the binding tag sits in a consistent slot. */}
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
        <span className="v2-eyebrow shrink-0">Conversation</span>
        <Link
          href={changeViewerHref(
            projectName,
            session.sessionId,
            session.tasks.map((t) => t.entryIndex),
            branch,
          )}
          title={`Read "${session.title}" filtered to this change`}
          className="min-w-0 truncate text-sm font-medium text-ink hover:underline focus-mono cursor-pointer"
        >
          {cleanTitle}
        </Link>
        <span className="shrink-0 font-mono text-[11px] text-ink-3">{session.harness}</span>
        <BindingTag candidate={candidate} />
      </div>

      {/* TOUCHED line — WHERE the session worked, and how much of it. */}
      <div className="mt-1 font-mono text-[11px] text-ink-3">
        {touchedModules.length > 0 && <span>{touchedModules.join(' · ')}</span>}
        {touchedModules.length > 0 && <span className="text-ink-4"> — </span>}
        <span className="tabular-nums">
          {plural(session.tasks.length, 'request')} · {plural(editedFiles.size, 'file')}
        </span>
      </div>

      {visibleTasks.length > 0 && (
        <ul className="mt-2 flex flex-col">
          {visibleTasks.map((t) => (
            <li key={t.entryIndex}>
              <TaskRow
                projectName={projectName}
                branch={branch}
                task={t}
                sessionTitle={session.title}
                sessionEditedFiles={editedFiles}
                onHighlight={onHighlight}
              />
            </li>
          ))}
        </ul>
      )}

      {hiddenTaskCount > 0 && (
        <button
          type="button"
          onClick={() => setTasksExpanded(true)}
          className="mt-2 border border-rule bg-surface px-2 py-1 text-[11px] text-ink-2 hover:bg-surface-hover focus-mono cursor-pointer"
        >
          Show all {session.tasks.length} requests
        </button>
      )}
    </div>
  );
}

/**
 * Viewer deep link for a whole conversation scoped to ONE change: the viewer
 * unions the task slices at these entry indices (the change's tasks in this
 * session). The Review surface owns the binding, so it supplies the indices —
 * the viewer needs no Review fetch (scopeTurns §change).
 */
export function changeViewerHref(
  projectName: string,
  sessionId: string,
  entryIndices: number[],
  branch: string,
): string {
  const params = new URLSearchParams({
    scope: 'change',
    scopeVal: entryIndices.join(','),
    origin: 'Review',
    originBranch: branch,
  });
  return `/projects/${encodeURIComponent(projectName)}/${encodeURIComponent(sessionId)}?${params.toString()}`;
}

/** Viewer deep link for one task using the scoped query parameters. */
export function taskHref(
  projectName: string,
  sessionId: string,
  entryIndex: number,
  branch: string,
): string {
  const params = new URLSearchParams({
    scope: 'task',
    scopeVal: String(entryIndex),
    origin: 'Review',
    originBranch: branch,
  });
  return `/projects/${encodeURIComponent(projectName)}/${encodeURIComponent(sessionId)}?${params.toString()}`;
}

function TaskRow({
  projectName,
  branch,
  task,
  sessionTitle,
  sessionEditedFiles,
  onHighlight,
}: {
  projectName: string;
  branch: string;
  task: TaskSummary;
  sessionTitle: string;
  sessionEditedFiles: ReadonlySet<string>;
  onHighlight: HighlightHandler;
}) {
  // De-duplicate: a row that just restates the session title carries no
  // information — show its position in the transcript instead.
  const repeated = repeatsSessionTitle(task.title, sessionTitle);
  const displayTitle = repeated
    ? `Request #${task.entryIndex} in this conversation`
    : sanitizeTitle(task.title, `Request #${task.entryIndex} in this conversation`);
  const taskFiles = useMemo(() => new Set(task.editedFiles) as ReadonlySet<string>, [task]);

  return (
    <Link
      href={taskHref(projectName, task.sessionId, task.entryIndex, branch)}
      aria-label={
        repeated
          ? `Open request #${task.entryIndex} in the transcript`
          : `Open request "${displayTitle}" in the transcript`
      }
      // Narrow the slice highlight to this task's edits; restore the session
      // set on leave (the pointer is still inside the session block).
      onMouseEnter={() => onHighlight(taskFiles)}
      onMouseLeave={() => onHighlight(sessionEditedFiles)}
      onFocus={(e) => {
        e.stopPropagation();
        onHighlight(taskFiles);
      }}
      className="flex items-center gap-2 py-1 px-1 -mx-1 hover:bg-surface-hover transition-colors focus-mono cursor-pointer"
    >
      <span className="text-xs text-ink-2 truncate flex-1" title={task.title}>
        {displayTitle}
      </span>
      {task.retryLoop && (
        <span
          title="the AI retried this request before it worked"
          className="inline-flex items-center gap-1 border border-rule px-1 py-0.5 text-[10px] text-ink-3 shrink-0"
        >
          <RotateCw size={10} aria-hidden />
          took several attempts
        </span>
      )}
      <span className="font-mono text-[11px] tabular-nums text-ink-3 shrink-0">
        {plural(task.editedFiles.length, 'file')}
      </span>
    </Link>
  );
}

// ---------------------------------------------------------------------------
// Footnotes — files · +A/−R lines · output tokens · cost when known.
// ---------------------------------------------------------------------------

function Footnotes({ payload }: { payload: DecodedChangeDetailPayload }) {
  const hasLines = payload.linesAdded > 0 || payload.linesRemoved > 0;
  const dot = <span className="text-ink-4">·</span>;
  return (
    <section
      aria-label="Totals"
      className="border border-rule bg-surface px-5 py-3 flex flex-wrap items-center gap-x-3 gap-y-1"
    >
      <span className="v2-eyebrow mr-1">Totals</span>
      <span className="font-mono text-xs tabular-nums text-ink-2" title="files this work touched">
        {payload.files.length} files touched
      </span>
      {/* Line counts omitted when both are zero (pure renames, or numstat
          unavailable — 0/0 carries no information either way). */}
      {hasLines && (
        <>
          {dot}
          <span
            className="font-mono text-xs tabular-nums text-ink-2"
            title="lines of code added / removed"
          >
            +{payload.linesAdded}/{MINUS}
            {payload.linesRemoved} lines
          </span>
        </>
      )}
      {dot}
      <span className="text-xs tabular-nums text-ink-2">
        AI wrote ≈
        {/* outputTokens is a wire int64 (bigint); token counts never approach
            Number.MAX_SAFE_INTEGER, so a plain Number() conversion is safe
            here — formatTokens does real division/toFixed arithmetic that
            bigint doesn't support. */}
        <span className="font-mono">{formatTokens(Number(payload.outputTokens))}</span>{' '}
        <Term k="outputTokens">tokens</Term>
      </span>
      {payload.costUsd != null && (
        <>
          {dot}
          <span className="text-xs tabular-nums text-ink-2">
            <Term k="cost">estimated AI spend</Term>{' '}
            <span className="font-mono">${payload.costUsd.toFixed(2)}</span>
          </span>
        </>
      )}
    </section>
  );
}

// ---------------------------------------------------------------------------
// Exits — open in Map · Contribute these conversations · the literal diff command.
// ---------------------------------------------------------------------------

function Exits({
  payload,
  projectName,
  branch,
  defaultBranch,
  sessionIds,
}: {
  payload: DecodedChangeDetailPayload;
  projectName: string;
  branch: string;
  defaultBranch: string;
  sessionIds: string[];
}) {
  const [copied, setCopied] = useState(false);
  const [recapCopied, setRecapCopied] = useState(false);
  const diffCommand = `git diff ${defaultBranch}...${branch}`;

  const copyDiff = () => {
    void navigator.clipboard?.writeText(diffCommand);
    setCopied(true);
    window.setTimeout(() => setCopied(false), 1500);
  };

  // Per-PR recap (5.5): copy a plain-markdown summary for a PR description.
  const copyRecap = () => {
    void navigator.clipboard?.writeText(buildChangeRecap(payload));
    setRecapCopied(true);
    window.setTimeout(() => setRecapCopied(false), 1500);
  };

  const exitLink =
    'border border-rule bg-surface px-3 py-1.5 text-[13px] text-ink hover:bg-surface-hover transition-colors focus-mono cursor-pointer';

  return (
    <div className="flex flex-col gap-2">
      <div className="flex flex-wrap items-center gap-3">
        <Link
          href={`/map/${encodeURIComponent(projectName)}`}
          aria-label={`See this work on the code map of ${displayProject(projectName)}`}
          className={exitLink}
        >
          See this work on the code map
        </Link>

        {sessionIds.length > 0 && (
          <Link
            href={`/share?sessions=${sessionIds.map(encodeURIComponent).join(',')}`}
            aria-label={`Contribute the ${sessionIds.length} conversations behind this change`}
            className={`inline-flex items-center gap-1.5 ${exitLink}`}
          >
            Contribute {plural(sessionIds.length, 'conversation')}…
            <ArrowRight size={12} aria-hidden />
          </Link>
        )}

        {/* Per-PR recap (5.5): a markdown summary for a PR description / standup. */}
        <button
          type="button"
          onClick={copyRecap}
          aria-label="Copy a markdown recap of this change for a PR description"
          className={`inline-flex items-center gap-1.5 ${exitLink}`}
        >
          <Copy size={12} aria-hidden />
          {recapCopied ? 'Recap copied' : 'Copy recap'}
        </button>
      </div>
      {sessionIds.length > 0 && (
        <p className="text-[11px] text-ink-4">
          Contributing opens a review step — you choose exactly what to include;
          nothing is sent until you confirm.
        </p>
      )}

      {/* "view diff" is the literal command, copyable — never a dead link.
          Labeled for engineers and de-fanged: it only reads. */}
      <div className="flex flex-col gap-1 mt-1">
        <span className="v2-eyebrow">for engineers</span>
        <p className="text-[11px] text-ink-4">
          A terminal command that prints every line-by-line edit in this work.
          Run it inside the project folder — it only reads, it changes nothing.
        </p>
        <span className="flex items-center mt-0.5" aria-label="git diff command">
          <code className="font-mono text-xs border border-rule bg-surface-hover px-2 py-2 text-ink-2">
            {diffCommand}
          </code>
          <button
            type="button"
            onClick={copyDiff}
            aria-label="Copy the git diff command"
            className="flex items-center gap-1 border border-rule border-l-0 bg-surface px-2 py-1.5 text-xs text-ink-2 hover:bg-surface-hover focus-mono cursor-pointer"
          >
            <Copy size={12} aria-hidden />
            {copied ? 'copied' : 'copy'}
          </button>
        </span>
      </div>
    </div>
  );
}
