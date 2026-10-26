'use client';

import { useEffect } from 'react';
import { usePathname, useRouter, useSearchParams } from 'next/navigation';
import { ReviewSurface } from './ReviewSurface';
import { parseReviewRoute, parseReviewRouteQuery, reviewHref, type ReturnLocation } from '@/lib/navigation/projectRoutes';
import { useProjectIdentity } from '@/lib/navigation/useProjectIdentity';
import { ProjectIdentityProblem } from '@/components/ProjectIdentityProblem';
import { ProjectPickerPage } from '@/components/picker/ProjectPickerPage';

/**
 * Client-side router for the Changes surface. The
 * route stays /review):
 *   /review                          → the project picker, its rows linking to changes.
 *   /review/{project}                → changes list for that project.
 *   /review/{project}?branch=<name>  → change detail. The branch rides in the
 *     query string because branch names contain slashes (feat/graph-cache).
 */
export function ReviewRouter() {
  const pathname = usePathname();
  const searchParams = useSearchParams();

  const route = parseReviewRoute(pathname);
  const query = parseReviewRouteQuery(searchParams ?? '');
  if (!query) return <p role="alert">This changes link contains malformed navigation state. Return Home and open the project again.</p>;
  const { branch, returnLocation } = query;

  if (route.kind === 'canonical') return <ReviewSurface key={`${route.projectHash}:${branch ?? ''}`} projectHash={route.projectHash} branch={branch} returnLocation={returnLocation} />;
  if (route.kind === 'legacy') return <LegacyReviewRoute projectLabel={route.projectLabel} branch={branch} returnLocation={returnLocation} />;
  if (route.kind === 'malformed') return <p role="alert">{route.message}</p>;
  return <ProjectPickerPage title="changes" destination="changes" docs={{ topic: 'changes', label: 'how to read changes' }} />;
}

function LegacyReviewRoute({ projectLabel, branch, returnLocation }: { projectLabel: string; branch: string | null; returnLocation: ReturnLocation | null }) {
  const router = useRouter();
  const { state, retry } = useProjectIdentity(projectLabel);
  useEffect(() => {
    if (state.phase === 'ready' && state.requestedIdentity === projectLabel) router.replace(reviewHref(state.projectHash, { branch: branch ?? undefined, returnLocation: returnLocation ?? undefined }));
  }, [branch, projectLabel, returnLocation, router, state]);
  if (state.phase === 'missing' || state.phase === 'error') return <ProjectIdentityProblem state={state} onRetry={retry} />;
  return <p>resolving project…</p>;
}
