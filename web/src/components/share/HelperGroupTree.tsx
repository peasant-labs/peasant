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
  /**
   * Saved helper groups this member itself anchors, from the member page's own
   * `helperGroups`. Each carries its own opaque scope and is mounted as a
   * separate, independently paged disclosure under this member's row. Selection
   * stays per-transcript-id: a nested group never selects its owner or a scope.
   */
  helperGroups?: ShareHelperGroup[];
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

interface HelperGroupBranchProps {
  groups: ShareHelperGroup[];
  selectedIds: Set<string>;
  onMemberToggle: (id: string, checked: boolean) => void;
  loadMembers: (group: ShareHelperGroup, page: number, limit: number) => Promise<HelperMembersPage>;
  onScopeExpired: () => void;
}

/** One group's key is its identity AND its scope: a re-minted scope is a new query. */
function groupKey(group: ShareHelperGroup): string {
  return `${group.groupId}:${group.memberScope}`;
}

/**
 * The member rows and their own nested helper groups for ONE disclosure level.
 *
 * Each level owns the paging and expiry state of exactly the groups it renders,
 * keyed by group id and scope, so paging a nested group never disturbs its
 * parent or a sibling. A member that anchors further groups renders those under
 * its own row, where the fairtrade tree deepens one more indent column. State
 * lives here (never in the fairtrade components), and selection always stays an
 * explicit transcript id with the chooser.
 */
function HelperGroupBranch({ groups, selectedIds, onMemberToggle, loadMembers, onScopeExpired }: HelperGroupBranchProps) {
  const [stateByGroup, setStateByGroup] = useState<Record<string, GroupState>>({});
  const [expandedByGroup, setExpandedByGroup] = useState<Record<string, boolean>>({});

  const fetchPage = useCallback(
    async (group: ShareHelperGroup, page: number) => {
      const limit = HELPER_MEMBERS_LIMIT;
      const key = groupKey(group);
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
      const key = groupKey(group);
      setExpandedByGroup((previous) => ({ ...previous, [key]: expanded }));
      if (!expanded) return;
      const state = stateByGroup[key] ?? EMPTY_GROUP;
      if (state.loaded || state.loading || state.expired) return;
      void fetchPage(group, 1);
    },
    [fetchPage, stateByGroup],
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
      >
        {row.helperGroups?.length ? (
          <HelperGroupBranch
            groups={row.helperGroups}
            selectedIds={selectedIds}
            onMemberToggle={onMemberToggle}
            loadMembers={loadMembers}
            onScopeExpired={onScopeExpired}
          />
        ) : null}
      </HelperThreadRow>
    ),
    [loadMembers, onMemberToggle, onScopeExpired, selectedIds],
  );

  return (
    <>
      {groups.map((group) => {
        const key = groupKey(group);
        const state = stateByGroup[key] ?? EMPTY_GROUP;
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
              state.error ? (
                <p className="share-helper-error text-xs text-danger" role="alert">
                  {state.error}
                </p>
              ) : state.loaded && state.total > state.limit ? (
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
    </>
  );
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
  return (
    <HelperGroupListItem owner={owner} ownerStatus={owner ? undefined : ownerStatus}>
      <HelperGroupBranch
        groups={groups}
        selectedIds={selectedIds}
        onMemberToggle={onMemberToggle}
        loadMembers={loadMembers}
        onScopeExpired={onScopeExpired}
      />
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
