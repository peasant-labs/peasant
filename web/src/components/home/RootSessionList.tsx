'use client';

import { useMemo, useState } from 'react';
import Link from 'next/link';
import {
  DataTable,
  ProviderIcon,
  PublishStateLabel,
  Segmented,
  Select,
  Button,
  type DataTableColumn,
} from '@/lib/ft-ui';
import { usePublicationAudience } from '@/hooks/useRootSessions';
import {
  PUBLISH_FILTERS,
  PUBLISH_FILTER_LABELS,
  isPublishFilter,
  matchesPublishFilter,
  publishFilterCounts,
  rowPublishState,
  type PublishFilter,
  type RootSessionRow,
} from '@/lib/home/publishState';
import { formatMinutes } from '@/lib/home/rootStats';
import { parseProjectHash, reviewHref, transcriptHref } from '@/lib/navigation/projectRoutes';
import { displayProject } from '@/lib/quality/utils';
import { formatRelative } from '@/app/review/[[...segments]]/format';

/** Rows the list adds each time `load more` is pressed. */
export const ROOT_LIST_PAGE_SIZE = 25;

const ALL_PROJECTS = 'all';

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

interface ListRow extends RootSessionRow {
  id: string;
}

export interface RootSessionListProps {
  rows: readonly RootSessionRow[];
  /** sessionId -> generated title (useSessionTitles). */
  titles: ReadonlyMap<string, string>;
  /** sessionId -> first prompt, for sessions without a generated title. */
  previews: ReadonlyMap<string, string>;
}

/**
 * The root page's session list: one row per session with its publish state,
 * the publish-state filters and a project filter over the same rows, and
 * `load more`. Every filter count is computed from the rows the filter lists.
 */
export function RootSessionList({ rows, titles, previews }: RootSessionListProps) {
  const [filter, setFilter] = useState<PublishFilter>('all');
  const [project, setProject] = useState<string>(ALL_PROJECTS);
  const [shown, setShown] = useState(ROOT_LIST_PAGE_SIZE);

  const projects = useMemo(() => {
    const names = new Map<string, string>();
    for (const row of rows) {
      if (!names.has(row.sync.projectHash)) names.set(row.sync.projectHash, displayProject(row.sync.projectName));
    }
    return [...names.entries()].sort((a, b) => a[1].localeCompare(b[1]));
  }, [rows]);

  // A project that left the rows (a reread, a narrower selection) resets the
  // project filter rather than listing nothing under a stale name.
  const activeProject = project !== ALL_PROJECTS && projects.some(([hash]) => hash === project) ? project : ALL_PROJECTS;
  const inProject = useMemo(
    () => (activeProject === ALL_PROJECTS ? rows : rows.filter((row) => row.sync.projectHash === activeProject)),
    [rows, activeProject],
  );
  const counts = useMemo(() => publishFilterCounts(inProject), [inProject]);
  const listed = useMemo(
    () => inProject.filter((row) => matchesPublishFilter(row, filter)).map((row) => ({ ...row, id: row.sync.id })),
    [inProject, filter],
  );
  const visible = listed.slice(0, shown);

  const publishedOnScreen = useMemo(
    () => visible.filter((row) => row.publication.state === 'published').map((row) => row.id),
    [visible],
  );
  const audience = usePublicationAudience(publishedOnScreen, rows);

  const columns = useMemo<DataTableColumn<ListRow>[]>(
    () => [
      {
        key: 'session',
        label: 'session',
        render: (_, row) => {
          const hash = parseProjectHash(row.sync.projectHash);
          const title = rowTitle(row.id, titles, previews);
          const harness = row.sync.harness.replace(/-/g, ' ');
          return (
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
    [titles, previews, audience],
  );

  const choose = (next: string) => {
    if (!isPublishFilter(next)) return;
    setFilter(next);
    setShown(ROOT_LIST_PAGE_SIZE);
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
                <span className="tabular-nums">{counts[option].toLocaleString('en-US')}</span>
              </>
            ),
          }))}
        />
        {projects.length > 1 && (
          <Select
            aria-label="project"
            value={activeProject}
            onChange={(event) => {
              setProject(event.target.value);
              setShown(ROOT_LIST_PAGE_SIZE);
            }}
            options={[
              { value: ALL_PROJECTS, label: 'all projects' },
              ...projects.map(([hash, name]) => ({ value: hash, label: name })),
            ]}
          />
        )}
      </div>

      {listed.length === 0 ? (
        <p className="border-y border-rule py-6 text-base text-ink-2">{EMPTY_FILTER_COPY[filter]}</p>
      ) : (
        <DataTable<ListRow>
          caption="sessions on this computer"
          // Below the table's natural width the wrapper scrolls sideways
          // instead of crushing every column into a sliver.
          className="[&_.tbl]:min-w-[68rem]"
          columns={columns}
          rows={visible}
          rowKey={(row) => row.id}
        />
      )}

      {listed.length > 0 && (
        <div className="flex items-center justify-between gap-3">
          <span className="font-mono text-sm text-ink-3" aria-live="polite">
            showing <span className="tabular-nums">{visible.length.toLocaleString('en-US')}</span> of{' '}
            <span className="tabular-nums">{listed.length.toLocaleString('en-US')}</span>
          </span>
          {visible.length < listed.length && (
            <Button variant="secondary" size="sm" onClick={() => setShown((value) => value + ROOT_LIST_PAGE_SIZE)}>
              load more
            </Button>
          )}
        </div>
      )}
    </section>
  );
}
