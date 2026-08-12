'use client';

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { Button, Checkbox } from '@/lib/ft-ui';
import type { ShareSession, ShareHierarchySession } from '@/lib/share/types';
import { groupShareHierarchy, isSelectable } from '@/lib/share/group';
import { decodeProjectPath, displayProject } from '@/lib/quality/utils';
import { summarizePrompt } from '@peasant-labs/transcript-browser';

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function formatDate(iso: string): string {
  const d = new Date(iso);
  return d.toLocaleDateString('en-US', { month: 'short', day: 'numeric' });
}

/**
 * Secondary metadata for one session row, shown in the item's `meta` slot.
 * Token totals are rendered alongside this metadata in the mounted hierarchy.
 * The share-lifecycle status is shown only when it is
 * something other than the default (`new`), and the heuristic outcome only when
 * one was computed — so a clean, newly-discovered session reads as `id · date`.
 */
function sessionMeta(s: ShareSession): string {
  const parts = [`${s.id.slice(5, 13)}…`, formatDate(s.startTime)];
  if (s.outcome) parts.push(s.outcome);
  if (s.shareStatus !== 'new') parts.push(s.shareStatus);
  return parts.join(' · ');
}

// ---------------------------------------------------------------------------
// Session picker — project → repository location → branch → session hierarchy.
// Selection remains an eligible session-id Set; project controls preserve the
// prior tri-state cascade while nested rows explain repository identity.
// ---------------------------------------------------------------------------

interface SessionPickerProps {
  sessions: ShareHierarchySession[];
  selectedIds: Set<string>;
  onSelectionChange: (ids: Set<string>) => void;
  onNext: () => void;
}

interface DescendantSelectionState {
  eligibleIds: string[];
  checked: boolean;
  mixed: boolean;
  disabled: boolean;
}

function descendantSelectionState(ids: string[], selectableIds: Set<string>, selectedIds: Set<string>): DescendantSelectionState {
  const eligibleIds = ids.filter((id) => selectableIds.has(id));
  const selectedCount = eligibleIds.filter((id) => selectedIds.has(id)).length;
  return {
    eligibleIds,
    checked: eligibleIds.length > 0 && selectedCount === eligibleIds.length,
    mixed: selectedCount > 0 && selectedCount < eligibleIds.length,
    disabled: eligibleIds.length === 0,
  };
}

function TriStateCheckbox({ checked, mixed, disabled, onChange, label }: {
  checked: boolean;
  mixed: boolean;
  disabled: boolean;
  onChange: () => void;
  label: string;
}) {
  const ref = useRef<HTMLInputElement>(null);
  useEffect(() => {
    if (ref.current) ref.current.indeterminate = mixed;
  }, [mixed]);
  return <label className="check share-hierarchy-check"><input ref={ref} type="checkbox" className="check-box" checked={checked} data-indeterminate={mixed || undefined} aria-checked={mixed ? 'mixed' : checked} disabled={disabled} onChange={onChange} aria-label={label} />{mixed && <span className="share-hierarchy-check__mixed" aria-hidden="true" />}</label>;
}

// ---------------------------------------------------------------------------
// Single-line hierarchy connector — one continuous Railway-style trace.
//
// The visible hierarchy is flattened into ordered rows carrying a typed depth
// (project 0 → repository location 1 → branch 2 → session 3). ONE continuous SVG
// path, measured from the mounted checkbox anchors, is traced through every row
// in document order: it stays vertical at the current depth column and takes a
// single square step at the deeper of two adjacent rows — in when descending,
// out when ascending — so at any y there is exactly one vertical segment. The
// step always lands in an indentation gutter, never across a row's label.
// Measuring the live anchors (instead of assuming row heights) keeps the trace
// continuous when rows wrap on a narrow viewport.
// ---------------------------------------------------------------------------

const RAIL_DEPTH = { project: 0, location: 1, branch: 2, session: 3 } as const;
/** Length (px) of the single start/end terminal cap of the trace. */
const RAIL_CAP = 6;

interface RailRow {
  key: string;
  depth: number;
}

interface RailAnchor {
  x: number;
  y: number;
  depth: number;
}

/**
 * Compose one continuous path from the ordered anchors. Between adjacent rows
 * the lone horizontal step sits at the DEEPER row's y (always an indentation
 * gutter), and the vertical changes column exactly once. The path opens and
 * closes with a single vertical cap collinear with the first/last segment, so
 * there is never a second line at any y.
 */
function buildRailPath(anchors: RailAnchor[]): string {
  if (anchors.length === 0) return '';
  const first = anchors[0];
  const parts = [`M ${first.x} ${first.y - RAIL_CAP}`, `L ${first.x} ${first.y}`];
  for (let i = 1; i < anchors.length; i++) {
    const prev = anchors[i - 1];
    const cur = anchors[i];
    if (cur.x === prev.x) {
      parts.push(`L ${cur.x} ${cur.y}`);
    } else if (cur.depth > prev.depth) {
      // Descending: fall at the parent column to the child row, then step in.
      parts.push(`L ${prev.x} ${cur.y}`, `L ${cur.x} ${cur.y}`);
    } else {
      // Ascending: step out at the deeper (previous) row, then fall to the row.
      parts.push(`L ${cur.x} ${prev.y}`, `L ${cur.x} ${cur.y}`);
    }
  }
  const last = anchors[anchors.length - 1];
  parts.push(`L ${last.x} ${last.y + RAIL_CAP}`);
  return parts.join(' ');
}

export function SessionPicker({
  sessions,
  selectedIds,
  onSelectionChange,
  onNext,
}: SessionPickerProps) {
  const groups = useMemo(() => groupShareHierarchy(sessions), [sessions]);

  // Flattened, ordered rows with typed depth — the spine the single rail traces.
  const orderedRows = useMemo<RailRow[]>(() => {
    const rows: RailRow[] = [];
    for (const project of groups) {
      rows.push({ key: `p:${project.key}`, depth: RAIL_DEPTH.project });
      for (const location of project.locations) {
        rows.push({ key: `l:${project.key}:${location.repositoryLocationId}`, depth: RAIL_DEPTH.location });
        for (const branch of location.branches) {
          rows.push({ key: `b:${project.key}:${location.repositoryLocationId}:${branch.branch}`, depth: RAIL_DEPTH.branch });
          for (const session of branch.sessions) {
            rows.push({ key: `s:${session.id}`, depth: RAIL_DEPTH.session });
          }
        }
      }
    }
    return rows;
  }, [groups]);

  const containerRef = useRef<HTMLDivElement>(null);
  const rowRefs = useRef(new Map<string, HTMLElement>());
  const setRowRef = useCallback((key: string) => (el: HTMLElement | null) => {
    if (el) rowRefs.current.set(key, el);
    else rowRefs.current.delete(key);
  }, []);
  const [railPath, setRailPath] = useState('');

  useEffect(() => {
    const container = containerRef.current;
    if (!container) return;
    let frame = 0;
    const measure = () => {
      const containerRect = container.getBoundingClientRect();
      const anchors: RailAnchor[] = [];
      for (const row of orderedRows) {
        const el = rowRefs.current.get(row.key);
        const input = el?.querySelector('input[type="checkbox"]') as HTMLElement | null;
        if (!input) return; // not laid out yet — a later observer tick retries
        const rect = input.getBoundingClientRect();
        anchors.push({
          x: rect.left + rect.width / 2 - containerRect.left,
          y: rect.top + rect.height / 2 - containerRect.top,
          depth: row.depth,
        });
      }
      if (anchors.length === 0) { setRailPath(''); return; }
      // Snap each depth to one column (its median x) so every vertical at a
      // given depth is exactly aligned — one provably single line per column.
      const byDepth = new Map<number, number[]>();
      for (const anchor of anchors) {
        const list = byDepth.get(anchor.depth) ?? [];
        list.push(anchor.x);
        byDepth.set(anchor.depth, list);
      }
      const column = new Map<number, number>();
      for (const [depth, xs] of byDepth) {
        const sorted = xs.slice().sort((a, b) => a - b);
        column.set(depth, sorted[Math.floor(sorted.length / 2)]);
      }
      for (const anchor of anchors) anchor.x = column.get(anchor.depth)!;
      setRailPath(buildRailPath(anchors));
    };
    const schedule = () => {
      if (typeof cancelAnimationFrame === 'function') cancelAnimationFrame(frame);
      frame = typeof requestAnimationFrame === 'function' ? requestAnimationFrame(measure) : (measure(), 0);
    };
    schedule();
    const observer = typeof ResizeObserver !== 'undefined' ? new ResizeObserver(schedule) : null;
    if (observer) {
      observer.observe(container);
      for (const el of rowRefs.current.values()) observer.observe(el);
    }
    window.addEventListener('resize', schedule);
    return () => {
      if (typeof cancelAnimationFrame === 'function') cancelAnimationFrame(frame);
      observer?.disconnect();
      window.removeEventListener('resize', schedule);
    };
  }, [orderedRows, selectedIds]);

  // The hierarchy shows every session so its counts stay honest. Every row,
  // project, evidence, and select-all action is intersected with this set.
  const selectableIds = useMemo(
    () => new Set(sessions.filter(isSelectable).map((s) => s.id)),
    [sessions],
  );

  const handleChange = useCallback(
    (next: Set<string>) => {
      const filtered = new Set<string>();
      for (const id of next) if (selectableIds.has(id)) filtered.add(id);
      onSelectionChange(filtered);
    },
    [selectableIds, onSelectionChange],
  );

  const toggleDescendants = useCallback((ids: string[]) => {
    const state = descendantSelectionState(ids, selectableIds, selectedIds);
    const next = new Set(selectedIds);
    for (const id of state.eligibleIds) state.checked ? next.delete(id) : next.add(id);
    handleChange(next);
  }, [handleChange, selectableIds, selectedIds]);

  const selectedCount = selectedIds.size;
  const selectedTokens = sessions.reduce((total, session) => selectedIds.has(session.id) ? total + session.totalTokens : total, 0);
  const selectedTokensLabel = selectedTokens >= 1000 ? `${Math.round(selectedTokens / 1000)}k` : String(selectedTokens);

  return (
    <div className="flex flex-col gap-4">
      {/* Step chrome — the forward control. Stays here (the wizard owns
          next/back); the select-all + running tally live inside the picker
          body's own toolbar. */}
      <div className="sticky top-16 z-30 flex items-center justify-end px-5 py-3 bg-surface border border-rule">
        <Button
          variant="primary"
          size="sm"
          onClick={onNext}
          disabled={selectedCount === 0}
        >
          Continue
        </Button>
      </div>

      <div className="border border-rule bg-surface" aria-label="choose sessions to contribute">
        <div className="px-4 py-3 border-b border-rule flex items-center justify-between gap-3">
          <div className="gms-tally font-mono text-sm tabular-nums">{selectedCount} selected · {selectedTokensLabel} tokens</div>
          <Button size="sm" variant="ghost" pressed={selectedCount === selectableIds.size && selectableIds.size > 0} onClick={() => handleChange(selectedCount === selectableIds.size ? new Set() : new Set(selectableIds))}>
            {selectedCount === selectableIds.size && selectableIds.size > 0 ? 'deselect all' : 'select all'}
          </Button>
        </div>
        <div ref={containerRef} className="relative">
          <svg className="share-rail" aria-hidden="true">
            {railPath && <path className="share-rail__path" d={railPath} shapeRendering="crispEdges" strokeLinecap="square" />}
          </svg>
          <div className="share-rail-rows">
        {groups.map((project) => {
          const cleanName = displayProject(project.projectName);
          const fullPath = decodeProjectPath(project.projectName);
          const projectIds = project.locations.flatMap((location) => location.branches.flatMap((branch) => branch.sessions.map((session) => session.id)));
           const projectState = descendantSelectionState(projectIds, selectableIds, selectedIds);
          return <section key={project.key} className="border-b border-rule last:border-b-0" aria-label={`project ${cleanName}`}>
            <h2 ref={setRowRef(`p:${project.key}`)} className="px-4 py-3 font-mono font-semibold flex items-center gap-3" title={fullPath !== cleanName ? fullPath : undefined}>
               <TriStateCheckbox {...projectState} onChange={() => toggleDescendants(projectIds)} label={`select project ${cleanName}`} />
              {cleanName}
            </h2>
            <div className="share-subtree">
            {project.locations.map((location) => {
              const locationIds = location.branches.flatMap((branch) => branch.sessions.map((session) => session.id));
              return <section key={location.repositoryLocationId} className="share-node" aria-label={`repository location ${location.locationLabel}`}>
              <h3 ref={setRowRef(`l:${project.key}:${location.repositoryLocationId}`)} className="px-4 py-2 font-mono text-sm text-ink-2 flex items-center gap-3"><TriStateCheckbox {...descendantSelectionState(locationIds, selectableIds, selectedIds)} onChange={() => toggleDescendants(locationIds)} label={`select repository location ${location.locationLabel}`} />repository location · {location.locationLabel}</h3>
              <div className="share-subtree">
              {location.branches.map((branch) => {
                const branchIds = branch.sessions.map((session) => session.id);
                return <section key={branch.branch} className="share-node" aria-label={`branch ${branch.branch || 'unknown'}`}>
                <h4 ref={setRowRef(`b:${project.key}:${location.repositoryLocationId}:${branch.branch}`)} className="px-4 py-2 font-mono text-sm text-ink-3 flex items-center gap-3"><TriStateCheckbox {...descendantSelectionState(branchIds, selectableIds, selectedIds)} onChange={() => toggleDescendants(branchIds)} label={`select branch ${branch.branch || 'unknown'}`} />branch · {branch.branch || 'unknown'}</h4>
                <div className="share-subtree">
                  {branch.sessions.map((session) => <div key={session.id} ref={setRowRef(`s:${session.id}`)} className="flex items-center gap-3 px-4 py-3">
                    <Checkbox checked={selectedIds.has(session.id)} disabled={!selectableIds.has(session.id)} onChange={(checked) => {
                      const next = new Set(selectedIds);
                      if (checked) next.add(session.id); else next.delete(session.id);
                      handleChange(next);
                    }} aria-label={`select session ${session.id}`} />
                    <div className="min-w-0"><div>{summarizePrompt(session.preview) || `${session.id.slice(5, 13)}…`}</div><div className="font-mono text-xs text-ink-3">{sessionMeta(session)} · {session.totalTokens.toLocaleString()} tokens</div></div>
                  </div>)}
                </div>
              </section>;})}
              </div>
            </section>;})}
            </div>
          </section>;
        })}
          </div>
        </div>
      </div>
    </div>
  );
}
