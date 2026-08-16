'use client';

import { useEffect, useMemo, useState } from 'react';
import Link from 'next/link';
import { SearchIcon } from 'lucide-react';
import { Input, ProviderName } from '@/lib/ft-ui';
import { formatTokens } from '@/lib/sessions/columns';
import { displayProject } from '@/lib/quality/utils';
import { parseProjectHash, transcriptHref } from '@/lib/navigation/projectRoutes';
import type { SessionSummary } from '@/types/messages';

/**
 * All Sessions — the flat, cross-project session list under the project picker
 * on `/`, restoring the pre-release archive's Projects page shape (a projects
 * list, then every ingested session beneath it).
 *
 * Columns are project · session · harness · turns · tokens.
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

/** Rows per page. The full payload can run to thousands of sessions. */
const PAGE_SIZE = 25;

/**
 * Column track sizing, in the reading order project · session · harness · turns
 * · tokens — project leads because it is how you orient in a cross-project list.
 *
 * The text columns take FRACTIONAL widths so the slack spreads across all of
 * them rather than pooling in one, and `session` takes the largest share since
 * it carries the generated title, which is the longest content in the row. The
 * two numeric columns stay fixed: their content has a bounded width, and holding
 * them steady keeps the tabular figures aligned down the page.
 */
const GRID =
  'grid grid-cols-[minmax(0,2fr)_minmax(0,4fr)_minmax(0,1.5fr)_88px_112px] items-center gap-5 px-5';

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

function SessionRow({ session, title }: { session: SessionSummary; title?: string }) {
  const projectHash = parseProjectHash(session.projectHash);
  const project = session.project ? displayProject(session.project) : '—';
  // The generated title is the row's identity when there is one; the short id is
  // the fallback, since the server only stores a title for some sessions.
  const primary = title ?? shortId(session.id);
  const label = `${primary}, ${session.harness}, ${session.turnCount} turns, ${session.totalTokens.toLocaleString()} tokens${session.project ? `, ${project}` : ''}`;

  const cells = (
    <>
      <span className="min-w-0 text-xs text-ink-3 truncate" title={session.project ?? undefined}>
        {project}
      </span>
      <span className="min-w-0">
        <span
          className={`block text-[13px] text-ink truncate ${title ? '' : 'font-mono'}`}
          title={title ?? session.id}
        >
          {primary}
        </span>
        <span className="block text-[11px] text-ink-4 truncate">{sessionDate(session.startTime)}</span>
      </span>
      <span className="min-w-0 text-[13px] text-ink-2">
        <ProviderName harness={session.harness} />
      </span>
      <span className="font-mono text-xs text-ink-3 tabular-nums text-right">
        {session.turnCount.toLocaleString()}
      </span>
      <span className="font-mono text-xs text-ink-3 tabular-nums text-right">
        {formatTokens(session.totalTokens)}
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
      <div
        role="group"
        className={`${GRID} py-3`}
        aria-label={`${label} (no project recorded, not openable)`}
      >
        {cells}
      </div>
    );
  }

  return (
    <Link
      href={transcriptHref(projectHash, session.id)}
      aria-label={`Open session ${label}`}
      className={`${GRID} py-3 hover:bg-surface-hover transition-colors focus-mono cursor-pointer`}
    >
      {cells}
    </Link>
  );
}

export function AllSessions({
  sessions,
  titles,
  title = 'all sessions',
  subtitle = 'ingested session transcripts.',
}: {
  sessions: SessionSummary[];
  /**
   * sessionId → generated title, from useSessionTitles(). Partial by design:
   * rows without an entry fall back to the short session id.
   */
  titles?: ReadonlyMap<string, string>;
  /** Heading, so a project-scoped view can name the project it is showing. */
  title?: string;
  subtitle?: string;
}) {
  const [page, setPage] = useState(0);
  const [query, setQuery] = useState('');

  // Newest first: the most recent work is what you are usually looking for.
  const ordered = useMemo(
    () => [...sessions].sort((a, b) => Date.parse(b.startTime) - Date.parse(a.startTime)),
    [sessions],
  );

  // Match the fields the row actually SHOWS, so a hit is always visible in the
  // result: the short id the cell renders (not the full id, which would match
  // on characters the user cannot see), the harness, and both the raw and
  // displayed project name — typing "peasant" finds it even though the cell
  // shows only the leaf segment.
  const visible = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (!q) return ordered;
    return ordered.filter(
      (s) =>
        (titles?.get(s.id) ?? '').toLowerCase().includes(q) ||
        shortId(s.id).toLowerCase().includes(q) ||
        s.harness.toLowerCase().includes(q) ||
        (s.project ?? '').toLowerCase().includes(q) ||
        (s.project ? displayProject(s.project).toLowerCase().includes(q) : false),
    );
  }, [ordered, query, titles]);

  // A narrowed result usually has fewer pages than the page you were on.
  // Without this the clamp below would silently show the last page of the new
  // result, which reads as "search jumped me somewhere random".
  useEffect(() => {
    setPage(0);
  }, [query]);

  const pageCount = Math.max(1, Math.ceil(visible.length / PAGE_SIZE));
  // Clamp rather than store a corrected page: if the payload shrinks under a
  // live socket update, a stale index must not render an empty table.
  const current = Math.min(page, pageCount - 1);
  const rows = visible.slice(current * PAGE_SIZE, current * PAGE_SIZE + PAGE_SIZE);

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
      <div>
        <h2 className="font-[family-name:var(--font-display)] text-lg font-semibold text-ink">
          {title}
        </h2>
        <p className="text-sm text-ink-3 mt-0.5">{subtitle}</p>
      </div>

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

      {visible.length === 0 ? (
        <p className="border border-rule bg-surface px-5 py-6 text-center text-[13px] text-ink-3">
          no sessions match &ldquo;{query.trim()}&rdquo;.
        </p>
      ) : (
        <div className="border border-rule bg-surface">
          <div className={`${GRID} py-2 border-b border-rule`}>
            <span className="v2-eyebrow">project</span>
            <span className="v2-eyebrow">session</span>
            <span className="v2-eyebrow">harness</span>
            <span className="v2-eyebrow text-right">turns</span>
            <span className="v2-eyebrow text-right">tokens</span>
          </div>

          <div className="divide-y divide-rule">
            {rows.map((session) => (
              <SessionRow key={session.id} session={session} title={titles?.get(session.id)} />
            ))}
          </div>

          {pageCount > 1 && (
            <div className="flex items-center justify-between gap-3 border-t border-rule px-5 py-2">
              <span className="font-mono text-xs text-ink-4 tabular-nums">
                page {current + 1} of {pageCount}
              </span>
              <span className="flex items-center gap-2">
                <button
                  type="button"
                  onClick={() => setPage(current - 1)}
                  disabled={current === 0}
                  className="border border-rule px-3 py-1 font-mono text-xs text-ink-2 hover:bg-surface-hover disabled:opacity-40 disabled:pointer-events-none focus-mono cursor-pointer"
                >
                  previous
                </button>
                <button
                  type="button"
                  onClick={() => setPage(current + 1)}
                  disabled={current >= pageCount - 1}
                  className="border border-rule px-3 py-1 font-mono text-xs text-ink-2 hover:bg-surface-hover disabled:opacity-40 disabled:pointer-events-none focus-mono cursor-pointer"
                >
                  next
                </button>
              </span>
            </div>
          )}
        </div>
      )}
    </section>
  );
}
