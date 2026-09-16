'use client';

import { useCallback, useState, type ReactNode } from 'react';
import {
  HelperGroup,
  HelperGroupListItem,
  HelperThreadRow,
  type HelperThreadRowProps,
} from '@/lib/ft-ui';
import {
  fetchHelperGroupMembers,
  isGroupScopeExpired,
  type GroupedMemberRow,
  type LocalSessionListPayload,
  type LocalSessionListItem,
  type LocalSessionRow,
} from '@/lib/api/grouped';
import { parseProjectHash, transcriptHref } from '@/lib/navigation/projectRoutes';
import {
  firstHelperMemberPage,
  helperMemberPageWindow,
  withHelperMemberTotal,
  type HelperMemberPaging,
} from '@/lib/sessions/helperGroups';

/**
 * Explicit, host-owned per-member selection.
 *
 * Selecting one member adds or removes EXACTLY that member's identity: the
 * owner row and the owner's other helpers are never implied, so a selection can
 * never widen to a parent, a sibling or a pulled transcript. A host that draws
 * per-member checkboxes supplies this; a browse-only mount omits it.
 */
export interface GroupedSelection {
  selectedIds: ReadonlySet<string>;
  onSelect(id: string, selected: boolean): void;
}

export interface GroupedLocalSessionsProps {
  payload: LocalSessionListPayload;
  /**
   * Refresh the originating grouped list. This is the ONLY recovery from an
   * expired member scope; the component never falls back to all members.
   */
  onRefreshList: () => void;
  /**
   * sessionId -> generated title, from useSessionTitles(). Partial by design:
   * a row without an entry falls back to the redaction-safe preview, then the
   * short session id.
   */
  titles?: ReadonlyMap<string, string>;
  /** Optional explicit per-member selection. */
  selection?: GroupedSelection;
  /** Shown when the payload carries no items. */
  emptyState?: ReactNode;
}

/** Short, stable handle for a session id when no title or preview exists. */
function shortId(id: string): string {
  return id.length > 8 ? id.slice(0, 8) : id;
}

/** Display title for one session row: generated title, preview, then id. */
export function groupedRowTitle(
  row: LocalSessionRow,
  titles?: ReadonlyMap<string, string>,
): string {
  const generated = titles?.get(row.session.id);
  if (generated) return generated;
  const preview = row.session.preview?.trim();
  if (preview) return preview;
  return shortId(row.session.id);
}

/** The individual identity of one item, or null for a context container. */
function itemSessionId(item: LocalSessionListItem): string | null {
  return item.transcript?.session.id ?? null;
}

/** The stable key for one item's rendered row. */
function itemKey(item: LocalSessionListItem): string {
  return itemSessionId(item) ?? item.context?.groupId ?? JSON.stringify(item);
}

function rowPresentation(
  session: LocalSessionRow['session'],
  titles?: ReadonlyMap<string, string>,
): Pick<HelperThreadRowProps, 'id' | 'title' | 'provider' | 'inputSubmissionCount' | 'turnCount' | 'href'> {
  const projectHash = parseProjectHash(session.projectHash);
  return {
    id: session.id,
    title: groupedRowTitle({ session }, titles),
    provider: session.harness,
    inputSubmissionCount: session.inputSubmissionCount,
    turnCount: session.turnCount,
    href: projectHash ? transcriptHref(projectHash, session.id) : undefined,
  };
}

/**
 * Render one session row. A helper member may itself own helpers, so the same
 * presenter recurses through `HelperGroupListItem` when the row carries its own
 * groups — the immediate-owner grouping is preserved rather than flat-mapped.
 */
function SessionRow({
  row,
  groups,
  titles,
  selection,
  onRefreshList,
}: {
  row: LocalSessionRow;
  groups: NonNullable<LocalSessionListItem['helperGroups']>;
  titles?: ReadonlyMap<string, string>;
  selection?: GroupedSelection;
  onRefreshList: () => void;
}) {
  const presentation = rowPresentation(row.session, titles);
  const selected = selection?.selectedIds.has(row.session.id) ?? false;
  const ownerRow = (
    <HelperThreadRow
      {...presentation}
      selected={selection ? selected : undefined}
      onSelect={selection ? (id, next) => selection.onSelect(id, next) : undefined}
    />
  );
  if (groups.length === 0) return ownerRow;
  return (
    <HelperGroupListItem owner={ownerRow}>
      {groups.map((group) => (
        <MountedHelperGroup
          key={`${group.groupId}:${group.memberScope}`}
          groupId={group.groupId}
          memberScope={group.memberScope}
          helperThreadCount={group.helperThreadCount}
          titles={titles}
          selection={selection}
          onRefreshList={onRefreshList}
        />
      ))}
    </HelperGroupListItem>
  );
}

/**
 * One expanded helper group: it owns ITS OWN member paging state, so two
 * rendered groups never share a page, a page size or a loaded member set.
 *
 * Members are fetched lazily when the disclosure opens, using the opaque scope
 * the list issued — never a widened query. An expired scope fails closed: the
 * members stay hidden and only an originating-list refresh is offered.
 */
function MountedHelperGroup({
  groupId,
  memberScope,
  helperThreadCount,
  titles,
  selection,
  onRefreshList,
}: {
  groupId: string;
  memberScope: string;
  helperThreadCount: number;
  titles?: ReadonlyMap<string, string>;
  selection?: GroupedSelection;
  onRefreshList: () => void;
}) {
  const [expanded, setExpanded] = useState(false);
  const [paging, setPaging] = useState<HelperMemberPaging>(() => firstHelperMemberPage());
  const [members, setMembers] = useState<GroupedMemberRow[] | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [scopeExpired, setScopeExpired] = useState(false);

  const load = useCallback(
    async (page: number) => {
      setLoading(true);
      setError(null);
      try {
        const payload = await fetchHelperGroupMembers({
          groupId,
          scope: memberScope,
          page,
          limit: paging.limit,
        });
        setMembers(payload.members);
        setPaging((current) =>
          withHelperMemberTotal(current, { page: payload.page, total: payload.total }),
        );
      } catch (cause) {
        if (isGroupScopeExpired(cause)) {
          setScopeExpired(true);
          setMembers(null);
        } else {
          setError(cause);
        }
      } finally {
        setLoading(false);
      }
    },
    [groupId, memberScope, paging.limit],
  );

  const handleExpandedChange = useCallback(
    (next: boolean) => {
      setExpanded(next);
      if (next && !scopeExpired && members === null && !loading) void load(paging.page);
    },
    [scopeExpired, members, loading, load, paging.page],
  );

  const pageWindow = helperMemberPageWindow(paging);
  const memberRows = members ?? [];
  const isMemberSelected =
    selection === undefined ? undefined : (row: unknown) => {
      const item = row as GroupedMemberRow;
      const id = itemSessionId(item);
      return id !== null && selection.selectedIds.has(id);
    };

  let footer: ReactNode = null;
  if (expanded && !scopeExpired && members !== null) {
    footer = (
      <div
        className="flex items-center justify-between gap-3 px-3 py-2"
        data-helper-member-paging="true"
      >
        <span className="font-mono text-xs text-ink-4 tabular-nums">
          page {paging.page} of {pageWindow.pageCount}
        </span>
        <span className="flex items-center gap-2">
          <button
            type="button"
            onClick={() => void load(paging.page - 1)}
            disabled={!pageWindow.hasPrevious || loading}
            className="border border-rule px-3 py-1 font-mono text-xs text-ink-2 hover:bg-surface-hover disabled:opacity-40 disabled:pointer-events-none focus-mono cursor-pointer"
          >
            previous
          </button>
          <button
            type="button"
            onClick={() => void load(paging.page + 1)}
            disabled={!pageWindow.hasNext || loading}
            className="border border-rule px-3 py-1 font-mono text-xs text-ink-2 hover:bg-surface-hover disabled:opacity-40 disabled:pointer-events-none focus-mono cursor-pointer"
          >
            next
          </button>
        </span>
      </div>
    );
  }

  return (
    <>
      <HelperGroup
        groupId={groupId}
        memberScope={memberScope}
        helperThreadCount={helperThreadCount}
        members={memberRows}
        getMemberKey={(row: unknown) => itemKey(row as GroupedMemberRow)}
        renderMember={(row: unknown) => {
          const member = (row as GroupedMemberRow).transcript;
          if (!member) {
            throw new Error(
              'The grouped helper member could not be rendered because the member item in renderMember is not a transcript row. No member was displayed, because a non-transcript member would misstate the saved helper identity. Refetch the member page; if it repeats, confirm the Peasant server and @peasant-labs/schema contract versions match.',
            );
          }
          return (
            <SessionRow
              row={member}
              groups={(row as GroupedMemberRow).helperGroups ?? []}
              titles={titles}
              selection={selection}
              onRefreshList={onRefreshList}
            />
          );
        }}
        expanded={expanded}
        onExpandedChange={handleExpandedChange}
        scopeExpired={scopeExpired}
        onRefreshList={onRefreshList}
        isMemberSelected={isMemberSelected}
        memberFooter={footer}
      />
      {error !== null && (
        <p role="alert" className="px-3 py-2 text-xs text-danger">
          {error instanceof Error ? error.message : String(error)}
        </p>
      )}
    </>
  );
}

/**
 * The mounted grouped local list/search result. It renders exactly the items,
 * counts and scopes the route returned: ordinary transcripts keep their route
 * fields, saved helper threads collapse under their owner, and an owner outside
 * the result becomes an explicit context container rather than a fake row.
 *
 * Counts shown here (ordinary / saved helper threads) come from the same
 * server-side route set the list was built from, so the list and its counts
 * cannot disagree.
 */
export function GroupedLocalSessions({
  payload,
  onRefreshList,
  titles,
  selection,
  emptyState,
}: GroupedLocalSessionsProps) {
  if (payload.items.length === 0) {
    return <>{emptyState ?? null}</>;
  }
  return (
    <div className="flex flex-col" data-grouped-local-sessions="true">
      {payload.items.map((item) => {
        if (item.transcript) {
          return (
            <div key={itemKey(item)} className="border-b border-rule">
              <SessionRow
                row={item.transcript}
                groups={item.helperGroups ?? []}
                titles={titles}
                selection={selection}
                onRefreshList={onRefreshList}
              />
            </div>
          );
        }
        return (
          <div key={itemKey(item)} className="border-b border-rule">
            <HelperGroupListItem ownerStatus={item.context?.ownerStatus}>
              {(item.helperGroups ?? []).map((group) => (
                <MountedHelperGroup
                  key={`${group.groupId}:${group.memberScope}`}
                  groupId={group.groupId}
                  memberScope={group.memberScope}
                  helperThreadCount={group.helperThreadCount}
                  titles={titles}
                  selection={selection}
                  onRefreshList={onRefreshList}
                />
              ))}
            </HelperGroupListItem>
          </div>
        );
      })}
    </div>
  );
}
