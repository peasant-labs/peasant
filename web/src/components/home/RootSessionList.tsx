'use client';

import { displayedTurnCount } from '@/lib/sessions/displayedTurnCount';
import { useEffect, useMemo, useRef } from 'react';
import type { useHomeSessions } from '@/hooks/useHomeSessions';
import { SkeletonList } from '@/lib/skeleton';
import { IngestTeach } from '@/components/IngestTeach';
import Link from 'next/link';
import {
  Button,
  ProviderName,
  PublishStateLabel,
  SessionGroupDisclosure,
} from '@/lib/ft-ui';
import {
  ColumnAlign,
  groupKeysFor,
  TranscriptLibraryTable,
  type SavedView,
  type TranscriptLibraryColumn,
} from '@peasant-labs/fairtrade/ui';
import type { PageSize } from '@peasant-labs/schema';
import {
  ALL_HOME_GROUP_BYS,
  HomeColumn,
  HomeGroupBy,
  type HomeSavedView,
} from './homeColumns';
import { usePublicationAudience } from '@/hooks/useRootSessions';
import {
  rowPublishState,
  type PublishFilter,
  type RootSessionRow,
} from '@/lib/home/publishState';
import { formatMinutes } from '@/lib/home/rootStats';
import { parseProjectHash, transcriptHref } from '@/lib/navigation/projectRoutes';
import { displayProject } from '@/lib/quality/utils';
import { groupLabelOf, qualifiedGroupId } from '@/lib/table/groupIds';
import { cleanSessionTitle } from '@peasant-labs/fairtrade/ui';
import { formatRelative } from '@/app/review/[[...segments]]/format';

const tokenFormat = new Intl.NumberFormat('en-US', { notation: 'compact', maximumFractionDigits: 1 });

/** Longest preview shown as a row title before it is cut with an ellipsis. */
const PREVIEW_TITLE_LIMIT = 96;

function shortId(id: string): string {
  return id.length > 8 ? id.slice(0, 8) : id;
}

/** The row's title: the generated title, else the quoted first prompt, else the short id. */
function rowTitle(
  id: string,
  titles: ReadonlyMap<string, string>,
  previews: ReadonlyMap<string, string>,
): { text: string; untitled: boolean } {
  const title = titles.get(id);
  const generated = title ? cleanSessionTitle(title) : '';
  if (generated) return { text: generated, untitled: false };
  const prompt = previews.get(id);
  const preview = prompt ? cleanSessionTitle(prompt) : '';
  if (preview) {
    const cut = preview.length > PREVIEW_TITLE_LIMIT ? `${preview.slice(0, PREVIEW_TITLE_LIMIT - 1).trimEnd()}…` : preview;
    return { text: `“${cut}”`, untitled: true };
  }
  return { text: shortId(id), untitled: true };
}

const EMPTY_FILTER_COPY: Readonly<Record<PublishFilter, string>> = {
  all: 'no session to list here.',
  'not-published': 'every session here is published.',
  published: 'nothing here is published yet.',
  auto: 'no session here publishes automatically.',
};

type SessionListRow = RootSessionRow & { id: string; kind: 'session' | 'child'; depth: number; groupKey: string };

const NO_CHILDREN: ReadonlyMap<string, readonly RootSessionRow[]> = new Map();

export interface RootSessionListProps {
  rows: readonly RootSessionRow[];
  childGroups?: ReadonlyMap<string, readonly RootSessionRow[]>;
  /** sessionId -> generated title supplied with the bounded page. */
  titles: ReadonlyMap<string, string>;
  /** sessionId -> first prompt, for sessions without a generated title. */
  previews: ReadonlyMap<string, string>;
  home: ReturnType<typeof useHomeSessions>;
  /** The publish-state filter the page set; it picks the empty copy. */
  filter: PublishFilter;
  /** The one-based page the URL carries. */
  page: number;
  /** The page size the URL carries. */
  pageSize: PageSize;
  /** The one saved view: URL grouping plus device columns and open groups. */
  viewState: HomeSavedView;
  /** The server's total for the current scope. */
  total: number;
  /** The host owns the URL and the device storage; the table reports a change. */
  onViewStateChange: (view: SavedView<HomeGroupBy, HomeColumn>) => void;
  onPageChange: (page: number) => void;
  onPageSizeChange: (size: PageSize) => void;
  loading?: boolean;
}

interface SessionChildrenGroupProps {
  parentId: string;
  childIds: readonly string[];
  expanded: boolean;
  onToggle: () => void;
  indent?: boolean;
  count?: number;
  pendingTarget?: boolean;
}

/** The family disclosure. Its rows are the flattened child rows that follow the parent row. */
function SessionChildrenGroup({ parentId, childIds, expanded, onToggle, indent = false, count, pendingTarget }: SessionChildrenGroupProps) {
  const label = <>subagents <span className="tabular-nums">{(count ?? childIds.length).toLocaleString('en-US')}</span></>;
  // rowsID is an IDREF list: the rendered child rows are the revealed content.
  const rowsID = pendingTarget ? `home-children-${parentId}` : childIds.map((id) => `session-group-rows:${id}`).join(' ');
  return (
    <div data-session-group={parentId}>
      <SessionGroupDisclosure
        label={label}
        collapsedLabel={label}
        expanded={expanded}
        onToggle={onToggle}
        rowsID={rowsID}
        showActionHint={false}
        bare
        indent={indent}
      />
    </div>
  );
}

/**
 * The root page's session list: one shared table row per session with its
 * publish state, paged by the server's offset page.
 *
 * The table owns the group headers, the continuation rows, the numbered pager,
 * the group-by picker, and the column picker. This host supplies the rows, the
 * column renderers, the bare project keys, and the page; it owns the URL
 * grouping and the device columns/open set. The URL carries the page; the
 * server sends the total, so the pager numbers the whole scope instead of the
 * pages read so far. A request past the last page comes back clamped, and the
 * host rewrites the URL to the page the server served.
 */
export function RootSessionList({
  rows,
  childGroups = NO_CHILDREN,
  titles,
  previews,
  home,
  filter,
  page,
  pageSize,
  total,
  viewState,
  onViewStateChange,
  onPageChange,
  onPageSizeChange,
  loading,
}: RootSessionListProps) {
  const expandedGroups = home.expanded;
  const reading = home.loading;
  const last = Math.max(1, Math.ceil(total / pageSize));
  const current = Math.min(Math.max(1, page), last);
  const visible = rows;
  // A new page starts at the table's top.
  const top = useRef<HTMLElement>(null);
  const turned = useRef(current);
  useEffect(() => {
    if (turned.current === current) return;
    turned.current = current;
    const box = top.current?.getBoundingClientRect();
    if (box && box.top < 0) top.current?.scrollIntoView({ block: 'start' });
  }, [current]);

  const grouped = viewState.groupBy === HomeGroupBy.Project;

  /**
   * A row's grouping key. A child row follows its parent's group even when the
   * child was recorded under another project: the family is one unit and the
   * table must not separate a subagent from the session that spawned it.
   */
  const projectKeyOf = (row: RootSessionRow) => row.sync.projectHash || row.sync.projectName || 'unknown';

  // Depth-first within one project: every child follows its parent. The table
  // stable-partitions by group key, so a page ordered A1, B1, A2 renders A1, A2
  // under project A and B1 under project B. No project row is emitted here: the
  // shared table owns every group header and continuation row.
  const renderRows = useMemo(() => {
    const out: SessionListRow[] = [];
    const visited = new Set<string>();
    const walk = (row: RootSessionRow, kind: 'session' | 'child', depth: number, groupKey: string) => {
      if (visited.has(row.sync.id)) return;
      visited.add(row.sync.id);
      out.push({ ...row, id: row.sync.id, kind, depth, groupKey });
      if (!expandedGroups.has(row.sync.id)) return;
      for (const child of childGroups.get(row.sync.id) ?? []) walk(child, 'child', depth + 1, groupKey);
    };
    for (const row of visible) walk(row, 'session', 0, projectKeyOf(row));
    return out;
  }, [visible, childGroups, expandedGroups]);

  const publishedOnScreen = useMemo(
    () => renderRows.filter((row) => row.publication.state === 'published').map((row) => row.id),
    [renderRows],
  );
  const audience = usePublicationAudience(publishedOnScreen, rows);
  const childCounts = useMemo(() => {
    const all = [...(home.rootPage?.items ?? []), ...[...home.children.values()].flatMap(group => group.pages.flatMap(page => page.items))];
    return new Map(all.map(row => [row.sync.id, row.childCount]));
  }, [home.rootPage, home.children]);

  // The label map covers the page, its continuation, and the previous page so a
  // continued group and a next-page group resolve the same project name. The
  // key is the qualified id the table passes to `groupLabel`.
  const projectLabels = useMemo(() => {
    const labels = new Map<string, string>();
    const remember = (row: RootSessionRow) => {
      const key = qualifiedGroupId(HomeGroupBy.Project, projectKeyOf(row));
      if (!labels.has(key)) labels.set(key, displayProject(row.sync.projectName));
    };
    for (const row of visible) remember(row);
    for (const row of home.rootPage?.nextItems ?? []) remember(row);
    for (const row of home.previousPage?.items ?? []) remember(row);
    return labels;
  }, [visible, home.rootPage, home.previousPage]);

  const whenOf = (row: SessionListRow) => {
    const started = Date.parse(row.sync.startTime);
    return Number.isNaN(started) ? null : formatRelative(started);
  };
  const stateOf = (row: SessionListRow) => {
    const state = rowPublishState(row);
    const published = state === 'published' || state === 'new-turns';
    // No approved collective is not an audience to count: "published, 0
    // collectives" read as hidden. The label then names the state alone, as
    // the transcript's publish bar does.
    return <PublishStateLabel state={state} collectives={published ? (audience.get(row.id) || undefined) : undefined} className="whitespace-nowrap" />;
  };

  const columns = useMemo<TranscriptLibraryColumn<SessionListRow, HomeColumn>[]>(() => [
    {
      id: HomeColumn.Session,
      label: 'session',
      align: ColumnAlign.Start,
      render: (row) => {
        const children = childGroups.get(row.id);
        const hash = parseProjectHash(row.sync.projectHash);
        const title = rowTitle(row.id, titles, previews);
        const childCount = childCounts.get(row.id) ?? 0;
        const group = childCount ? (
          <>
            <SessionChildrenGroup parentId={row.id} childIds={(children ?? []).map(child => child.sync.id)} count={childCount} pendingTarget expanded={expandedGroups.has(row.id)} onToggle={() => home.toggle(row.id)} indent />
            <div id={`home-children-${row.id}`} aria-owns={expandedGroups.has(row.id) ? (children ?? []).map(child => `session-group-rows:${child.sync.id}`).join(' ') || undefined : undefined}>
              {expandedGroups.has(row.id) && <div className="flex items-center gap-2" role="status">
                {home.children.get(row.id)?.loading && 'loading subagents…'}
                {home.children.get(row.id)?.error != null && <Button variant="secondary" size="sm" onClick={() => home.retryChild(row.id)}>retry subagents</Button>}
                {(home.children.get(row.id)?.pages.at(-1)?.nextItems.length ?? 0) > 0 && <Button variant="secondary" size="sm" disabled={home.children.get(row.id)?.loading} onClick={() => home.more(row.id)}>load more subagents</Button>}
              </div>}
            </div>
          </>
        ) : null;
        const meta = (
          // The agent's mark leads the line, so a child row's mark sits under its parent's chevron.
          <span className="home-session-meta">
            <ProviderName harness={row.sync.harness} className="home-session-agent" />,{' '}
            <span className="tabular-nums">{tokenFormat.format(row.sync.totalTokens).toLowerCase()}</span> tokens
            {title.untitled ? ', untitled' : ''}
          </span>
        );
        const link = hash ? (
          <Link href={transcriptHref(hash, row.id)} className="home-session-title focus-mono">
            {title.text}
          </Link>
        ) : (
          <span className="home-session-title">{title.text}</span>
        );
        // The recorded teammate identity is evidence the server read from the
        // parent transcript, so it rides under the title as one small line and
        // never replaces the title, agent, token, project, count, or state.
        const teammate = row.sync.teammate ? (
          <span className="home-session-teammate">
            teammate {row.sync.teammate.name} · {row.sync.teammate.team}
          </span>
        ) : null;
        if (row.kind === 'child') {
          return (
            <div className="home-session-cell" style={{ paddingLeft: `calc(${row.depth} * (var(--sp-5) + var(--sp-4)))` }}>
              <span id={`session-group-rows:${row.id}`} data-session-group-child={row.id} className="home-session">
                {link}
                {teammate}
                {meta}
              </span>
              {group}
            </div>
          );
        }
        return (
          <div className="home-session-cell" data-session-family={row.id}>
            <span className="home-session" data-session-id={row.id}>
              {link}
              {teammate}
              {meta}
            </span>
            {group}
          </div>
        );
      },
    },
    {
      id: HomeColumn.Project,
      label: 'project',
      align: ColumnAlign.Start,
      render: (row) => <span className="home-project">{displayProject(row.sync.projectName)}</span>,
    },
    {
      id: HomeColumn.Turns,
      label: 'turns',
      align: ColumnAlign.End,
      numeric: true,
      render: (row) => displayedTurnCount(row.sync).toLocaleString('en-US'),
    },
    {
      id: HomeColumn.Duration,
      label: 'duration',
      align: ColumnAlign.End,
      numeric: true,
      render: (row) => formatMinutes(Number(row.sync.durationMs) / 60_000),
    },
    {
      id: HomeColumn.When,
      label: 'when',
      align: ColumnAlign.Start,
      render: (row) => {
        const when = whenOf(row);
        return when === null ? '—' : <span className="whitespace-nowrap">{when}</span>;
      },
    },
    {
      id: HomeColumn.State,
      label: 'status',
      align: ColumnAlign.Start,
      render: (row) => stateOf(row),
    },
  ], [titles, previews, audience, childGroups, expandedGroups, home, childCounts]);

  const groupKeys = (row: SessionListRow) => grouped ? [row.groupKey] : [];
  const nextGroupKeys = grouped && home.rootPage
    ? groupKeysFor(home.rootPage.nextItems, row => [projectKeyOf(row)])
    : null;
  const continuedGroupKeys = grouped && home.previousPage
    ? groupKeysFor(home.previousPage.items, row => [projectKeyOf(row)])
    : null;

  return (
    <section ref={top} aria-label="sessions" aria-busy={reading || undefined} className="home-list" data-root-session-list="true">
      {loading && visible.length === 0 ? <SkeletonList rows={6} label="Loading your sessions" /> : visible.length === 0 && filter === 'all' && total === 0 ? (
        // Nothing recorded at all: say so, and link to the docs that teach recording.
        <IngestTeach />
      ) : visible.length === 0 ? (
        <p className="home-empty">{current > 1 ? 'no sessions on this page.' : EMPTY_FILTER_COPY[filter]}</p>
      ) : (
        <TranscriptLibraryTable<SessionListRow, HomeGroupBy, HomeColumn>
          caption="sessions on this computer"
          rows={renderRows}
          rowKey={(row) => row.id}
          columns={columns}
          groupBy={viewState.groupBy}
          groupByOptions={ALL_HOME_GROUP_BYS}
          groupKeys={groupKeys}
          groupLabel={(id) => groupLabelOf(id, viewState.groupBy, projectLabels)}
          nextGroupKeys={nextGroupKeys}
          continuedGroupKeys={continuedGroupKeys}
          page={{ index: home.rootPage?.page ?? current, size: home.rootPage?.perPage ?? pageSize, total: home.rootPage?.total ?? total }}
          viewState={viewState}
          onViewStateChange={onViewStateChange}
          onPageChange={onPageChange}
          onPageSizeChange={onPageSizeChange}
        />
      )}
    </section>
  );
}
