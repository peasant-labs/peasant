'use client';

import { useEffect, useMemo, useRef } from 'react';
import type { useHomeSessions } from '@/hooks/useHomeSessions';
import { SkeletonList } from '@/lib/skeleton';
import { IngestTeach } from '@/components/IngestTeach';
import Link from 'next/link';
import {
  DataTable,
  ProviderName,
  PublishStateLabel,
  SessionGroupDisclosure,
  Button,
  Pagination,
  type DataTableColumn,
} from '@/lib/ft-ui';
import { isPageSize, type PageSize } from '@peasant-labs/schema';
import { HomeGroupBy, type HomeColumnId, type HomeSavedView } from './homeColumns';
import { usePublicationAudience } from '@/hooks/useRootSessions';
import {
  rowPublishState,
  type PublishFilter,
  type RootSessionRow,
} from '@/lib/home/publishState';
import { formatMinutes } from '@/lib/home/rootStats';
import { parseProjectHash, transcriptHref } from '@/lib/navigation/projectRoutes';
import { displayProject } from '@/lib/quality/utils';
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

type SessionListRow = RootSessionRow & { id: string; kind: 'session' | 'child'; depth: number };
type GroupListRow = { id: string; kind: 'group'; groupLabel: string; groupKey: string; open: boolean; depth: 0 };
type ListRow = SessionListRow | GroupListRow;

interface ProjectRowGroup {
  key: string;
  qualified: string;
  label: string;
  rows: RootSessionRow[];
}

/**
 * Group the visible root rows by project in first-appearance order. The key,
 * the qualified id, and the label are derived here ONCE so the row renderer and
 * the collapse set cannot drift on a fallback or prefix.
 */
function groupProjectRows(rows: readonly RootSessionRow[]): ProjectRowGroup[] {
  const order: string[] = [];
  const buckets = new Map<string, RootSessionRow[]>();
  for (const row of rows) {
    const key = row.sync.projectHash || row.sync.projectName;
    const bucket = buckets.get(key);
    if (bucket) bucket.push(row);
    else {
      buckets.set(key, [row]);
      order.push(key);
    }
  }
  return order.map((key) => {
    const grouped = buckets.get(key)!;
    return {
      key,
      qualified: `${HomeGroupBy.Project.wire}:${key}`,
      label: displayProject(grouped[0].sync.projectName),
      rows: grouped,
    };
  });
}

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
  /** The host owns the URL; the table reports the page it wants. */
  onPageChange: (page: number) => void;
  onPageSizeChange: (size: PageSize) => void;
  /** The host owns the device storage; the table reports the collapsed group set. */
  onCollapsedChange?: (collapsed: readonly string[] | null) => void;
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
 * The root page's session list: one row per session with its publish state,
 * paged by the server's offset page. The URL carries the page; the server sends
 * the total, so the pager numbers the whole scope instead of the pages read so
 * far. A request past the last page comes back clamped, and the host rewrites
 * the URL to the page the server served. Page lengths never stand in for global
 * totals or the complete project catalog.
 */
export function RootSessionList({ rows, childGroups = NO_CHILDREN, titles, previews, home, filter, page, pageSize, total, viewState, onPageChange, onPageSizeChange, onCollapsedChange, loading }: RootSessionListProps) {
  const expandedGroups = home.expanded;
  const reading = home.loading;
  const last = Math.max(1, Math.ceil(total / pageSize));
  const current = Math.min(Math.max(1, page), last);
  const visible = rows;
  const start = (current - 1) * pageSize;
  // A new page starts at the table's top.
  const top = useRef<HTMLElement>(null);
  const turned = useRef(current);
  useEffect(() => {
    if (turned.current === current) return;
    turned.current = current;
    const box = top.current?.getBoundingClientRect();
    if (box && box.top < 0) top.current?.scrollIntoView({ block: 'start' });
  }, [current]);

  const projectGroups = useMemo(() => groupProjectRows(visible), [visible]);
  const collapsedGroups = useMemo(() => new Set(viewState.collapsed ?? []), [viewState.collapsed]);

  // Depth-first within one project: every child follows its parent, and the
  // rows are STABLE-PARTITIONED by project before the headers are emitted, so a
  // page ordered A1, B1, A2 renders header A / A1 / A2 / header B / B1 and
  // every header is followed by its own rows.
  const renderRows = useMemo(() => {
    const out: ListRow[] = [];
    const visited = new Set<string>();
    const walk = (row: RootSessionRow, kind: 'session' | 'child', depth: number) => {
      if (visited.has(row.sync.id)) return;
      visited.add(row.sync.id);
      out.push({ ...row, id: row.sync.id, kind, depth });
      if (!expandedGroups.has(row.sync.id)) return;
      for (const child of childGroups.get(row.sync.id) ?? []) walk(child, 'child', depth + 1);
    };
    if (viewState.groupBy !== HomeGroupBy.Project) {
      for (const row of visible) walk(row, 'session', 0);
      return out;
    }
    for (const group of projectGroups) {
      const open = !collapsedGroups.has(group.qualified);
      out.push({
        id: group.qualified,
        kind: 'group',
        groupLabel: group.label,
        groupKey: group.qualified,
        open,
        depth: 0,
      });
      if (!open) continue;
      for (const row of group.rows) walk(row, 'session', 0);
    }
    return out;
  }, [visible, projectGroups, collapsedGroups, childGroups, expandedGroups, viewState.groupBy]);

  const publishedOnScreen = useMemo(
    () => renderRows.filter((row) => row.kind !== 'group' && row.publication.state === 'published').map((row) => row.id),
    [renderRows],
  );
  const audience = usePublicationAudience(publishedOnScreen, rows);
  const childCounts = useMemo(() => {
    const all = [...(home.rootPage?.items ?? []), ...[...home.children.values()].flatMap(group => group.pages.flatMap(page => page.items))];
    return new Map(all.map(row => [row.sync.id, row.childCount]));
  }, [home.rootPage, home.children]);

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

  const columnOrder: readonly HomeColumnId[] = ['session', 'project', 'turns', 'duration', 'when', 'state'];
  const shownKeys = viewState.visibleColumns.length === 0
    ? columnOrder
    : columnOrder.filter(key => viewState.visibleColumns.includes(key));
  const firstShownKey: string = shownKeys[0] ?? 'session';

  const columns = useMemo<DataTableColumn<ListRow>[]>(() => {
    // The header must survive hiding the session column, so it is emitted by
    // whichever column is first on the row, not by the session projection.
    const groupHeader = (columnKey: string, row: GroupListRow) => {
      if (columnKey !== firstShownKey) return null;
      return (
        <div className="home-group-head" data-home-group={row.groupKey}>
          {onCollapsedChange ? (
            <button
              type="button"
              className="home-group-toggle"
              aria-expanded={row.open}
              onClick={() => {
                const current = new Set(viewState.collapsed ?? []);
                if (current.has(row.groupKey)) current.delete(row.groupKey);
                else current.add(row.groupKey);
                onCollapsedChange([...current]);
              }}
            >
              {row.groupLabel}
            </button>
          ) : row.groupLabel}
        </div>
      );
    };
    return [
      {
        key: 'session',
        label: 'session',
        render: (_, row) => {
          if (row.kind === 'group') return groupHeader('session', row);
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
          const when = whenOf(row);
          const meta = (
            // The agent's mark leads the line, so a child row's mark sits under its parent's chevron.
            <span className="home-session-meta">
              <ProviderName harness={row.sync.harness} className="home-session-agent" />,{' '}
              <span className="tabular-nums">{tokenFormat.format(row.sync.totalTokens).toLowerCase()}</span> tokens
              {title.untitled ? ', untitled' : ''}
            </span>
          );
          // A phone shows the session column only, so the other columns' facts
          // follow the title there, above the subagents disclosure.
          const phone = (
            <span className="home-session-phone">
              <span className="home-session-phone-facts">
                {displayProject(row.sync.projectName)}, <span className="tabular-nums">{row.sync.turnCount.toLocaleString('en-US')}</span> turns,{' '}
                <span className="tabular-nums">{formatMinutes(Number(row.sync.durationMs) / 60_000)}</span>{when ? `, ${when}` : ''}
              </span>
              {stateOf(row)}
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
                  {phone}
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
                {phone}
              </span>
              {group}
            </div>
          );
        },
      },
      {
        key: 'project',
        label: 'project',
        render: (_, row) => {
          if (row.kind === 'group') return groupHeader('project', row);
          const name = displayProject(row.sync.projectName);
          return <span className="home-project">{name}</span>;
        },
      },
      { key: 'turns', label: 'turns', align: 'right', render: (_, row) => row.kind === 'group' ? groupHeader('turns', row) : row.sync.turnCount.toLocaleString('en-US') },
      {
        key: 'duration',
        label: 'duration',
        align: 'right',
        render: (_, row) => row.kind === 'group' ? groupHeader('duration', row) : formatMinutes(Number(row.sync.durationMs) / 60_000),
      },
      {
        key: 'when',
        label: 'when',
        render: (_, row) => {
          if (row.kind === 'group') return groupHeader('when', row);
          const when = whenOf(row);
          return when === null ? '—' : <span className="whitespace-nowrap">{when}</span>;
        },
      },
      {
        key: 'state',
        label: 'status',
        render: (_, row) => row.kind === 'group' ? groupHeader('state', row) : stateOf(row),
      },
    ];
  }, [titles, previews, audience, childGroups, expandedGroups, home, childCounts, viewState.visibleColumns, viewState.collapsed, firstShownKey, onCollapsedChange]);
  const shownColumns = useMemo(() => {
    if (viewState.visibleColumns.length === 0) return columns;
    const wanted = new Set<string>(viewState.visibleColumns);
    return columns.filter(column => wanted.has(String(column.key)));
  }, [columns, viewState.visibleColumns]);

  const end = start + visible.length;
  return (
    <section ref={top} aria-label="sessions" aria-busy={reading || undefined} className="home-list" data-root-session-list="true">
      {loading && visible.length === 0 ? <SkeletonList rows={6} label="Loading your sessions" /> : visible.length === 0 && filter === 'all' && total === 0 ? (
        // Nothing recorded at all: say so, and link to the docs that teach recording.
        <IngestTeach />
      ) : visible.length === 0 ? (
        <p className="home-empty">{current > 1 ? 'no sessions on this page.' : EMPTY_FILTER_COPY[filter]}</p>
      ) : (
        <DataTable<ListRow>
          caption="sessions on this computer"
          // Below the table's natural width the wrapper scrolls sideways
          // instead of crushing every column into a sliver; a phone shows the
          // session column alone, its cell carrying the rest (home.css).
          className="home-table"
          columns={shownColumns}
          rows={renderRows}
          rowKey={(row) => row.id}
        />
      )}

      {(visible.length > 0 || last > 1) && (
        <div className="home-pager" data-home-pager="true">
          <span className="home-pager-range tabular-nums" role="status">
            {visible.length > 0 ? `showing ${(start + 1).toLocaleString('en-US')}–${end.toLocaleString('en-US')} of ${total.toLocaleString('en-US')}` : ''}
          </span>
          {last > 1 && <Pagination page={current} total={last} onChange={onPageChange} label="session pages" />}
          <label className="home-page-size">
            <span>per page</span>
            <select
              aria-label="sessions per page"
              value={pageSize}
              onChange={(event) => {
                const next = Number(event.target.value);
                if (isPageSize(next)) onPageSizeChange(next);
              }}
            >
              <option value={25}>25</option>
              <option value={50}>50</option>
              <option value={100}>100</option>
            </select>
          </label>
        </div>
      )}
    </section>
  );
}
