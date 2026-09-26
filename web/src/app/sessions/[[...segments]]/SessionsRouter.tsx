'use client';

import { useCallback, useEffect, useMemo, useState } from 'react';
import Link from 'next/link';
import { usePathname, useRouter } from 'next/navigation';
import { useChannel } from '@/contexts/WebSocketContext';
import { Breadcrumbs } from '@/components/Breadcrumbs';
import { AllSessions } from '@/components/sessions/AllSessions';
import { GroupedSessionsSection } from '@/components/sessions/GroupedSessionsSection';
import { useSessionTitles } from '@/hooks/useSessionTitles';
import { SkeletonList } from '@/lib/skeleton';
import { TeachingEmptyState } from '@/lib/ft-ui';
import { displayProject } from '@/lib/quality/utils';
import { parseProjectHash } from '@/lib/navigation/projectRoutes';
import type { SessionsPayload, SessionSummary } from '@/types/messages';

/**
 * `/sessions/{projectHash}` — every ingested session for one project.
 *
 * This is where a project row on the Home picker leads. It renders the SAME
 * server-grouped list Home renders, scoped to this project: the project hash
 * rides the grouped REST route so the server applies the project predicate to
 * the ordinary rows, the counts and every issued helper-member scope. Sorting
 * and grouping are therefore server-driven and cannot disagree with the counts,
 * and a saved helper expands from an originating scope that already excludes
 * other projects.
 *
 * Its own route rather than `/projects/{hash}`: that path deliberately redirects
 * to the code map, and the map is capability-gated, so on a default server it is
 * not a reachable destination for a project click.
 *
 * The project is read from the PATH, not a query string — `useSearchParams`
 * would force a Suspense boundary under `output: 'export'` (see the /review
 * page), and there is nothing to gain from a query here.
 *
 * The WebSocket sessions channel is NOT the list source: it supplies the
 * project display name for the heading and an invalidation signal, so a live
 * update refetches the grouped REST read instead of re-deriving the list on the
 * client. The flat route stays untouched for its existing consumers.
 */

const CHANNELS: ['sessions'] = ['sessions'];

/** `/sessions/{hash}` → hash, or null when absent/malformed. */
export function parseSessionsRoute(pathname: string): string | null {
  const segments = pathname.split('/').filter(Boolean);
  if (segments[0] !== 'sessions' || segments.length < 2) return null;
  try {
    return decodeURIComponent(segments[1]);
  } catch {
    // decodeURIComponent throws URIError on a stray/short escape ("/sessions/%").
    // A hand-typed or truncated URL must not take the render down; treat it as
    // no project, which routes Home like any other unusable identity.
    return null;
  }
}

export function SessionsRouter() {
  const pathname = usePathname();
  const router = useRouter();
  const [mounted, setMounted] = useState(false);
  // A server that does not yet apply the grouped project filter refuses the
  // scoped read (see assertGroupedProjectScope). Rather than show a broken
  // route, fall back to the flat project list, which is exactly what this
  // route served before the grouped mount. Once the scoped read succeeds, the
  // grouped list is the only body.
  const [groupedScopeUnavailable, setGroupedScopeUnavailable] = useState(false);
  const handleScopeUnavailable = useCallback(() => setGroupedScopeUnavailable(true), []);
  const { data, error } = useChannel<SessionsPayload>(CHANNELS);
  const sessionTitles = useSessionTitles();

  useEffect(() => {
    setMounted(true);
  }, []);

  const raw = parseSessionsRoute(pathname);
  const projectHash = raw ? parseProjectHash(raw) : null;

  // A bare /sessions (or a malformed identity) has no project to show. Send the
  // reader Home rather than rendering an empty frame they cannot act on.
  useEffect(() => {
    if (!mounted) return;
    if (!projectHash) router.replace('/');
  }, [mounted, projectHash, router]);

  const all: SessionSummary[] = useMemo(() => data?.sessions ?? [], [data]);
  const scoped = useMemo(
    () => (projectHash ? all.filter((s) => s.projectHash === projectHash) : []),
    [all, projectHash],
  );

  // The payload carries the project NAME per session, so the heading can name
  // the project without a second request. With no sessions to read a name from
  // it falls back to a SHORT hash — an unabbreviated 64-character hash as an
  // <h1> wraps across lines and reads as breakage, while the short form still
  // identifies the project well enough to act on.
  const projectName = scoped[0]?.project ?? null;
  const label = projectName ? displayProject(projectName) : (raw ? raw.slice(0, 8) : 'project');

  if (!mounted || (!projectHash && !raw)) {
    return (
      <div className="max-w-[1600px] mx-auto px-6 pt-6 pb-12">
        <SkeletonList rows={5} label="Loading sessions" />
      </div>
    );
  }

  if (!projectHash) {
    return (
      <div className="max-w-[1600px] mx-auto px-6 pt-6 pb-12">
        <p role="alert" className="text-base leading-relaxed text-danger">
          This project link has a malformed identity, so no sessions were requested.
          Taking you Home…
        </p>
      </div>
    );
  }

  return (
    <div className="max-w-[1600px] mx-auto px-6 pt-6 pb-12 flex flex-col gap-6 animate-fade-up">
      <Breadcrumbs items={[{ label: 'projects', href: '/' }, { label }]} />

      <div>
        <h1 className="font-[family-name:var(--font-display)] text-2xl font-semibold tracking-tight text-ink">
          {label}
        </h1>
        <p className="text-sm text-ink-3 mt-1">
          every recorded session in this project.{' '}
          <Link href="/" className="underline underline-offset-2 hover:text-ink focus-mono">
            all projects
          </Link>
        </p>
      </div>

      {error && (
        <p role="alert" className="text-base leading-relaxed text-danger">
          Couldn&rsquo;t load sessions: {error}
        </p>
      )}

      {/* No payload yet — skeleton regardless of socket state. Gating this on
          `!connected` too left a CONNECTED-but-still-loading gap where none of
          the three branches rendered and the page went blank under the heading. */}
      {!error && data === undefined && <SkeletonList rows={5} label="Loading sessions" />}

      {/* The project-scoped grouped list. It renders exactly the server's items
          and counts for THIS project; when the project has no sessions the
          section shows the ingest teaching state it owns. A project scope the
          server did not apply is refused (see assertGroupedProjectScope) rather
          than showing another project's sessions, and this route then falls
          back to the flat project list below. */}
      {!error && data !== undefined && !groupedScopeUnavailable && (
        <GroupedSessionsSection
          variant="sessions"
          projectHash={projectHash}
          invalidationKey={data}
          titles={sessionTitles}
          heading="sessions"
          onScopeUnavailable={handleScopeUnavailable}
          emptyState={
            <TeachingEmptyState
              title="no sessions recorded for this project yet"
              body="run the command below in your terminal to scan this computer for ai coding conversations and index what it finds."
              command="peasant ingest"
            />
          }
        />
      )}

      {/* Fallback body for a server that cannot scope the grouped read. This is
          the project list this route served before the grouped mount: the same
          table Home uses, filtered to the project from the sessions channel. */}
      {!error && data !== undefined && groupedScopeUnavailable && (
        scoped.length > 0 ? (
          <AllSessions
            sessions={scoped}
            titles={sessionTitles}
            title="sessions"
            subtitle="ingested session transcripts for this project."
          />
        ) : (
          <TeachingEmptyState
            title="no sessions recorded for this project yet"
            body="run the command below in your terminal to scan this computer for ai coding conversations and index what it finds."
            command="peasant ingest"
          />
        )
      )}
    </div>
  );
}
