'use client';

import { useEffect, useState } from 'react';
import { usePathname, useRouter, useSearchParams } from 'next/navigation';
import { Skeleton } from '@/lib/ft-ui';
import { SessionDetailV2 } from '@/components/session-detail/v2/SessionDetailV2';
import { formatMapRouteState, mapHref, parseMapRouteState, parseProjectDetailRoute, parseTranscriptRoute, parseTranscriptRouteQuery, transcriptHref, type ProjectHash, type TranscriptHrefOptions, type TranscriptRouteQuery } from '@/lib/navigation/projectRoutes';
import { useProjectIdentity } from '@/lib/navigation/useProjectIdentity';

/**
 * /projects/[name]/[id] is the session viewer — the deep-link target Map and
 * Review drill into while preserving the viewer route.
 *
 * The legacy /projects list and /projects/[name] detail pages are retired:
 * the changes-first Home picker and the Map rail's session list (every
 * session, zero-touch included) replaced them in the Changes/Map/Contribute
 * IA. Old links redirect — list → Home, detail → that project's map.
 */
export function ProjectsRouter() {
  const [mounted, setMounted] = useState(false);
  const pathname = usePathname();
  const router = useRouter();
  const searchParams = useSearchParams();

  useEffect(() => {
    setMounted(true);
  }, []);

  const route = parseTranscriptRoute(pathname);
  const detailRoute = parseProjectDetailRoute(pathname);
  const query = parseTranscriptRouteQuery(searchParams ?? '');
  const isViewer = route.kind === 'canonical' || route.kind === 'legacy';

  useEffect(() => {
    if (!mounted) return;
    if (isViewer || detailRoute.kind === 'legacy') return;
    if (detailRoute.kind === 'canonical') router.replace(canonicalProjectDetailDestination(detailRoute.projectHash, searchParams?.toString() ?? ''));
    else if (route.kind === 'missing') router.replace('/');
  }, [detailRoute, isViewer, mounted, route, router, searchParams]);

  if (!mounted) {
    return <ProjectsRouteSkeleton />;
  }

  if (isViewer && !query) return <p role="alert">This transcript link contains malformed navigation state. Return Home and open the session again.</p>;
  if (route.kind === 'canonical' && query) return <ResolvedProjectViewer projectLabel={route.projectHash} sessionId={route.sessionId} query={query} canonical />;
  if (route.kind === 'legacy' && query) return <LegacyProjectViewer projectLabel={route.projectLabel} sessionId={route.sessionId} query={query} />;
  if (detailRoute.kind === 'legacy') return <LegacyProjectDetail projectLabel={detailRoute.projectLabel} search={searchParams?.toString() ?? ''} />;
  if (route.kind === 'malformed') return <p role="alert">{route.message}</p>;

  // Redirecting (effect above) — show a brief affordance instead of a blank
  // frame, so the interim is never a flash of nothing.
  return (
    <div className="flex h-[40vh] items-center justify-center text-[13px] text-ink-4">
      Taking you Home…
    </div>
  );
}

function canonicalProjectDetailDestination(projectHash: ProjectHash, search: string): string {
  const canonicalPath = mapHref(projectHash);
  const state = parseMapRouteState(canonicalPath, search);
  return state ? formatMapRouteState(state) : canonicalPath;
}

function LegacyProjectDetail({ projectLabel, search }: { projectLabel: string; search: string }) {
  const router = useRouter();
  const { state, retry } = useProjectIdentity(projectLabel);
  useEffect(() => {
    if (state.phase === 'ready' && state.requestedIdentity === projectLabel) router.replace(canonicalProjectDetailDestination(state.projectHash, search));
  }, [router, search, state]);
  if (state.phase === 'missing' || state.phase === 'error') return <button type="button" onClick={retry}>{state.message} retry project resolution</button>;
  return <ProjectsRouteSkeleton />;
}

function transcriptOptions(query: TranscriptRouteQuery): TranscriptHrefOptions {
  return {
    turn: query.turn ?? undefined,
    scope: query.scope ?? undefined,
    scopeVal: query.scopeVal || undefined,
    origin: query.origin ?? undefined,
    originNode: query.originNode ?? undefined,
    originBranch: query.originBranch ?? undefined,
    returnLocation: query.returnLocation ?? undefined,
  };
}

function LegacyProjectViewer({ projectLabel, sessionId, query }: { projectLabel: string; sessionId: string; query: TranscriptRouteQuery }) {
	return <ResolvedProjectViewer projectLabel={projectLabel} sessionId={sessionId} query={query} canonical={false} />;
}

function ResolvedProjectViewer({ projectLabel, sessionId, query, canonical }: { projectLabel: string; sessionId: string; query: TranscriptRouteQuery; canonical: boolean }) {
  const router = useRouter();
  const { state, retry } = useProjectIdentity(projectLabel);
  useEffect(() => {
    if (!canonical && state.phase === 'ready' && state.requestedIdentity === projectLabel) router.replace(transcriptHref(state.projectHash, sessionId, transcriptOptions(query)));
  }, [canonical, projectLabel, query, router, sessionId, state]);
  if (state.phase === 'missing' || state.phase === 'error') return <button type="button" onClick={retry}>{state.message} retry project resolution</button>;
  if (canonical && state.phase === 'ready' && state.requestedIdentity === projectLabel) return <SessionDetailV2 sessionId={sessionId} projectHash={state.projectHash} projectName={state.label} routeQuery={query} />;
  return <ProjectsRouteSkeleton />;
}

function ProjectsRouteSkeleton() {
  return (
    <div className="max-w-[1600px] mx-auto px-6 pt-6 pb-12 flex flex-col gap-4">
      <Skeleton className="h-4 w-44" />
      <Skeleton className="h-[min(72vh,760px)] min-h-[480px]" />
    </div>
  );
}
