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
 * Token totals are NOT included here — the GroupedMultiSelect renders its own
 * per-item token column. The share-lifecycle status is shown only when it is
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
// Session picker — project-grouped, tri-state multi-select. The selection
// contract is unchanged (session-id Set in/out); the body is the fairtrade
// GroupedMultiSelect (project rows → groups, sessions → items, the old
// session-count Badge → the group meta tally, the per-session Checkboxes → the
// tri-state boxes). The wizard's Continue control stays here as step chrome.
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
          return <section key={project.key} className="border-b border-rule last:border-b-0" aria-label={`project ${cleanName}`}>
            <h2 className="px-4 py-3 font-mono font-semibold" title={fullPath !== cleanName ? fullPath : undefined}>{cleanName}</h2>
            {project.locations.map((location) => <section key={location.locationLabel} className="ml-4 border-l border-rule" aria-label={`repository location ${location.locationLabel}`}>
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
