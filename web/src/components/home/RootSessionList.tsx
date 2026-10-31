'use client';

import { useEffect, useMemo, useRef, useState } from 'react';
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

type ListRow = RootSessionRow & { id: string; kind: 'session' | 'child'; depth: number };

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
  /** What the list is scoped to (filter, project); a new scope starts on the first page. */
  scope: string;
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
 * paged. Each page of the table is one page the server sent: the server pages
 * by an opaque continuation and sends no total, so the pager numbers the pages
 * read so far, plus the next one while the server says more follow. Only a
 * press on the pager reads a page, and the table stays on the page it shows
 * until the next one has arrived. Page lengths never stand in for global
 * totals or the complete project catalog, so the footer counts the rows on
 * screen, not the library. A new scope (filter, project) starts on the first page.
 */
export function RootSessionList({ rows, childGroups = NO_CHILDREN, titles, previews, home, filter, scope, loading }: RootSessionListProps) {
  const expandedGroups = home.expanded;
  const root = home.groups.get('');
  const serverPages = root?.pages;
  // A page can come back short or empty and still have more after it, so the
  // pager follows `next`, never a count.
  const hasMore = (serverPages?.at(-1)?.next ?? '') !== '';
  const reading = root?.loading ?? false;
  // A session one page repeats from an earlier page stays on the earlier one.
  const pageRows = useMemo(() => {
    const seen = new Set<string>();
    return (serverPages ?? []).map((served) => served.items.filter((row) => {
      if (seen.has(row.sync.id)) return false;
      seen.add(row.sync.id);
      return true;
    }));
  }, [serverPages]);
  const pageCount = Math.max(1, pageRows.length) + (hasMore && pageRows.length > 0 ? 1 : 0);
  // `page` is the page shown; `wanted` is a page asked for while it is being
  // read, shown once it arrives. A new scope starts over on the first page.
  const [paging, setPaging] = useState<{ scope: string; page: number; wanted: number | null }>({ scope, page: 1, wanted: null });
  if (paging.scope !== scope) setPaging({ scope, page: 1, wanted: null });
  const { page, wanted } = paging;
  // The asked-for page shows in the same render its rows arrive in.
  const shown = wanted !== null && pageRows.length >= wanted ? wanted : page;
  const current = Math.min(Math.max(1, shown), Math.max(1, pageRows.length));
  const visible = pageRows[current - 1] ?? [];
  const start = pageRows.slice(0, current - 1).reduce((sum, items) => sum + items.length, 0);
  useEffect(() => {
    if (wanted === null) return;
    // Rows render before this effect settles the requested page. A newer click
    // or scope change must win over that older request's completion.
    if (pageRows.length >= wanted) setPaging((state) => state.scope === scope && state.wanted === wanted ? { ...state, page: wanted, wanted: null } : state);
    else if (!reading && !hasMore) setPaging((state) => state.scope === scope && state.wanted === wanted ? { ...state, wanted: null } : state);
  }, [scope, wanted, pageRows.length, reading, hasMore]);
  const choose = (next: number) => {
    if (next <= pageRows.length) {
      setPaging((state) => ({ ...state, page: next, wanted: null }));
    } else if (hasMore) {
      setPaging((state) => ({ ...state, wanted: next }));
      home.more();
    }
  };
  // A new page starts at the table's top.
  const top = useRef<HTMLElement>(null);
  const turned = useRef(current);
  useEffect(() => {
    if (turned.current === current) return;
    turned.current = current;
    const box = top.current?.getBoundingClientRect();
    if (box && box.top < 0) top.current?.scrollIntoView({ block: 'start' });
  }, [current]);

  // Depth-first: each expanded child becomes its own row immediately after its
  // parent. Folded groups materialize nothing, so a deep chain stays bounded.
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
    for (const row of visible) walk(row, 'session', 0);
    return out;
  }, [visible, childGroups, expandedGroups]);

  const publishedOnScreen = useMemo(
    () => renderRows.filter((row) => row.publication.state === 'published').map((row) => row.id),
    [renderRows],
  );
  const audience = usePublicationAudience(publishedOnScreen, rows);
  const childCounts = useMemo(() => new Map([...home.groups.values()].flatMap(group => group.pages.flatMap(page => page.items)).map(row => [row.sync.id, row.childCount])), [home.groups]);

  const whenOf = (row: ListRow) => {
    const started = Date.parse(row.sync.startTime);
    return Number.isNaN(started) ? null : formatRelative(started);
  };
  const stateOf = (row: ListRow) => {
    const state = rowPublishState(row);
    const published = state === 'published' || state === 'new-turns';
    // No approved collective is not an audience to count: "published, 0
    // collectives" read as hidden. The label then names the state alone, as
    // the transcript's publish bar does.
    return <PublishStateLabel state={state} collectives={published ? (audience.get(row.id) || undefined) : undefined} className="whitespace-nowrap" />;
  };

  const columns = useMemo<DataTableColumn<ListRow>[]>(
    () => [
      {
        key: 'session',
        label: 'session',
        render: (_, row) => {
          const children = childGroups.get(row.id);
          const hash = parseProjectHash(row.sync.projectHash);
          const title = rowTitle(row.id, titles, previews);
          const childCount = childCounts.get(row.id) ?? 0;
          const group = childCount ? (
            <>
              <SessionChildrenGroup parentId={row.id} childIds={(children ?? []).map(child => child.sync.id)} count={childCount} pendingTarget expanded={expandedGroups.has(row.id)} onToggle={() => home.toggle(row.id)} indent />
              <div id={`home-children-${row.id}`} aria-owns={expandedGroups.has(row.id) ? (children ?? []).map(child => `session-group-rows:${child.sync.id}`).join(' ') || undefined : undefined}>
                {expandedGroups.has(row.id) && <div className="flex items-center gap-2" role="status">
                  {home.groups.get(row.id)?.loading && 'loading subagents…'}
                  {home.groups.get(row.id)?.error != null && <Button variant="secondary" size="sm" onClick={() => home.retry(row.id)}>retry subagents</Button>}
                  {(home.groups.get(row.id)?.pages.at(-1)?.next ?? '') !== '' && <Button variant="secondary" size="sm" disabled={home.groups.get(row.id)?.loading} onClick={() => home.more(row.id)}>load more subagents</Button>}
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
          const name = displayProject(row.sync.projectName);
          return <span className="home-project">{name}</span>;
        },
      },
      { key: 'turns', label: 'turns', align: 'right', render: (_, row) => row.sync.turnCount.toLocaleString('en-US') },
      {
        key: 'duration',
        label: 'duration',
        align: 'right',
        render: (_, row) => formatMinutes(Number(row.sync.durationMs) / 60_000),
      },
      {
        key: 'when',
        label: 'when',
        render: (_, row) => {
          const when = whenOf(row);
          return when === null ? '—' : <span className="whitespace-nowrap">{when}</span>;
        },
      },
      {
        key: 'state',
        label: 'status',
        render: (_, row) => stateOf(row),
      },
    ],
    [titles, previews, audience, childGroups, expandedGroups, home, childCounts],
  );

  const end = start + visible.length;
  return (
    <section ref={top} aria-label="sessions" aria-busy={reading || undefined} className="home-list" data-root-session-list="true">
      {loading ? <SkeletonList rows={6} label="Loading your sessions" /> : visible.length === 0 && filter === 'all' && !hasMore && rows.length === 0 ? (
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
          columns={columns}
          rows={renderRows}
          rowKey={(row) => row.id}
        />
      )}

      {!loading && (visible.length > 0 || pageCount > 1) && (
        <div className="home-pager">
          <span className="home-pager-range tabular-nums" role="status">
            {wanted !== null && reading ? `reading page ${wanted}…` : visible.length > 0 ? `showing ${(start + 1).toLocaleString('en-US')}–${end.toLocaleString('en-US')}` : ''}
          </span>
          {pageCount > 1 && <Pagination page={current} total={pageCount} onChange={choose} label="session pages" />}
        </div>
      )}
    </section>
  );
}
