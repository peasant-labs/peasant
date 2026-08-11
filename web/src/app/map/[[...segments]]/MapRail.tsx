'use client';

import { useMemo, useState } from 'react';
import Link from 'next/link';
import { ArrowRight, FileCode, FolderOpen, RotateCw, X } from 'lucide-react';
import { OutcomeChip, formatRelative, summarizePrompt } from '@peasant-labs/transcript-browser';
import { ProviderName, RailSection, Skeleton } from '@/lib/ft-ui';
import { Skeleton as SkeletonBox } from '@/lib/skeleton';
import type { DecodedMapNodeDetailPayload, DecodedTaskSummary } from '@/lib/api/map';
import type { SessionSummary } from '@/types/messages';
import { displayProject } from '@/lib/quality/utils';
import { OutcomeHeuristicHelp } from '@/components/OutcomeHeuristicHelp';
import { RouteOrigin, transcriptHref, type ProjectHash, type ReturnLocation } from '@/lib/navigation/projectRoutes';
import {
  contributeSessionsHref,
  interleaveShapedBy,
  shapedBySessionIds,
  touchedModules,
  type Coverage,
  type CoupledNode,
} from '../lib/mapData';

/**
 * The Map rail is always present; selection swaps its content.
 * Two states live here: the project panel (unselected: coverage line,
 * recent recorded work, the session list) and the node panel (selected,
 * fed by the node-detail REST call + the graph's co-edit coupling).
 *
 * These components return rail CONTENT (ft-ui RailSection blocks) to be
 * composed into the ft-ui RailShell's `rail` prop in MapPageClient.
 */

// ---------------------------------------------------------------------------
// Shared bits
// ---------------------------------------------------------------------------

function relTime(ms: number | undefined | null, nowMs: number): string {
  if (ms === undefined || ms === null) return '';
  return formatRelative(new Date(ms).toISOString(), nowMs);
}

function nodeLeaf(nodeId: string): string {
  return nodeId.split('/').filter(Boolean).pop() ?? nodeId;
}

function commitRowSessionName(session: SessionSummary | undefined, sessionId: string): string {
  return session ? summarizePrompt(session.preview) || sessionId.slice(0, 8) : sessionId.slice(0, 8);
}

function commitRowAccessibleName(session: SessionSummary | undefined, name: string): string {
  return session ? `${session.harness} ${name}` : name;
}

/**
 * Relay for the canvas highlight: a hovered or focused task row reports the
 * files it edited (cleared with null on leave/blur) so the page can light up
 * the touched nodes; keyboard focus behaves exactly like hover.
 */
export type WorkHighlightHandler = (editedFiles: readonly string[] | null) => void;

/**
 * One task row: outcome glyph, one-line truncated title, the "touched" line
 * (top modules this task edited) and relative time.
 * Label chips render only with `showLabels`; the node panel keeps them, and
 * the project panel's Recent work dropped them (the touched line says WHAT
 * and WHERE, which the raw chips never did).
 */
function TaskRow({
  projectHash,
  task,
  originNode,
  returnTo,
  nowMs,
  showLabels = false,
  onHighlight,
}: {
  projectHash: ProjectHash;
  task: DecodedTaskSummary;
  originNode?: string;
  returnTo?: ReturnLocation;
  nowMs: number;
  showLabels?: boolean;
  onHighlight?: WorkHighlightHandler;
}) {
  const modules = touchedModules(task.editedFiles);
  return (
    <Link
      href={transcriptHref(projectHash, task.sessionId, { turn: task.entryIndex, origin: RouteOrigin.Map, originNode, returnLocation: returnTo })}
      aria-label={`Open task at turn ${task.entryIndex} of session ${task.sessionId}`}
      className="block px-1 py-1.5 -mx-1 hover:bg-surface-hover transition-colors focus-mono cursor-pointer"
      onMouseEnter={onHighlight ? () => onHighlight(task.editedFiles) : undefined}
      onMouseLeave={onHighlight ? () => onHighlight(null) : undefined}
      onFocus={onHighlight ? () => onHighlight(task.editedFiles) : undefined}
      onBlur={onHighlight ? () => onHighlight(null) : undefined}
    >
      {/* min-w-0 on BOTH flex rows (not just the truncate spans inside them): a
          flex container's own preferred width can still be driven by an
          unbounded white-space:nowrap descendant's max-content size unless the
          container itself is also allowed to shrink below that. Without it,
          this row silently widened past the 320px rail and got raggedly
          clipped (mid-word, no ellipsis) by the rail card's overflow-x:hidden
          instead of ellipsis-truncating at its own edge: a real divergence
          from the demo's task row, which sidesteps this entirely with a CSS
          Grid `minmax(0, 1fr)` middle column (fairtrade's .gmp-task-row). */}
      <span className="flex min-w-0 items-center gap-1.5">
        <OutcomeChip outcome={task.outcome} />
        <span className="min-w-0 flex-1 truncate text-[13px] text-ink" title={task.title}>
          {task.title || `turn ${task.entryIndex}`}
        </span>
        {task.retryLoop && (
          <RotateCw size={12} aria-label="Retry loop recorded" className="shrink-0 text-ink-3" />
        )}
      </span>
      <span className="mt-0.5 flex min-w-0 items-baseline justify-between gap-2">
        <span
          className="min-w-0 truncate font-mono text-[11px] text-ink-3"
          title={modules.join(' · ')}
        >
          {modules.join(' · ')}
        </span>
        <span className="shrink-0 font-mono tabular-nums text-[11px] text-ink-4">
          {relTime(task.startMs, nowMs)}
        </span>
      </span>
      {showLabels && task.labels.length > 0 && (
        <span className="mt-0.5 flex flex-wrap items-center gap-1.5">
          {task.labels.map((label) => (
            <span key={label} className="border border-rule px-1 py-px text-[10px] text-ink-3">
              {label}
            </span>
          ))}
        </span>
      )}
    </Link>
  );
}

// ---------------------------------------------------------------------------
// Project panel (unselected)
// ---------------------------------------------------------------------------

/** Cap the recent-work list at roughly the project's latest ten task titles. */
const RECENT_TASKS_LIMIT = 10;

export interface ProjectRailProps {
  projectHash: ProjectHash;
  projectName: string;
  /** Project-scoped, reverse-chronological. EVERY session, zero-touch included. */
  sessions: readonly SessionSummary[];
  /** Coverage summed from the graph's root nodes; null until the graph loads. */
  coverage: Coverage | null;
  /** True while the graph (which carries coverage) is still loading, so a
   *  blank coverage reads as "loading", not "no data". */
  coverageLoading?: boolean;
  /** Reverse-chron recorded tasks ("Recent work"); null while loading. */
  recentTasks: readonly DecodedTaskSummary[] | null;
  /** True while the tasks call is in flight. */
  recentTasksLoading?: boolean;
  /** Error line when the tasks call failed. */
  recentTasksError: string | null;
  /** Hover/focus relay: a row's edited files to canvas highlight. */
  onWorkHighlight?: WorkHighlightHandler;
  nowMs: number;
  returnTo?: ReturnLocation;
}

/**
 * The unselected rail content: identity, the one-sentence coverage line, the
 * "Recent work" recorded-stories block, and the session list.
 *
 * Returns ft-ui RailSection blocks for composition into RailShell's `rail`
 * prop. No outer RailShell wrapper; the ft-ui RailShell in MapPageClient
 * provides the 320px card on desktop and bottom-sheet on mobile.
 */
export function ProjectRail({
  projectHash,
  projectName,
  sessions,
  coverage,
  coverageLoading = false,
  recentTasks,
  recentTasksLoading = false,
  recentTasksError,
  onWorkHighlight,
  nowMs,
  returnTo,
}: ProjectRailProps) {
  return (
    <>
      {/* Identity: ft-ui section header "Project" (CSS renders mono-lowercase) + project name + session count. */}
      <RailSection title="Project" icon={FolderOpen}>
        <p className="text-sm font-medium text-ink">{displayProject(projectName)}</p>
        <p className="mt-0.5 font-mono tabular-nums text-xs text-ink-3">
          {sessions.length} session{sessions.length !== 1 ? 's' : ''}
        </p>
      </RailSection>

      <RailSection title="AI-built files">
        {coverage ? (
          <p className="text-[13px] text-ink-2">
            <span className="font-mono tabular-nums text-ink">{coverage.recorded}</span> of{' '}
            <span className="font-mono tabular-nums text-ink">{coverage.total}</span> files have a
            saved AI conversation behind them
          </p>
        ) : coverageLoading ? (
          <SkeletonBox className="h-4 w-40" />
        ) : (
          // Loaded, but no structure to measure (e.g. no git repo / unparsed
          // languages); say so plainly instead of a dash that could mean either.
          <p className="text-[13px] text-ink-4">No structure recorded yet.</p>
        )}
      </RailSection>

      <RailSection title="Recent AI conversations">
        {/* Clarifying sub-line: say what this list is and that hovering reveals
            what each conversation shaped on the canvas. */}
        <p className="mb-2 text-[11px] leading-snug text-ink-3">
          latest recorded conversations; hover one to light up what it
          shaped.
        </p>
        {recentTasks?.some((task) => task.outcome) && (
          <OutcomeHeuristicHelp className="mb-2" />
        )}
        {recentTasksError ? (
          <p className="text-[13px] text-danger">{recentTasksError}</p>
        ) : recentTasksLoading || recentTasks === null ? (
          // Loading: explicit shimmer lines, never a dash that also means "none".
          <Skeleton avatar={false} lines={3} label="Loading recent work" />
        ) : recentTasks.length === 0 ? (
          <p className="text-[13px] text-ink-4">No recorded tasks yet.</p>
        ) : (
          <ul className="flex flex-col">
            {recentTasks.slice(0, RECENT_TASKS_LIMIT).map((t) => (
              <li key={`${t.sessionId}:${t.entryIndex}`}>
                <TaskRow
                  projectHash={projectHash}
                  task={t}
                  nowMs={nowMs}
                  onHighlight={onWorkHighlight}
                  returnTo={returnTo}
                />
              </li>
            ))}
          </ul>
        )}
      </RailSection>

      <AllConversations projectHash={projectHash} sessions={sessions} nowMs={nowMs} returnTo={returnTo} />
    </>
  );
}

/** Initial cap on the "All conversations" list before "Show all". */
const ALL_SESSIONS_LIMIT = 25;
/** Show the filter box only once the list is long enough to need it. */
const FILTER_THRESHOLD = 8;

/**
 * The project's full session list: capped (D4) so a project with hundreds of
 * conversations doesn't render an unbounded DOM, with a substring filter over
 * the conversation summary + id and a "Show all N" expander. Every session
 * still appears; the cap only governs what's rendered at once.
 */
function AllConversations({
  projectHash,
  sessions,
  nowMs,
  returnTo,
}: {
  projectHash: ProjectHash;
  sessions: readonly SessionSummary[];
  nowMs: number;
  returnTo?: ReturnLocation;
}) {
  const [query, setQuery] = useState('');
  const [expanded, setExpanded] = useState(false);

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (!q) return sessions;
    return sessions.filter(
      (s) =>
        (summarizePrompt(s.preview) || '').toLowerCase().includes(q) ||
        s.id.toLowerCase().includes(q),
    );
  }, [sessions, query]);

  const shown = expanded ? filtered : filtered.slice(0, ALL_SESSIONS_LIMIT);
  const hidden = filtered.length - shown.length;

  return (
    <RailSection title="All conversations">
      {sessions.length === 0 ? (
        <p className="text-[13px] text-ink-4">No conversations recorded for this project.</p>
      ) : (
        <>
          {sessions.length > FILTER_THRESHOLD && (
            <input
              type="search"
              value={query}
              onChange={(e) => {
                setQuery(e.target.value);
                setExpanded(false);
              }}
              aria-label="Filter conversations"
              placeholder="Filter conversations…"
              className="mb-2 w-full border border-rule bg-surface px-2 py-1 text-[12px] text-ink placeholder:text-ink-4 focus-mono"
            />
          )}
          {filtered.length === 0 ? (
            <p className="text-[13px] text-ink-4">No conversations match.</p>
          ) : (
            <ul className="flex flex-col">
              {shown.map((s) => (
                <li key={s.id}>
                  <Link
                    href={transcriptHref(projectHash, s.id, { origin: RouteOrigin.Map, returnLocation: returnTo })}
                    aria-label={`Open the conversation ${s.id}`}
                    className="block px-1 py-1.5 -mx-1 hover:bg-surface-hover transition-colors focus-mono cursor-pointer"
                  >
                    <span className="block truncate text-[13px] text-ink">
                      {summarizePrompt(s.preview) || s.id.slice(0, 8)}
                    </span>
                    <span className="mt-0.5 flex items-center gap-2 font-mono tabular-nums text-[11px] text-ink-4">
                      <span>{formatRelative(s.startTime, nowMs)}</span>
                      <span>
                        {s.turnCount} request{s.turnCount !== 1 ? 's' : ''}
                      </span>
                    </span>
                  </Link>
                </li>
              ))}
            </ul>
          )}
          {hidden > 0 && (
            <button
              type="button"
              onClick={() => setExpanded(true)}
              className="mt-1.5 border border-rule bg-surface px-2 py-1 text-[12px] text-ink hover:bg-surface-hover transition-colors focus-mono cursor-pointer"
            >
              Show all {filtered.length}
            </button>
          )}
        </>
      )}
    </RailSection>
  );
}

// ---------------------------------------------------------------------------
// Node panel (selected)
// ---------------------------------------------------------------------------

export interface NodeRailProps {
  projectHash: ProjectHash;
  projectName: string;
  nodeId: string;
  /** Node-detail payload; null while the REST call is in flight. */
  detail: DecodedMapNodeDetailPayload | null;
  /** Error line when the node-detail call failed. */
  error: string | null;
  /**
   * Project-scoped sessions from the live `sessions` channel, the same list
   * `ProjectRail` renders. Used to resolve a commit row's `sessionIds` into a
   * harness + display name per session; a sessionId absent from this list
   * renders name-only (absent is unknown, never fabricated).
   */
  sessions: readonly SessionSummary[];
  /**
   * "Often edited with" rows: the graph payload's activityEdges
   * touching this node, top `MAX_COUPLED_NODES` by shared-task count.
   */
  coupling: readonly CoupledNode[];
  /** Clicking a coupling row selects that node on the canvas. */
  onSelectNode: (id: string) => void;
  onClose: () => void;
  /** Hover/focus relay: a row's edited files to canvas highlight. */
  onWorkHighlight?: WorkHighlightHandler;
  nowMs: number;
  returnTo?: ReturnLocation;
}

/** Close affordance for the node panel. */
function CloseButton({ onClose, label }: { onClose: () => void; label: string }) {
  return (
    <button
      type="button"
      aria-label={label}
      className="flex h-6 w-6 items-center justify-center border border-rule text-ink-3 hover:bg-surface-hover focus-mono cursor-pointer"
      onClick={onClose}
    >
      <X size={12} aria-hidden />
    </button>
  );
}

/** A structural-connection list (Depends on / Used by, 5.6): each row selects
 *  that code area on the canvas. ids are repo-relative node paths. */
function ConnectionList({
  title,
  ids,
  onSelectNode,
}: {
  title: string;
  ids: readonly string[];
  onSelectNode: (id: string) => void;
}) {
  return (
    <div>
      <p className="v2-eyebrow mb-1">{title}</p>
      <ul className="flex flex-col">
        {ids.map((id) => (
          <li key={id}>
            <button
              type="button"
              aria-label={`Select the code area ${id}`}
              className="-mx-1 flex w-full px-1 py-1 text-left hover:bg-surface-hover transition-colors focus-mono cursor-pointer"
              onClick={() => onSelectNode(id)}
            >
              <span className="min-w-0 truncate font-mono text-xs text-ink-2" title={id}>
                {id}
              </span>
            </button>
          </li>
        ))}
      </ul>
    </div>
  );
}

/** Quiet font-mono footnote row, never styled as a verdict. */
function FootnoteRow({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex items-center justify-between text-[12px]">
      <span className="text-ink-3">{label}</span>
      <span className="font-mono tabular-nums text-ink-2">{value}</span>
    </div>
  );
}

/**
 * The node panel content: identity · traceability · shaped by · often edited
 * with · footnotes · contribute.
 *
 * Returns ft-ui RailSection blocks for composition into RailShell's `rail`
 * prop. No outer RailShell wrapper; the ft-ui RailShell in MapPageClient
 * provides the 320px card on desktop and bottom-sheet on mobile.
 */
export function NodeRail({
  projectHash,
  projectName,
  nodeId,
  detail,
  error,
  sessions,
  coupling,
  onSelectNode,
  onClose,
  onWorkHighlight,
  nowMs,
  returnTo,
}: NodeRailProps) {
  const shapedRows = detail ? interleaveShapedBy(detail.shapedBy, detail.recentCommits) : [];
  const hasShapedOutcome = shapedRows.some((row) => row.kind === 'task' && row.task.outcome);
  const contributeIds = detail ? shapedBySessionIds(detail.shapedBy) : [];
  /** sessionId -> SessionSummary, for resolving a commit row's `sessionIds`
   *  into a harness + display name. */
  const sessionsById = useMemo(() => {
    const map = new Map<string, SessionSummary>();
    for (const s of sessions) {
      if (s.projectHash === projectHash) map.set(s.id, s);
    }
    return map;
  }, [projectHash, sessions]);

  return (
    <>
      {/* Identity: ft-ui section header "Code area" (CSS renders mono-lowercase) + leaf name
          + full path + close affordance + kind/lines. Merges the old panel header
          (eyebrow/title/meta) with the former no-label first RailSection. */}
      <RailSection title="Code area" icon={FileCode}>
        <div className="flex items-start justify-between gap-2">
          <div className="min-w-0">
            {/* The leaf name (was the panel title) stays prominent in the section body. */}
            <p className="text-sm font-medium text-ink mb-0.5">{nodeLeaf(nodeId)}</p>
            {/* Full path: rs-sec-body already sets overflow-wrap:anywhere for long paths. */}
            <p className="font-mono text-xs text-ink-2" title={detail ? detail.path : nodeId}>
              {detail ? detail.path : nodeId}
            </p>
            {detail?.language && (
              <p className="mt-1 text-[12px] text-ink-3">{detail.language}</p>
            )}
          </div>
          {/* Close button lives in the BODY (not a section meta slot) so it
              never nests inside a <button> element. */}
          <CloseButton onClose={onClose} label="Clear node selection" />
        </div>
        {detail && (
          <p className="mt-1.5 font-mono tabular-nums text-[12px] text-ink-3">
            {detail.kind}{detail.loc > 0 ? ` · ${detail.loc.toLocaleString()} lines` : ''}
          </p>
        )}
      </RailSection>

      {error && (
        <RailSection>
          <p className="text-[13px] text-danger">{error}</p>
        </RailSection>
      )}

      {!detail && !error && (
        <RailSection>
          <Skeleton avatar={false} lines={3} label="Loading details" />
        </RailSection>
      )}

      {detail && (
        <>
          {/* What this area connects to (5.6): the parsed import graph, what
              this area builds on, and what builds on it. Deterministic input
              only; rendered only when the graph yields connections (package
              grain), so module/file/activity-only nodes simply omit it. */}
          {(detail.dependsOn.length > 0 || detail.usedBy.length > 0) && (
            <RailSection title="What this area connects to">
              <p className="mb-2 text-[11px] leading-snug text-ink-3">
                Drawn from the code&rsquo;s imports: what this area builds on, and what builds on it.
              </p>
              {detail.dependsOn.length > 0 && (
                <ConnectionList title="Depends on" ids={detail.dependsOn} onSelectNode={onSelectNode} />
              )}
              {detail.usedBy.length > 0 && (
                <div className={detail.dependsOn.length > 0 ? 'mt-3' : ''}>
                  <ConnectionList title="Used by" ids={detail.usedBy} onSelectNode={onSelectNode} />
                </div>
              )}
            </RailSection>
          )}

          <RailSection title="AI-built files">
            {detail.sessionCount === 0 ? (
              <p className="text-[13px] text-ink-3">No recorded conversations touch this code.</p>
            ) : (
              <>
                <p className="text-[13px] text-ink-2">
                  <span className="font-mono tabular-nums text-ink">{detail.recordedFiles}</span> of{' '}
                  <span className="font-mono tabular-nums text-ink">{detail.totalFiles}</span> files
                  have a saved conversation behind them
                </p>
                <p className="mt-1 font-mono tabular-nums text-[12px] text-ink-3">
                  {detail.sessionCount} conversation{detail.sessionCount !== 1 ? 's' : ''} &middot;{' '}
                  {detail.taskCount} request{detail.taskCount !== 1 ? 's' : ''}
                </p>
                {detail.lastTouchMs !== undefined && (
                  <p className="mt-0.5 font-mono tabular-nums text-[12px] text-ink-4">
                    last: {relTime(detail.lastTouchMs, nowMs)}
                  </p>
                )}
              </>
            )}
          </RailSection>

          <RailSection title="Conversations that built this">
            {hasShapedOutcome && <OutcomeHeuristicHelp className="mb-2" />}
            {shapedRows.length === 0 ? (
              <p className="text-[13px] text-ink-4">No recorded conversations or updates.</p>
            ) : (
              <ul className="flex flex-col">
                {shapedRows.map((row) => (
                  <li key={row.kind === 'task' ? `t:${row.task.sessionId}:${row.task.entryIndex}` : `c:${row.commit.hash}`}>
                    {row.kind === 'task' ? (
                      <TaskRow
                        projectHash={projectHash}
                        task={row.task}
                        originNode={nodeId}
                        nowMs={nowMs}
                        showLabels
                        onHighlight={onWorkHighlight}
                        returnTo={returnTo}
                      />
                    ) : (
                      <div className="px-1 py-1.5 -mx-1">
                        {/* min-w-0 on the row itself: see the matching note on
                            TaskRow above; same overflow-past-the-rail bug. */}
                        <span className="flex min-w-0 items-center gap-1.5">
                          <span className="shrink-0 font-mono text-[11px] text-ink-3">
                            {row.commit.hash.slice(0, 7)}
                          </span>
                          <span
                            className="min-w-0 flex-1 truncate text-[13px] text-ink-2"
                            title={row.commit.subject}
                          >
                            {row.commit.subject}
                          </span>
                        </span>
                        <span className="mt-0.5 flex items-center gap-2 text-[11px]">
                          {row.commit.sessionIds.length === 0 && (
                            <span className="text-ink-4">no AI conversation captured</span>
                          )}
                          <span className="font-mono tabular-nums text-ink-4">
                            {relTime(row.commit.timeMs, nowMs)}
                          </span>
                        </span>
                        {row.commit.sessionIds.length > 0 && (
                          <ul
                            className="mt-1 flex flex-col gap-0.5"
                            aria-label={`Conversations behind commit ${row.commit.subject}`}
                          >
                            {row.commit.sessionIds.map((sessionId) => {
                              const session = sessionsById.get(sessionId);
                              const name = commitRowSessionName(session, sessionId);
                              const accessibleName = commitRowAccessibleName(session, name);
                              return (
                                <li key={sessionId} className="min-w-0">
                                  <Link
                                    href={transcriptHref(projectHash, sessionId, {
                                      origin: RouteOrigin.Map,
                                      originNode: nodeId,
                                      returnLocation: returnTo,
                                    })}
                                    aria-label={accessibleName}
                                    className="-mx-1 flex min-w-0 items-center gap-1.5 px-1 py-1 hover:bg-surface-hover transition-colors focus-mono cursor-pointer"
                                    style={{ fontSize: 'var(--fs-label)' }}
                                  >
                                    {session && (
                                      <ProviderName
                                        harness={session.harness}
                                        className="shrink-0"
                                        style={{ fontSize: 'var(--fs-label)' }}
                                      />
                                    )}
                                    <span
                                      className="min-w-0 flex-1 truncate text-ink-2"
                                      title={name}
                                    >
                                      {name}
                                    </span>
                                  </Link>
                                </li>
                              );
                            })}
                          </ul>
                        )}
                      </div>
                    )}
                  </li>
                ))}
              </ul>
            )}
          </RailSection>
        </>
      )}

      {/* Co-edit coupling reads as plain rows derived from the
          graph payload, so it renders even while the detail call is in
          flight or failed. An observation about the work, not the code. */}
      {coupling.length > 0 && (
        <RailSection title="Usually changed alongside">
          <p className="mb-2 text-[11px] leading-snug text-ink-3">
            These areas tend to change in the same conversations.
          </p>
          <ul className="flex flex-col">
            {coupling.map((c) => (
              <li key={c.id}>
                <button
                  type="button"
                  aria-label={`Select the code area ${c.id}`}
                  className="-mx-1 flex w-full items-baseline justify-between gap-2 px-1 py-1.5 text-left hover:bg-surface-hover transition-colors focus-mono cursor-pointer"
                  onClick={() => onSelectNode(c.id)}
                >
                  <span className="min-w-0 truncate font-mono text-xs text-ink-2" title={c.id}>
                    {c.id}
                  </span>
                  <span className="shrink-0 font-mono tabular-nums text-[11px] text-ink-4">
                    {c.taskCount} shared task{c.taskCount !== 1 ? 's' : ''}
                  </span>
                </button>
              </li>
            ))}
          </ul>
        </RailSection>
      )}

      {detail && (
        <>
          <RailSection title="Effort totals">
            <p className="mb-2 text-[11px] leading-snug text-ink-3">
              Across every recorded conversation that touched this area.
            </p>
            <div className="flex flex-col gap-1">
              <FootnoteRow label="times the AI retried" value={String(detail.retryLoops)} />
              <FootnoteRow label="files re-edited" value={String(detail.reEdits)} />
              <FootnoteRow label="files in this area" value={String(detail.totalFiles)} />
              {detail.costUsd != null && (
                <FootnoteRow label="estimated AI spend" value={`$${detail.costUsd.toFixed(2)}`} />
              )}
            </div>
          </RailSection>

          {contributeIds.length > 0 && (
            <RailSection>
              <Link
                href={contributeSessionsHref(contributeIds)}
                aria-label={`Contribute the ${contributeIds.length} sessions behind this node`}
                className="inline-flex items-center gap-1.5 text-[13px] text-ink underline underline-offset-2 hover:text-ink-2 transition-colors focus-mono cursor-pointer"
              >
                Contribute these sessions
                <ArrowRight size={12} aria-hidden />
              </Link>
            </RailSection>
          )}
        </>
      )}
    </>
  );
}
