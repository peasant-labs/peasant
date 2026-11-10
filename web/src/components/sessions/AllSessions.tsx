'use client';

import { useMemo, useState } from 'react';
import Link from 'next/link';
import { SearchIcon } from 'lucide-react';
import { Input, ProviderName } from '@/lib/ft-ui';
import { closed } from '@peasant-labs/fairtrade/closed-value';
import {
  ColumnAlign,
  TranscriptLibraryTable,
  type TranscriptLibraryColumn,
} from '@peasant-labs/fairtrade/ui';
import { formatTokens } from '@/lib/sessions/columns';
import { displayProject } from '@/lib/quality/utils';
import { parseProjectHash, transcriptHref } from '@/lib/navigation/projectRoutes';
import type { SessionSummary } from '@/types/messages';

/**
 * All Sessions — the flat, cross-project session list under the project picker
 * on `/`, restoring the pre-release archive's Projects page shape (a projects
 * list, then every ingested session beneath it).
 *
 * The caller supplies the complete scope for search. The table mounts at most
 * 100 matching rows; the search affordance reaches rows beyond that limit.
 *
 * Columns are session · project · harness · turns · tokens. The session column
 * is the row identity, so the shared table always shows it.
 *
 * Two columns the archive showed are deliberately ABSENT:
 *   - "Source" / "Status". Those were never real data — the archive's own
 *     columns.tsx defaulted them (`row.source ?? 'imported'`,
 *     `row.status ?? 'local'`), so every row rendered the same constant. The
 *     current SessionSummary carries no such field either, so rendering them
 *     would be three columns of decoration that read as live state.
 *
 * And one column is deliberately RENAMED:
 *   - "Turns", not "user turns". SessionSummary.turnCount counts USER AND
 *     ASSISTANT turns together (the ingest tests pin this: a 1-user/1-assistant
 *     fixture yields turnCount === 2). There is no user-only count in the
 *     pipeline, and turns do not reliably alternate, so it cannot be derived by
 *     halving. The honest header is the total.
 */

const AllSessionColumn = Object.freeze({
  Session: closed('session', 'session'),
  Project: closed('project', 'project'),
  Harness: closed('harness', 'harness'),
  Turns: closed('turns', 'turns'),
  Tokens: closed('tokens', 'tokens'),
} as const);
type AllSessionColumn = (typeof AllSessionColumn)[keyof typeof AllSessionColumn];

/** One grouping member: a complete list renders as one ungrouped body. */
const AllSessionGroupBy = Object.freeze({
  None: closed('none', 'none'),
} as const);
type AllSessionGroupBy = (typeof AllSessionGroupBy)[keyof typeof AllSessionGroupBy];

/** Bound DOM work without truncating the input searched by this adapter. */
const MAX_RENDERED_SESSIONS = 100;

function sessionDate(iso: string): string {
  const ms = Date.parse(iso);
  if (Number.isNaN(ms)) return '—';
  return new Date(ms).toLocaleString(undefined, {
    year: 'numeric',
    month: 'short',
    day: 'numeric',
    hour: 'numeric',
    minute: '2-digit',
  });
}

/** Short, stable handle for a session id — enough to recognise, short enough
 *  to sit beside a title without dominating the cell. */
function shortId(id: string): string {
  return id.length > 8 ? id.slice(0, 8) : id;
}

function rowTitle(session: SessionSummary): string {
  const preview = session.preview?.trim();
  return preview || shortId(session.id);
}

function SessionCell({ session }: { session: SessionSummary }) {
  const projectHash = parseProjectHash(session.projectHash);
  const project = session.project ? displayProject(session.project) : '—';
  const primary = rowTitle(session);
  const label = `${primary}, ${session.harness}, ${session.turnCount} turns, ${session.totalTokens.toLocaleString()} tokens${session.project ? `, ${project}` : ''}`;

  const body = (
    <>
      <span className="min-w-0">
        <span
          className={`block text-[13px] text-ink truncate ${session.preview?.trim() ? '' : 'font-mono'}`}
          title={session.preview?.trim() || session.id}
        >
          {primary}
        </span>
        <span className="block text-[11px] text-ink-4 truncate">{sessionDate(session.startTime)}</span>
      </span>
    </>
  );

  // A session with no project hash has no viewer route to open — render the
  // same row as static text rather than a link that would 404.
  if (!projectHash) {
    return (
      // role="group" so the aria-label is actually exposed: a label on a bare
      // <div> with no role is dropped by assistive tech, which would have made
      // the "not openable" explanation silent.
      <div role="group" className="py-1" aria-label={`${label} (no project recorded, not openable)`}>
        {body}
      </div>
    );
  }

  return (
    <Link
      href={transcriptHref(projectHash, session.id)}
      aria-label={`Open session ${label}`}
      className="block py-1 hover:bg-surface-hover transition-colors focus-mono cursor-pointer"
    >
      {body}
    </Link>
  );
}

export function AllSessions({
  sessions,
  title = 'all sessions',
}: {
  sessions: SessionSummary[];
  /** Heading, so a project-scoped view can name the project it is showing. */
  title?: string;
}) {
  const [query, setQuery] = useState('');

  // Newest first: the most recent work is what you are usually looking for.
  const ordered = useMemo(
    () => [...sessions].sort((a, b) => Date.parse(b.startTime) - Date.parse(a.startTime)),
    [sessions],
  );

  // Match the fields the row actually SHOWS, so a hit is always visible in the
  // result: the short id the cell renders (not the full id, which would match
  // on characters the user cannot see), the preview, the harness, and both the
  // raw and displayed project name.
  const visible = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (!q) return ordered;
    return ordered.filter(
      (s) =>
        (s.preview?.trim() ?? '').toLowerCase().includes(q) ||
        shortId(s.id).toLowerCase().includes(q) ||
        s.harness.toLowerCase().includes(q) ||
        (s.project ?? '').toLowerCase().includes(q) ||
        (s.project ? displayProject(s.project).toLowerCase().includes(q) : false),
    );
  }, [ordered, query]);

  const columns = useMemo<TranscriptLibraryColumn<SessionSummary, AllSessionColumn>[]>(() => [
    {
      id: AllSessionColumn.Session,
      label: 'session',
      align: ColumnAlign.Start,
      render: (session) => <SessionCell session={session} />,
    },
    {
      id: AllSessionColumn.Project,
      label: 'project',
      align: ColumnAlign.Start,
      render: (session) => (
        <span className="min-w-0 text-xs text-ink-3 truncate" title={session.project ?? undefined}>
          {session.project ? displayProject(session.project) : '—'}
        </span>
      ),
    },
    {
      id: AllSessionColumn.Harness,
      label: 'harness',
      align: ColumnAlign.Start,
      render: (session) => (
        <span className="min-w-0 text-[13px] text-ink-2">
          <ProviderName harness={session.harness} />
        </span>
      ),
    },
    {
      id: AllSessionColumn.Turns,
      label: 'turns',
      align: ColumnAlign.End,
      numeric: true,
      render: (session) => session.turnCount.toLocaleString(),
    },
    {
      id: AllSessionColumn.Tokens,
      label: 'tokens',
      align: ColumnAlign.End,
      numeric: true,
      render: (session) => formatTokens(session.totalTokens),
    },
  ], []);

  if (ordered.length === 0) return null;

  return (
    // `w-full max-w-none mx-0 px-0` neutralizes fairtrade's BARE ELEMENT rule
    // `section { max-width: var(--maxw); margin: 0 auto; padding: 0 var(--gutter) }`
    // (base.css), which is meant for its own single-column presentation page.
    // Two things happen to an app <section> without this: it is capped at
    // --maxw (1040px), and — because a cross-axis `margin: auto` CANCELS
    // `align-items: stretch` — it stops filling its flex-column parent and
    // shrinks to its content, then centres. That is why this table rendered
    // narrower AND inset than the project picker beside it, despite sharing a
    // container. fairtrade turns the rule off inside its own shells (.iu,
    // .cmg-page, .gan-root); peasant's home page is in none of them. The rule
    // lives in @layer base, so these utilities (@layer utilities) win.
    <section
      className="flex flex-col gap-2 w-full max-w-none mx-0 px-0"
      data-tour="all-sessions"
    >
      <h2 className="font-[family-name:var(--font-display)] text-lg font-semibold text-ink">
        {title}
      </h2>

      {/* Filter row: search on the left, live count on the right. The count
          reflects the FILTERED set so it doubles as the result readout while
          typing. */}
      <div className="flex items-center justify-between gap-4">
        <div className="w-full max-w-sm">
          <Input
            type="search"
            iconLeft={SearchIcon}
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            aria-label="search sessions"
            placeholder="search sessions…"
          />
        </div>
        <span aria-live="polite" className="shrink-0 font-mono text-xs text-ink-3 tabular-nums">
          {visible.length.toLocaleString()} session{visible.length !== 1 ? 's' : ''}
          {query.trim() && ordered.length !== visible.length
            ? ` of ${ordered.length.toLocaleString()}`
            : ''}
        </span>
      </div>

      {visible.length > MAX_RENDERED_SESSIONS && (
        <p role="status" className="text-xs text-ink-3">
          showing the newest {MAX_RENDERED_SESSIONS} of {visible.length.toLocaleString()} matching sessions; search to narrow the list.
        </p>
      )}

      {visible.length === 0 ? (
        <p className="border border-rule bg-surface px-5 py-6 text-center text-[13px] text-ink-3">
          no sessions match &ldquo;{query.trim()}&rdquo;.
        </p>
      ) : (
        <TranscriptLibraryTable<SessionSummary, AllSessionGroupBy, AllSessionColumn>
          caption={title}
          rows={visible.slice(0, MAX_RENDERED_SESSIONS)}
          rowKey={(session) => session.id}
          columns={columns}
          groupBy={AllSessionGroupBy.None}
          groupByOptions={[AllSessionGroupBy.None]}
          groupKeys={() => []}
          groupLabel={(id) => id}
          nextGroupKeys={null}
          continuedGroupKeys={null}
          page={null}
          viewState={null}
        />
      )}
    </section>
  );
}
