'use client';

import { useEffect, useState } from 'react';
import { usePathname, useRouter, useSearchParams } from 'next/navigation';
import { Breadcrumbs } from '@/components/Breadcrumbs';
import { IngestTeach } from '@/components/IngestTeach';
import { FeedbackPanel, Skeleton } from '@/lib/ft-ui';
import { ExplainerToggle, useExplainer } from '@/components/Explainer';
import { ProjectPicker, PickerExplainer, rowsFromSummaries } from '@/components/picker/ProjectPicker';
import {
  ProjectListState,
  SelectionRecoveryPanel,
  projectListState,
} from '@/components/picker/SelectionRecoveryPanel';
import {
  cachedProjectSummaries,
  fetchProjectSummaries,
  type DecodedProjectSummariesPayload,
} from '@/lib/api/map';
import { discoveryErrorMessage } from '@/lib/selectionGuidance';
import { MapPageClient } from './MapPageClient';
import { formatMapRouteState, mapHref, parseMapRoute, parseMapRouteState } from '@/lib/navigation/projectRoutes';
import { useProjectIdentity } from '@/lib/navigation/useProjectIdentity';

/**
 * Client-side router for the /map surface:
 *   /map/{project} → the project map.
 *   /map           → map-flavored project picker (rows link to maps; the
 *                    home picker owns the Changes path).
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
    return <div role="alert" className="p-6"><FeedbackPanel variant="error">{state.message}</FeedbackPanel><button type="button" onClick={retry}>retry project resolution</button></div>;
  }
  return <MapRouteSkeleton />;
}

function MalformedMapRoute({ message }: { message: string }) {
  return <div role="alert" className="p-6"><FeedbackPanel variant="error">{message}</FeedbackPanel><a href="/map">return to the project picker</a></div>;
}

function MapRouteSkeleton() {
  return (
    <div className="max-w-[1600px] mx-auto px-6 pt-6 pb-12 flex flex-col gap-6 animate-fade-up">
      <Breadcrumbs items={[{ label: 'map' }]} />
      <div className="space-y-2">
        <Skeleton avatar={false} lines={2} label="Loading map header" />
      </div>
      <Skeleton avatar={false} lines={6} label="Loading map" />
    </div>
  );
}

function MapPicker() {
  // Seed from the module cache so returning to this tab renders the last
  // list instantly; the effect below refreshes it in the background.
  const [summaries, setSummaries] = useState<DecodedProjectSummariesPayload | null>(
    () => cachedProjectSummaries(),
  );
  const [error, setError] = useState<unknown>(null);
  const [reload, setReload] = useState(0);
  // Distinct key from the per-project map's own explainer ("map").
  const explainer = useExplainer('map-picker');

  useEffect(() => {
    let cancelled = false;
    fetchProjectSummaries()
      .then((payload) => {
        if (!cancelled) {
          setError(null);
          setSummaries(payload);
        }
      })
      .catch((err: unknown) => {
        if (!cancelled) {
          setSummaries(null);
          setError(err);
        }
      });
    return () => {
      cancelled = true;
    };
  }, [reload]);

  const listState = summaries === null ? null : projectListState(summaries);
  const selectionRecovery =
    listState === ProjectListState.SelectionRecovery ? summaries?.selection ?? null : null;

  return (
    <div className="max-w-[1600px] mx-auto px-6 pt-6 pb-12 flex flex-col gap-6 animate-fade-up">
      <Breadcrumbs items={[{ label: 'map' }]} />
      <div>
        <div className="flex items-start gap-2">
          <h1 className="font-[family-name:var(--font-display)] text-2xl font-semibold tracking-tight text-ink">
            Map
          </h1>
          <ExplainerToggle explainer={explainer} />
        </div>
        <p className="text-sm text-ink-3 mt-1">
          pick a project to see its code map: how the files connect, and who shaped them.
        </p>
      </div>

      {/* Same on-screen column meaning Home has, so /map isn't tooltip-only. */}
      {summaries !== null && summaries.projects.length > 0 && (
        <PickerExplainer explainer={explainer} destination="map" />
      )}

      {error !== null && (
        <div className="flex flex-col items-start gap-3">
          <FeedbackPanel variant="error">{discoveryErrorMessage(error)}</FeedbackPanel>
          <button
            type="button"
            className="border border-rule px-3 py-2 font-mono text-sm text-ink focus-mono"
            onClick={() => {
              setSummaries(null);
              setError(null);
              setReload((value) => value + 1);
            }}
          >
            retry project discovery
          </button>
        </div>
      )}

      {!error && summaries === null && (
        <Skeleton avatar={false} lines={3} label="Loading projects" />
      )}

      {!error && selectionRecovery && <SelectionRecoveryPanel {...selectionRecovery} />}

      {!error && listState === ProjectListState.NoData && <IngestTeach />}

      {/* The ONE shared picker — same rows/columns as Home, only the row's
          destination differs (map vs changes). */}
      {summaries !== null && summaries.projects.length > 0 && (
        <ProjectPicker rows={rowsFromSummaries(summaries)} destination="map" />
      )}
    </div>
  );
}
