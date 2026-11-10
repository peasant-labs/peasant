'use client';

import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { FeedbackPanel } from '@/lib/ft-ui';
import { SkeletonList } from '@/lib/skeleton';
import { PageSize } from '@peasant-labs/schema';
import { closed } from '@peasant-labs/fairtrade/closed-value';
import {
  ColumnAlign,
  groupKeysFor,
  TranscriptLibraryTable,
  type TranscriptLibraryColumn,
} from '@peasant-labs/fairtrade/ui';
import { displayProject } from '@/lib/quality/utils';
import {
  assertGroupedProjectScope,
  fetchGroupedLocalSearch,
  fetchGroupedLocalSessions,
  isGroupedProjectScopeError,
  type LocalSessionListPayload,
  type LocalSessionListItem,
  type LocalSessionRow,
} from '@/lib/api/grouped';
import { GroupedItemCell, itemKey, itemProjectKeys, type GroupedSelection } from './GroupedSessionRows';

export interface GroupedSessionsSectionProps {
  /** The originating route: the session list or the full-text search. */
  variant: 'sessions' | 'search';
  /** Search query; required for the search variant. */
  query?: string;
  /**
   * Scope the sessions variant to one project. The project hash is sent as the
   * grouped route's `project` filter, so the server scopes the candidates,
   * the counts and every issued member scope; the section refuses a response
   * that still carries another project's rows rather than rendering a
   * cross-project list under a project heading. Ignored by the search variant.
   */
  projectHash?: string;
  /**
   * Any value that changes when the WebSocket sessions channel delivers an
   * update. The grouped list is a REST read; an existing WS update invalidates
   * it and refetches, so the list never drifts from the live session set.
   */
  invalidationKey?: unknown;
  /** Optional explicit per-member selection. */
  selection?: GroupedSelection;
  /** Heading above the list. */
  heading?: string;
  /** Shown when the route returns no items. */
  emptyState?: ReactNode;
  /**
   * Called when a project-scoped read is refused because the server did not
   * apply the grouped project filter. The section then renders NOTHING so the
   * host can fall back to a project list it can trust; without this callback
   * the refusal is shown as an error instead.
   */
  onScopeUnavailable?: () => void;
  /** Optional host content for one session row, such as its publish state or search match. */
  rowDetail?: (row: LocalSessionRow) => ReactNode;
}

/**
 * Fetch and mount one grouped local list/search surface.
 *
 * It renders exactly the route's items and counts — ordinary transcripts with
 * their saved helpers collapsed underneath, and explicit context containers for
 * helper-only or owner-excluded results. An expired member scope refreshes THIS
 * list, never a broader one.
 *
 * The body is the shared table: this adapter keeps the fetch, the project-scope
 * assertion, and the error path, then hands the rows, the bare project keys,
 * and the server page to `TranscriptLibraryTable`.
 */
const GROUPED_PAGE_SIZE = PageSize.P25;

const GroupedColumn = Object.freeze({
  Session: closed('session', 'session'),
  Project: closed('project', 'project'),
  Harness: closed('harness', 'harness'),
  Turns: closed('turns', 'turns'),
  Tokens: closed('tokens', 'tokens'),
} as const);
type GroupedColumn = (typeof GroupedColumn)[keyof typeof GroupedColumn];

const GroupedGroupBy = Object.freeze({
  Project: closed('project', 'project'),
} as const);
type GroupedGroupBy = (typeof GroupedGroupBy)[keyof typeof GroupedGroupBy];

type GroupedRow = { id: string; item: LocalSessionListItem };

function tokenLabel(value: number): string {
  return value.toLocaleString('en-US');
}

export function GroupedSessionsSection({
  variant,
  query,
  projectHash,
  invalidationKey,
  selection,
  heading,
  emptyState,
  onScopeUnavailable,
  rowDetail,
}: GroupedSessionsSectionProps) {
  const [payload, setPayload] = useState<LocalSessionListPayload | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [loading, setLoading] = useState(true);
  const [scopeUnavailable, setScopeUnavailable] = useState(false);
  const [reload, setReload] = useState(0);
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState<PageSize>(GROUPED_PAGE_SIZE);
  // The pages this tab already read, so a continuing group can name the keys
  // that began on the previous page. A cold jump holds no previous page.
  const pages = useRef(new Map<number, LocalSessionListPayload>());

  const refresh = useCallback(() => setReload((value) => value + 1), []);

  // A new route, query, project, or live set starts at page one.
  useEffect(() => {
    setPage(1);
  }, [variant, query, projectHash, invalidationKey]);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setError(null);
    const request =
      variant === 'search'
        ? fetchGroupedLocalSearch(query ?? '', 20, { page, perPage: pageSize })
        : fetchGroupedLocalSessions({ projectHash }, { page, perPage: pageSize });
    request
      .then((next) => {
        if (cancelled) return;
        if (variant === 'sessions' && projectHash) {
          assertGroupedProjectScope(next, projectHash);
        }
        pages.current.set(next.page, next);
        setPayload(next);
        setLoading(false);
      })
      .catch((cause: unknown) => {
        if (cancelled) return;
        if (variant === 'sessions' && projectHash && onScopeUnavailable && isGroupedProjectScopeError(cause)) {
          setPayload(null);
          setError(null);
          setScopeUnavailable(true);
          setLoading(false);
          onScopeUnavailable?.();
          return;
        }
        setPayload(null);
        setError(cause);
        setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [variant, query, projectHash, invalidationKey, reload, page, pageSize, onScopeUnavailable]);

  // A new scope invalidates every page this section held.
  useEffect(() => {
    pages.current.clear();
  }, [variant, query, projectHash]);

  const rows = useMemo<GroupedRow[]>(
    () => (payload?.items ?? []).map((item) => ({ id: itemKey(item), item })),
    [payload],
  );
  const previous = payload === null ? null : pages.current.get(payload.page - 1) ?? null;

  // The label map covers this page, its continuation, and the previous page so a
  // continued group and a next-page group resolve the same project name.
  const projectLabels = useMemo(() => {
    const labels = new Map<string, string>();
    const remember = (item: LocalSessionListItem) => {
      const session = item.transcript?.session;
      if (!session) return;
      const key = `${GroupedGroupBy.Project.wire}:${session.projectHash ?? ''}`;
      if (!labels.has(key)) labels.set(key, session.project ? displayProject(session.project) : '');
    };
    for (const row of payload?.items ?? []) remember(row);
    for (const item of payload?.nextItems ?? []) remember(item);
    for (const item of previous?.items ?? []) remember(item);
    return labels;
  }, [payload, previous]);

  const columns = useMemo<TranscriptLibraryColumn<GroupedRow, GroupedColumn>[]>(() => [
    {
      id: GroupedColumn.Session,
      label: 'session',
      align: ColumnAlign.Start,
      render: (row) => (
        <GroupedItemCell
          item={row.item}
          selection={selection}
          onRefreshList={refresh}
          rowDetail={rowDetail}
        />
      ),
    },
    {
      id: GroupedColumn.Project,
      label: 'project',
      align: ColumnAlign.Start,
      render: (row) => {
        const project = row.item.transcript?.session.project;
        return <span className="home-project">{project ? displayProject(project) : '—'}</span>;
      },
    },
    {
      id: GroupedColumn.Harness,
      label: 'harness',
      align: ColumnAlign.Start,
      render: (row) => row.item.transcript?.session.harness ?? '—',
    },
    {
      id: GroupedColumn.Turns,
      label: 'turns',
      align: ColumnAlign.End,
      numeric: true,
      render: (row) => row.item.transcript ? tokenLabel(row.item.transcript.session.turnCount) : '—',
    },
    {
      id: GroupedColumn.Tokens,
      label: 'tokens',
      align: ColumnAlign.End,
      numeric: true,
      render: (row) => row.item.transcript ? tokenLabel(row.item.transcript.session.totalTokens) : '—',
    },
  ], [selection, refresh, rowDetail]);

  if (scopeUnavailable) return null;

  if (loading && payload === null) {
    return <SkeletonList rows={4} label="Loading grouped sessions" />;
  }

  if (error !== null) {
    return (
      <FeedbackPanel variant="error">
        {error instanceof Error ? error.message : String(error)}
      </FeedbackPanel>
    );
  }

  if (payload === null) return null;

  return (
    <section className="flex flex-col gap-2 w-full max-w-none mx-0 px-0" data-grouped-sessions-section="true">
      {heading && (
        <div className="flex items-baseline justify-between gap-4">
          <h2 className="font-[family-name:var(--font-display)] text-lg font-semibold text-ink">
            {heading}
          </h2>
          <span className="shrink-0 font-mono text-xs text-ink-3 tabular-nums" aria-live="polite">
            {payload.ordinarySessionTotal.toLocaleString()} session
            {payload.ordinarySessionTotal !== 1 ? 's' : ''}
            {payload.helperThreadTotal > 0
              ? ` · ${payload.helperThreadTotal.toLocaleString()} helper thread${payload.helperThreadTotal !== 1 ? 's' : ''}`
              : ''}
          </span>
        </div>
      )}
      {rows.length === 0 ? emptyState ?? null : (
        <TranscriptLibraryTable<GroupedRow, GroupedGroupBy, GroupedColumn>
          caption={heading ?? 'grouped sessions'}
          rows={rows}
          rowKey={(row) => row.id}
          columns={columns}
          groupBy={GroupedGroupBy.Project}
          groupByOptions={[GroupedGroupBy.Project]}
          groupKeys={(row) => itemProjectKeys(row.item)}
          groupLabel={(id) => projectLabels.get(id) ?? (id.slice(id.indexOf(':') + 1) || id)}
          nextGroupKeys={payload.nextItems.length > 0 ? groupKeysFor(payload.nextItems, itemProjectKeys) : null}
          continuedGroupKeys={previous ? groupKeysFor(previous.items, itemProjectKeys) : null}
          page={{ index: payload.page, size: payload.perPage, total: payload.total }}
          viewState={null}
          onPageChange={setPage}
          onPageSizeChange={(size) => { setPageSize(size); setPage(1); }}
        />
      )}
    </section>
  );
}
