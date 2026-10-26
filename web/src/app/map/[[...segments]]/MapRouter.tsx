'use client';

import { useEffect, useState } from 'react';
import { usePathname, useRouter, useSearchParams } from 'next/navigation';
import { Breadcrumbs } from '@/components/Breadcrumbs';
import { FeedbackPanel, Skeleton } from '@/lib/ft-ui';
import { ProjectPickerPage } from '@/components/picker/ProjectPickerPage';
import { MapPageClient } from './MapPageClient';
import { formatMapRouteState, mapHref, parseMapRoute, parseMapRouteState } from '@/lib/navigation/projectRoutes';
import { useProjectIdentity } from '@/lib/navigation/useProjectIdentity';
import { ProjectIdentityProblem } from '@/components/ProjectIdentityProblem';

/**
 * Client-side router for the /map surface:
 *   /map/{project} → the project map.
 *   /map           → the project picker, its rows linking to maps.
 */
export function MapRouter() {
  const pathname = usePathname();
  const searchParams = useSearchParams();
  const [mounted, setMounted] = useState(false);

  useEffect(() => {
    setMounted(true);
  }, []);

  if (!mounted) {
    return <MapRouteSkeleton />;
  }

  const route = parseMapRoute(pathname);
  if (route.kind === 'canonical') return <MapPageClient projectHash={route.projectHash} />;
  if (route.kind === 'legacy') return <LegacyMapRoute projectLabel={route.projectLabel} search={searchParams?.toString() ?? ''} />;
  if (route.kind === 'malformed') return <MalformedMapRoute message={route.message} />;

  return <MapPicker />;
}

function LegacyMapRoute({ projectLabel, search }: { projectLabel: string; search: string }) {
  const router = useRouter();
  const { state, retry } = useProjectIdentity(projectLabel);
  useEffect(() => {
    if (state.phase === 'ready') {
      const canonicalPath = mapHref(state.projectHash);
      const preserved = parseMapRouteState(canonicalPath, search);
      router.replace(preserved ? formatMapRouteState(preserved) : canonicalPath);
    }
  }, [router, search, state]);
  if (state.phase === 'missing' || state.phase === 'error') {
    return <ProjectIdentityProblem state={state} onRetry={retry} />;
  }
  return <MapRouteSkeleton />;
}

function MalformedMapRoute({ message }: { message: string }) {
  return <div role="alert" className="p-6"><FeedbackPanel variant="error">{message}</FeedbackPanel><a href="/map">return to the project picker</a></div>;
}

function MapRouteSkeleton() {
  return (
    <div className="asb-page animate-fade-up">
      <Breadcrumbs items={[{ label: 'home', href: '/' }, { label: 'map' }]} />
      <div className="space-y-2">
        <Skeleton avatar={false} lines={2} label="Loading map header" />
      </div>
      <Skeleton avatar={false} lines={6} label="Loading map" />
    </div>
  );
}

function MapPicker() {
  return <ProjectPickerPage title="map" destination="map" docs={{ topic: 'codeMap', label: 'how to read the code map' }} />;
}
