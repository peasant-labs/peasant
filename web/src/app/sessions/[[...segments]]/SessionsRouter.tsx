'use client';

import { useEffect, useMemo, useState } from 'react';
import Link from 'next/link';
import { usePathname, useRouter } from 'next/navigation';
import { useChannel } from '@/contexts/WebSocketContext';
import { Breadcrumbs } from '@/components/Breadcrumbs';
import { AllSessions } from '@/components/sessions/AllSessions';
import { useSessionTitles } from '@/hooks/useSessionTitles';
import { SkeletonList } from '@/lib/skeleton';
import { TeachingEmptyState } from '@/lib/ft-ui';
import { displayProject } from '@/lib/quality/utils';
import { parseProjectHash } from '@/lib/navigation/projectRoutes';
import type { SessionsPayload, SessionSummary } from '@/types/messages';

/**
 * `/sessions/{projectHash}` — every ingested session for one project.
 *
 * This is where a project row on the Home picker leads. It restores the
 * pre-release archive's project-detail behaviour (its ProjectDetailClient
 * filtered the sessions channel by project and rendered the session table)
 * using the same table Home renders, scoped to one project.
 *
 * Its own route rather than `/projects/{hash}`: that path deliberately redirects
 * to the code map, and the map is capability-gated, so on a default server it is
 * not a reachable destination for a project click.
 *
 * The project is read from the PATH, not a query string — `useSearchParams`
 * would force a Suspense boundary under `output: 'export'` (see the /review
 * page), and there is nothing to gain from a query here.
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

      {!error && data !== undefined && scoped.length === 0 && (
        <TeachingEmptyState
          title="no sessions recorded for this project yet"
          body="run the command below in your terminal to scan this computer for ai coding conversations and index what it finds."
          command="peasant ingest"
        />
      )}

      {scoped.length > 0 && (
        <AllSessions
          sessions={scoped}
          titles={sessionTitles}
          title="sessions"
          subtitle="ingested session transcripts for this project."
        />
      )}
    </div>
  );
}
