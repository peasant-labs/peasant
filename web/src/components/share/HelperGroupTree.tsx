'use client';

import { useCallback, useState, type ReactNode } from 'react';
import { HelperGroup, HelperGroupListItem, HelperThreadRow } from '@/lib/ft-ui';
import type { ShareHelperGroup, ShareSession } from '@/lib/share/types';

/**
 * One decorated helper member row, mapped by the host from its typed wire row.
 * The fairtrade tree never decodes the wire: it renders exactly the authorized
 * page the host fetched for the exact scope. `session` carries the host's own
 * session projection so the chooser can feed the selected member to the
 * downstream contribution steps by its explicit transcript id.
 */
export interface HelperMemberRow {
  id: string;
  title: string;
  provider: string;
  inputSubmissionCount?: number;
  turnCount?: number;
  /** False when the row's route summary says it cannot be contributed. */
  selectable: boolean;
  session: ShareSession;
}

export interface HelperMembersPage {
  members: HelperMemberRow[];
  total: number;
  page: number;
  limit: number;
}

interface GroupState {
  rows: HelperMemberRow[];
  total: number;
  page: number;
  limit: number;
  loading: boolean;
  loaded: boolean;
  expired: boolean;
  error: string | null;
}

const HELPER_MEMBERS_LIMIT = 20;

const EMPTY_GROUP: GroupState = {
  rows: [],
  total: 0,
  page: 1,
  limit: HELPER_MEMBERS_LIMIT,
  loading: false,
  loaded: false,
  expired: false,
  error: null,
};

export interface HelperGroupTreeProps {
  groups: ShareHelperGroup[];
  selectedIds: Set<string>;
  onMemberToggle: (id: string, checked: boolean) => void;
  loadMembers: (group: ShareHelperGroup, page: number, limit: number) => Promise<HelperMembersPage>;
  /**
   * The originating grouped list must be refreshed when a member scope is
   * refused. The tree never falls back to an all-members request.
   */
  onScopeExpired: () => void;
  owner?: ReactNode;
  ownerStatus?: string;
}

/**
 * One owner-anchored helper tree on the share chooser. It composes the
 * canonical fairtrade primitives and owns ONLY the host-side concerns the
 * design system deliberately leaves out: fetching each group's authorized page
 * for its exact scope, paging, and the fail-closed refresh when a scope
 * expires. Selection state stays with the chooser; members are explicit
 * transcript IDs and never a group or owner aggregate.
 */
export function HelperGroupTree({
  groups,
  selectedIds,
  onMemberToggle,
  loadMembers,
  onScopeExpired,
  owner,
  ownerStatus,
}: HelperGroupTreeProps) {
  const [stateByGroup, setStateByGroup] = useState<Record<string, GroupState>>({});
  const [expandedByGroup, setExpandedByGroup] = useState<Record<string, boolean>>({});

  // A changed scope is a different query: keying state by group AND scope means
  // a refreshed list starts folded instead of reusing rows fetched for the old
  // scope. The fairtrade group remounts on the same boundary.
  const treeKey = useCallback((group: ShareHelperGroup) => `${group.groupId}:${group.memberScope}`, []);

  const stateOf = useCallback(
    (groupId: string): GroupState => stateByGroup[groupId] ?? EMPTY_GROUP,
    [stateByGroup],
  );

  const fetchPage = useCallback(
    async (group: ShareHelperGroup, page: number) => {
      const limit = HELPER_MEMBERS_LIMIT;
      const key = `${group.groupId}:${group.memberScope}`;
      setStateByGroup((previous) => ({
        ...previous,
        [key]: { ...(previous[key] ?? EMPTY_GROUP), loading: true, expired: false, error: null },
      }));
      try {
        const result = await loadMembers(group, page, limit);
        setStateByGroup((previous) => ({
          ...previous,
          [key]: {
            rows: result.members,
            total: result.total,
            page: result.page,
            limit: result.limit,
            loading: false,
            loaded: true,
            expired: false,
            error: null,
          },
        }));
      } catch (error) {
        // A refused scope is fail-closed: the group hides its members and only
        // an originating-list refresh is offered, never a broader load.
        if (error instanceof HelperScopeExpiredError) {
          setStateByGroup((previous) => ({
            ...previous,
            [key]: { ...(previous[key] ?? EMPTY_GROUP), loading: false, loaded: false, expired: true, error: null },
          }));
          onScopeExpired();
          return;
        }
        setStateByGroup((previous) => ({
          ...previous,
          [key]: {
            ...(previous[key] ?? EMPTY_GROUP),
            loading: false,
            loaded: false,
            expired: false,
            error: error instanceof Error ? error.message : 'the helper query failed',
          },
        }));
      }
    },
    [loadMembers, onScopeExpired],
  );

  const handleExpandedChange = useCallback(
    (group: ShareHelperGroup, expanded: boolean) => {
      const key = treeKey(group);
      setExpandedByGroup((previous) => ({ ...previous, [key]: expanded }));
      if (!expanded) return;
      const state = stateByGroup[key] ?? EMPTY_GROUP;
      if (state.loaded || state.loading || state.expired) return;
      void fetchPage(group, 1);
    },
    [fetchPage, stateByGroup, treeKey],
  );

  const renderMember = useCallback(
    (row: HelperMemberRow) => (
      <HelperThreadRow
        id={row.id}
        title={row.title}
        provider={row.provider}
        inputSubmissionCount={row.inputSubmissionCount}
        turnCount={row.turnCount}
        selected={selectedIds.has(row.id)}
        selectionDisabled={!row.selectable}
        onSelect={onMemberToggle}
      />
    ),
    [onMemberToggle, selectedIds],
  );

  return (
    <HelperGroupListItem owner={owner} ownerStatus={owner ? undefined : ownerStatus}>
      {groups.map((group) => {
        const key = treeKey(group);
        const state = stateOf(key);
        const pageCount = Math.max(1, Math.ceil(state.total / state.limit));
        return (
          <HelperGroup
            key={key}
            groupId={group.groupId}
            memberScope={group.memberScope}
            helperThreadCount={group.helperThreadCount}
            members={state.rows}
            renderMember={renderMember}
            getMemberKey={(row) => row.id}
            expanded={expandedByGroup[key] ?? false}
            onExpandedChange={(expanded) => handleExpandedChange(group, expanded)}
            scopeExpired={state.expired}
            onRefreshList={onScopeExpired}
            isMemberSelected={(row) => selectedIds.has(row.id as string)}
            memberFooter={
              state.loaded && state.total > state.limit ? (
                <div className="share-helper-paging flex items-center gap-3 font-mono text-xs text-ink-3">
                  <span className="tabular-nums">
                    page {state.page} of {pageCount}
                  </span>
                  <button
                    type="button"
                    className="underline underline-offset-2 disabled:opacity-40"
                    disabled={state.page <= 1 || state.loading}
                    onClick={() => void fetchPage(group, state.page - 1)}
                  >
                    previous
                  </button>
                  <button
                    type="button"
                    className="underline underline-offset-2 disabled:opacity-40"
                    disabled={state.page >= pageCount || state.loading}
                    onClick={() => void fetchPage(group, state.page + 1)}
                  >
                    next
                  </button>
                </div>
              ) : undefined
            }
          />
        );
      })}
    </HelperGroupListItem>
  );
}

/** Raised by the member loader when the server refuses the scope (409). */
export class HelperScopeExpiredError extends Error {
  constructor() {
    super('the helper member scope expired; refresh the originating list');
    this.name = 'HelperScopeExpiredError';
  }
}
