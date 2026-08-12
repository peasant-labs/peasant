'use client';

import { useCallback, useMemo } from 'react';
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

export function SessionPicker({
  sessions,
  selectedIds,
  onSelectionChange,
  onNext,
}: SessionPickerProps) {
  const groups = useMemo(() => groupShareHierarchy(sessions), [sessions]);

  // Only `new`/`updated` sessions can be contributed. The picker still shows
  // every session (so the project counts stay honest), but a non-selectable
  // session can never enter the selection: we intersect every change GMS
  // reports — including select-all and per-group cascades — with this set.
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

  const toggleGroup = useCallback((ids: string[]) => {
    const eligible = ids.filter((id) => selectableIds.has(id));
    const allSelected = eligible.length > 0 && eligible.every((id) => selectedIds.has(id));
    const next = new Set(selectedIds);
    for (const id of eligible) allSelected ? next.delete(id) : next.add(id);
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
        {groups.map((project) => {
          const cleanName = displayProject(project.projectName);
          const fullPath = decodeProjectPath(project.projectName);
          const projectIds = project.locations.flatMap((location) => location.branches.flatMap((branch) => branch.sessions.map((session) => session.id)));
          const eligibleProjectIds = projectIds.filter((id) => selectableIds.has(id));
          const projectSelectedCount = eligibleProjectIds.filter((id) => selectedIds.has(id)).length;
          return <section key={project.key} className="border-b border-rule last:border-b-0" aria-label={`project ${cleanName}`}>
            <h2 className="px-4 py-3 font-mono font-semibold flex items-center gap-3" title={fullPath !== cleanName ? fullPath : undefined}>
              <Checkbox checked={eligibleProjectIds.length > 0 && projectSelectedCount === eligibleProjectIds.length} aria-checked={projectSelectedCount > 0 && projectSelectedCount < eligibleProjectIds.length ? 'mixed' : undefined} disabled={eligibleProjectIds.length === 0} onChange={() => toggleGroup(projectIds)} aria-label={`select project ${cleanName}`} />
              {cleanName}
            </h2>
            {project.locations.map((location) => <section key={location.repositoryLocationId} className="ml-4 border-l border-rule" aria-label={`repository location ${location.locationLabel}`}>
              <h3 className="px-4 py-2 font-mono text-sm text-ink-2">repository location · {location.locationLabel}</h3>
              {location.branches.map((branch) => <section key={branch.branch} className="ml-4 border-l border-rule" aria-label={`branch ${branch.branch || 'unknown'}`}>
                <h4 className="px-4 py-2 font-mono text-sm text-ink-3">branch · {branch.branch || 'unknown'}</h4>
                <div className="ml-4">
                  {branch.sessions.map((session) => <div key={session.id} className="flex items-center gap-3 px-4 py-3 border-t border-rule">
                    <Checkbox checked={selectedIds.has(session.id)} disabled={!selectableIds.has(session.id)} onChange={(checked) => {
                      const next = new Set(selectedIds);
                      if (checked) next.add(session.id); else next.delete(session.id);
                      handleChange(next);
                    }} aria-label={`select session ${session.id}`} />
                    <div className="min-w-0"><div>{summarizePrompt(session.preview) || `${session.id.slice(5, 13)}…`}</div><div className="font-mono text-xs text-ink-3">{sessionMeta(session)} · {session.totalTokens.toLocaleString()} tokens</div></div>
                  </div>)}
                </div>
              </section>)}
            </section>)}
          </section>;
        })}
      </div>
    </div>
  );
}
