'use client';

import { useMemo } from 'react';
import type { useHomeSessions } from '@/hooks/useHomeSessions';
import { SkeletonList } from '@/lib/skeleton';
import type { LocalSyncProjectOption } from '@/lib/api/home';
import { discoveryErrorMessage } from '@/lib/selectionGuidance';
import Link from 'next/link';
import {
  DataTable,
  ProviderIcon,
  PublishStateLabel,
  Segmented,
  Select,
  SessionGroupDisclosure,
  Button,
  type DataTableColumn,
} from '@/lib/ft-ui';
import { usePublicationAudience } from '@/hooks/useRootSessions';
import {
  PUBLISH_FILTERS,
  PUBLISH_FILTER_LABELS,
  isPublishFilter,
  rowPublishState,
  type PublishFilter,
  type RootSessionRow,
} from '@/lib/home/publishState';
import { formatMinutes } from '@/lib/home/rootStats';
import { parseProjectHash, reviewHref, transcriptHref } from '@/lib/navigation/projectRoutes';
import { displayProject } from '@/lib/quality/utils';
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
  const generated = titles.get(id);
  if (generated) return { text: generated, untitled: false };
  const preview = previews.get(id)?.replace(/\s+/g, ' ').trim();
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
  filter: PublishFilter;
  onFilter: (filter: PublishFilter) => void;
  loading?: boolean;
  project: LocalSyncProjectOption | null;
  onProject: (hash: string) => void;
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
 * the server-owned publish-state filters over the same rows, and
 * `load more`. Home owns server filters and pages; page lengths never stand
 * in for global totals or the complete project catalog.
 */
export function RootSessionList({ rows, childGroups = NO_CHILDREN, titles, previews, home, filter, onFilter, loading, project, onProject }: RootSessionListProps) {
  const expandedGroups = home.expanded;
  const visible = rows;

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

  const columns = useMemo<DataTableColumn<ListRow>[]>(
    () => [
      {
        key: 'session',
        label: 'session',
        render: (_, row) => {
          const children = childGroups.get(row.id);
          const hash = parseProjectHash(row.sync.projectHash);
          const title = rowTitle(row.id, titles, previews);
          const harness = row.sync.harness.replace(/-/g, ' ');
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
          if (row.kind === 'child') {
            return (
              <div className="flex min-w-0 flex-col" style={{ paddingLeft: `calc(${row.depth} * (var(--sp-5) + var(--sp-4)))` }}>
                <span id={`session-group-rows:${row.id}`} data-session-group-child={row.id} className="inline-flex min-w-0 items-start gap-2">
                  <ProviderIcon harness={row.sync.harness} accent className="mt-1 shrink-0" />
                  {hash ? (
                    <Link
                      href={transcriptHref(hash, row.id)}
                      className="font-[family-name:var(--font-body)] text-base text-ink underline decoration-dotted decoration-[color:var(--ink-4)] underline-offset-[3px] [overflow-wrap:anywhere] hover:text-[color:var(--amber)] focus-mono"
                    >
                      {title.text}
                    </Link>
                  ) : <span className="font-[family-name:var(--font-body)] text-base text-ink [overflow-wrap:anywhere]">{title.text}</span>}
                </span>
                {group}
              </div>
            );
          }
          return (
            <div className="flex min-w-0 flex-col" data-session-family={row.id}>
              <span className="inline-flex min-w-0 items-start gap-2" data-session-id={row.id}>
                <ProviderIcon harness={row.sync.harness} accent className="mt-1 shrink-0" />
                <span className="flex min-w-0 flex-col gap-0.5">
                  {hash ? (
                    <Link
                      href={transcriptHref(hash, row.id)}
                      className="font-[family-name:var(--font-body)] text-base text-ink underline decoration-dotted decoration-[color:var(--ink-4)] underline-offset-[3px] [overflow-wrap:anywhere] hover:text-[color:var(--amber)] focus-mono"
                    >
                      {title.text}
                    </Link>
                  ) : (
                    <span className="font-[family-name:var(--font-body)] text-base text-ink [overflow-wrap:anywhere]">{title.text}</span>
                  )}
                  <span className="font-mono text-sm text-ink-3">
                    {title.untitled ? 'untitled · ' : ''}
                    {harness} · <span className="tabular-nums">{tokenFormat.format(row.sync.totalTokens)}</span> tokens
                  </span>
                </span>
              </span>
              {group}
            </div>
          );
        },
      },
      {
        key: 'project',
        label: 'project',
        width: '11rem',
        render: (_, row) => {
          const hash = parseProjectHash(row.sync.projectHash);
          const name = displayProject(row.sync.projectName);
          return (
            <span className="flex min-w-0 flex-col gap-0.5">
              <span className="[overflow-wrap:anywhere]">{name}</span>
              {hash && (
                <Link
                  href={reviewHref(hash)}
                  aria-label={`changes in ${name}`}
                  className="w-fit font-mono text-sm text-ink-3 underline decoration-dotted underline-offset-[3px] hover:text-ink focus-mono"
                >
                  changes
                </Link>
              )}
            </span>
          );
        },
      },
      { key: 'turns', label: 'turns', align: 'right', width: '4.5rem', render: (_, row) => row.sync.turnCount.toLocaleString('en-US') },
      {
        key: 'duration',
        label: 'duration',
        align: 'right',
        width: '6rem',
        render: (_, row) => formatMinutes(Number(row.sync.durationMs) / 60_000),
      },
      {
        key: 'when',
        label: 'when',
        width: '6.5rem',
        render: (_, row) => {
          const started = Date.parse(row.sync.startTime);
          return Number.isNaN(started) ? '—' : <span className="whitespace-nowrap">{formatRelative(started)}</span>;
        },
      },
      {
        key: 'state',
        label: 'publish state',
        width: '22rem',
        render: (_, row) => {
          const state = rowPublishState(row);
          const published = state === 'published' || state === 'new-turns';
          return (
            <PublishStateLabel
              state={state}
              collectives={published ? audience.get(row.id) : undefined}
              className="whitespace-nowrap"
            />
          );
        },
      },
    ],
    [titles, previews, audience, childGroups, expandedGroups, home, childCounts],
  );

  const choose = (next: string) => {
    if (!isPublishFilter(next)) return;
    onFilter(next);
  };

  return (
    <section aria-label="sessions" className="mx-0 flex w-full max-w-none flex-col gap-4 px-0" data-root-session-list="true">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <Segmented
          label="filter by publish state"
          value={filter}
          onChange={choose}
          className="max-w-full flex-wrap [&_.bs-seg-opt]:flex-none [&_.bs-seg-opt]:whitespace-nowrap"
          options={PUBLISH_FILTERS.map((option) => ({
            value: option,
            label: (
              <>
                {PUBLISH_FILTER_LABELS[option]}{' '}
              </>
            ),
          }))}
        />
        <Select aria-label="project" value={project?.projectHash ?? ''} onFocus={() => { void home.loadProjects(); }} onChange={event => onProject(event.target.value)} options={[
          { value: '', label: 'all projects' },
          ...(project && !home.projects?.some(option => option.projectHash === project.projectHash) ? [{ value: project.projectHash, label: project.projectName }] : []),
          ...(home.projects ?? []).map(option => ({ value: option.projectHash, label: option.projectName })),
        ]} />
      </div>
      {home.projectsLoading && <p role="status" className="m-0 text-base text-ink-2">loading project choices…</p>}
      {home.projectError != null && <div className="flex items-center gap-2" role="status"><span>{discoveryErrorMessage(home.projectError)}</span><Button variant="secondary" size="sm" onClick={() => { void home.loadProjects(); }}>retry project choices</Button></div>}

      {loading ? <SkeletonList rows={6} label="Loading your sessions" /> : rows.length === 0 ? (
        <p className="border-y border-rule py-6 text-base text-ink-2">{EMPTY_FILTER_COPY[filter]}</p>
      ) : (
        <DataTable<ListRow>
          caption="sessions on this computer"
          // Below the table's natural width the wrapper scrolls sideways
          // instead of crushing every column into a sliver.
          className="[&_.tbl]:min-w-[68rem]"
          columns={columns}
          rows={renderRows}
          rowKey={(row) => row.id}
        />
      )}

      {(home.groups.get('')?.pages.at(-1)?.next ?? '') !== '' && (
        <Button variant="secondary" size="sm" disabled={home.groups.get('')?.loading} onClick={() => home.more()}>load more</Button>
      )}
    </section>
  );
}
