'use client';

import { useCallback, useMemo } from 'react';
import { GroupedMultiSelect, Button } from '@/lib/ft-ui';
import type { ShareSession } from '@/lib/share/types';
import { groupByProject, isSelectable } from '@/lib/share/group';
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
  sessions: ShareSession[];
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
  const groups = useMemo(
    () =>
      groupByProject(sessions).map((project) => {
        // Decode Claude-encoded / host-slug paths BEFORE truncation so the
        // group title is never an opaque blob. The clean name is the label; the
        // full path rides along as a native tooltip when it adds information.
        const cleanName = displayProject(project.projectName);
        const fullPath = decodeProjectPath(project.projectName);
        const showPath = fullPath !== cleanName;
        return {
          id: project.key,
          label: showPath ? (
            <span title={fullPath}>{cleanName}</span>
          ) : (
            cleanName
          ),
          items: project.sessions.map((s) => ({
            id: s.id,
            label: summarizePrompt(s.preview) || `${s.id.slice(5, 13)}…`,
            meta: sessionMeta(s),
            tokens: s.totalTokens,
          })),
        };
      }),
    [sessions],
  );

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

      <GroupedMultiSelect
        groups={groups}
        value={selectedIds}
        onChange={handleChange}
        tokenLabel="tokens"
        ariaLabel="choose sessions to contribute"
      />
    </div>
  );
}
