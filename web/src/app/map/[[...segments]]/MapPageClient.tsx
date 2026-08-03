'use client';

import { useCallback, useContext, useEffect, useMemo, useState } from 'react';
import { AppRouterContext } from 'next/dist/shared/lib/app-router-context.shared-runtime';
import {
  CodeMapComposition,
  DefaultMapLegend,
  reduceCodeMapState,
  type CodeMapState,
} from '@peasant-labs/fairtrade/graph';
import { Breadcrumbs } from '@/components/Breadcrumbs';
import { CONNECTION } from '@/components/ConnectionState';
import { Term } from '@/components/Term';
import { Explainer, ExplainerToggle, useExplainer } from '@/components/Explainer';
import { useChannel } from '@/contexts/WebSocketContext';
import {
  TimeStrip,
  type TimeStripBranch,
} from '@/components/map';
import {
  fetchMapGraph,
  fetchMapNodeDetail,
  fetchProjectTasks,
  fetchReviewChanges,
  type DecodedMapGraphPayload,
  type DecodedMapNodeDetailPayload,
  type DecodedProjectTasksPayload,
} from '@/lib/api/map';
import { useProjectIdentity } from '@/lib/navigation/useProjectIdentity';
import { mapHref, returnLocation, reviewHref, type ProjectHash } from '@/lib/navigation/projectRoutes';
import type { ReviewListPayload } from '@peasant-labs/schema';
import { subscribe, type SessionsPayload } from '@/types/messages';
import { displayProject } from '@/lib/quality/utils';
import { adaptCodeMap } from '@/lib/adapters/map';
import { Skeleton as SkeletonBox } from '@/lib/skeleton';
import { NodeRail, ProjectRail } from './MapRail';
import {
  commitAtOrBefore,
  coupledNodes,
  localDayEndMs,
  projectCoverage,
  projectSessions,
  sessionsPerDay,
} from '../lib/mapData';
import {
  defaultCodeMapState,
  formatCodeMapLocation,
  parseCodeMapLocation,
} from '../lib/codeMapRouteState';

/**
 * The Map surface: node search, the deterministic
 * canvas + the rail, and the time strip. Node width is always code size;
 * activity edges never render on the canvas — co-edit coupling surfaces as
 * the node panel's "Often edited with" rows instead.
 *
 * Data: ambient liveness (session list, sparkline) comes from the `sessions`
 * WS channel; the graph, node detail, tasks, and branch chips are REST
 * (`@/lib/api/map`), keyed by the project hash. The hash is
 * resolved from `GET /projects/summary` (which carries it per project and
 * loads independently of the `sessions` firehose), falling back to the
 * sessions-channel hash — so the REST surfaces no longer wait on the
 * firehose, and a named project whose sessions lack the hash still maps.
 */

/** Stable subscription instance — the sessions channel (ambient liveness). */
const SESSIONS_SUB = subscribe.sessions();

/** Sparkline window: roughly two months of local days. */
const SPARKLINE_DAYS = 60;

/**
 * SPA navigation that works without an app-router context (vitest/jsdom):
 * Next's `useRouter()` throws when the router is absent (the Map shell's own
 * tests render without a router). Falls back to a full-page navigation when no
 * router is mounted.
 */
function useNavigate(): (href: string) => void {
  const router = useContext(AppRouterContext);
  return useCallback(
    (href: string) => {
      if (router) router.push(href);
      else if (typeof window !== 'undefined') window.location.assign(href);
    },
    [router],
  );
}

/** Square shimmer skeleton over the loading canvas region. */
function CanvasShimmer({ note }: { note?: string }) {
  return (
    <div className="flex h-full flex-col">
      <SkeletonBox className="min-h-0 flex-1" />
      {note && <p className="px-3 py-2 text-xs text-ink-3">{note}</p>}
    </div>
  );
}

/** One-line status note (plain and factual — the caption voice). */
function StatusNote({ children }: { children: React.ReactNode }) {
  return <p className="text-xs text-ink-3">{children}</p>;
}

type FetchState = 'idle' | 'loading' | 'done' | 'error';
/**
 * The map shell — search row, canvas + rail two-column, time strip. Used on
 * the /map/{project} route. The export name and the `projectName` prop are the
 * stable contract; `showLedger` adds the header ledger line on /map/{project}.
 */
export function MapShell({
  projectHash,
  projectName,
  showLedger = false,
}: {
  projectHash: ProjectHash;
  projectName: string;
  showLedger?: boolean;
}) {
  const { data: sessionsData, connected, error: sessionsError } = useChannel<SessionsPayload>(SESSIONS_SUB);
  const navigate = useNavigate();
  const nowMs = useMemo(() => Date.now(), []);
  const explainer = useExplainer('map');

  // -- Ambient data (sessions channel) --------------------------------------
  const sessions = useMemo(() => sessionsData?.sessions ?? [], [sessionsData]);
  const mySessions = useMemo(
    () => projectSessions(sessions, projectHash),
    [sessions, projectHash],
  );

  // -- View state ------------------------------------------------------------
  // Fairtrade owns the complete interaction state. Peasant keeps one value so
  // selection, disclosure, filter, focus, grain, presentation, and viewport
  // are restored atomically from history instead of drifting across parallel
  // host state variables.
  const defaultPresentation = showLedger ? 'navigator' : 'canvas';
  const [mapState, setMapState] = useState<CodeMapState>(() =>
    defaultCodeMapState(defaultPresentation),
  );
  const [urlStateReady, setURLStateReady] = useState(false);
  // Rail hover/focus relay: the hovered task row's edited files —
  // file-grain ids are fine because MapCanvas lifts every highlight id to its
  // nearest visible ancestor node (liftIdsToVisible). Null when nothing hovers.
  const [workHighlightFiles, setWorkHighlightFiles] = useState<readonly string[] | null>(null);
  // Time scrub: the playhead's index into the sparkline `days`, or null
  // for "live"/HEAD. Resolved to a default-branch commit below so the graph
  // can refetch as-of that point in history (`?commit=`).
  const [scrubIndex, setScrubIndex] = useState<number | null>(null);
  // Review payload (open-branch chips + the default-branch commit timeline the
  // scrubber walks). Declared here — above the graph effect — because the
  // scrub→commit resolution it feeds must be in scope before that effect.
  const [review, setReview] = useState<ReviewListPayload | null>(null);

  // Sessions/day sparkline (oldest → newest) and the commit a scrubbed day
  // resolves to. `scrubIndex === null` (live) leaves the commit undefined so
  // the graph renders HEAD.
  const days = useMemo(
    () => sessionsPerDay(mySessions, SPARKLINE_DAYS, nowMs),
    [mySessions, nowMs],
  );
  const scrubCommit = useMemo<string | undefined>(() => {
    if (scrubIndex === null) return undefined;
    const day = days[scrubIndex];
    if (!day) return undefined;
    return commitAtOrBefore(review?.recentCommits ?? [], localDayEndMs(day.date))?.hash;
  }, [scrubIndex, days, review]);

  // Deep link and reversible navigator/canvas state — read once on mount from the location,
  // not useSearchParams(): the shell also renders on `/`, which is statically
  // prerendered without a Suspense boundary.
  useEffect(() => {
    if (typeof window === 'undefined') return;
    const readLocation = () => {
      const locationState = parseCodeMapLocation(
        window.location.pathname,
        window.location.search,
      );
      if (locationState?.projectHash === projectHash) {
        setMapState(locationState.state);
      } else if (window.location.pathname === `/map/${projectHash}`) {
        // A malformed/stale query must not leave the previous history entry's
        // interaction state mounted. Fall back to the route's declared default.
        setMapState(defaultCodeMapState(defaultPresentation));
      }
      setURLStateReady(true);
    };
    readLocation();
    window.addEventListener('popstate', readLocation);
    return () => window.removeEventListener('popstate', readLocation);
  }, [defaultPresentation, projectHash]);

  // Keep the mounted route shareable without creating a navigation entry for
  // every keystroke or tree expansion. Back/forward restores the entry state
  // supplied by an external link; the explicit back control restores browse
  // state within this page.
  useEffect(() => {
    if (!urlStateReady || typeof window === 'undefined') return;
    const href = formatCodeMapLocation(projectHash, mapState);
    const current = `${window.location.pathname}${window.location.search}`;
    if (href !== current) window.history.replaceState(window.history.state, '', href);
  }, [mapState, projectHash, urlStateReady]);

  // -- REST: graph -------------------------------------------------------------
  const [graph, setGraph] = useState<DecodedMapGraphPayload | null>(null);
  const [graphState, setGraphState] = useState<FetchState>('idle');
  useEffect(() => {
    if (!projectHash) return;
    let cancelled = false;
    setGraphState('loading');
    // `scrubCommit` is undefined when live (HEAD) and a default-branch SHA
    // while scrubbed — the graph refetches as-of that commit. The last-good
    // graph stays on screen during the refetch (non-flashing).
    fetchMapGraph(projectHash, scrubCommit)
      .then((payload) => {
        if (cancelled) return;
        setGraph(payload);
        setGraphState('done');
      })
      .catch(() => {
        if (!cancelled) setGraphState('error');
      });
    return () => {
      cancelled = true;
    };
  }, [projectHash, scrubCommit]);

  // -- REST: review (branch chips + commit timeline; best-effort) -------------
  useEffect(() => {
    if (!projectHash) return;
    let cancelled = false;
    fetchReviewChanges(projectHash)
      .then((payload) => {
        if (!cancelled) setReview(payload);
      })
      .catch(() => {
        // Branch chips simply don't render — the strip still works from sessions.
      });
    return () => {
      cancelled = true;
    };
  }, [projectHash]);

  // -- REST: node detail (rail panel) -----------------------------------------
  const [detail, setDetail] = useState<DecodedMapNodeDetailPayload | null>(null);
  const [detailError, setDetailError] = useState<string | null>(null);
  useEffect(() => {
    setDetail(null);
    setDetailError(null);
    if (!projectHash || !mapState.selectedId) return;
    let cancelled = false;
    fetchMapNodeDetail(projectHash, mapState.selectedId)
      .then((payload) => {
        if (!cancelled) setDetail(payload);
      })
      .catch(() => {
        if (!cancelled) setDetailError('Could not load this node’s detail.');
      });
    return () => {
      cancelled = true;
    };
  }, [projectHash, mapState.selectedId]);

  // -- REST: tasks (the rail's "Recent work" block) ---------------------------
  const [tasks, setTasks] = useState<DecodedProjectTasksPayload | null>(null);
  const [tasksState, setTasksState] = useState<FetchState>('idle');
  useEffect(() => {
    if (!projectHash) return;
    let cancelled = false;
    setTasksState('loading');
    fetchProjectTasks(projectHash)
      .then((payload) => {
        if (cancelled) return;
        setTasks(payload);
        setTasksState('done');
      })
      .catch(() => {
        if (!cancelled) setTasksState('error');
      });
    return () => {
      cancelled = true;
    };
  }, [projectHash]);

  // -- Derived view data -------------------------------------------------------
  const codeMapPayload = useMemo(() => (graph ? adaptCodeMap(graph) : null), [graph]);
  const coverage = useMemo(() => (graph ? projectCoverage(graph.nodes) : null), [graph]);
  const branches = useMemo<TimeStripBranch[]>(
    () =>
      (review?.changes ?? [])
        .filter((c) => !c.merged)
        .map((c) => ({ name: c.branch, aheadCount: c.aheadCount })),
    [review],
  );
  // "Often edited with": the wire payload's activityEdges feed the
  // node panel's coupling rows — they no longer render as canvas edges.
  const coupling = useMemo(
    () => (graph && mapState.selectedId ? coupledNodes(graph.activityEdges, mapState.selectedId) : []),
    [graph, mapState.selectedId],
  );

  const handleSelect = useCallback((id: string | null) => {
    setMapState((current) => reduceCodeMapState(
      current,
      id ? { type: 'select', id } : { type: 'clear-selection' },
    ));
    // The rail content swaps on selection (project panel ↔ node panel), so a
    // hovered row unmounts without firing leave/blur — clear its relay here.
    setWorkHighlightFiles(null);
  }, []);

  useEffect(() => {
    // Fairtrade owns navigator and canvas selection. Either surface can replace
    // the host rail while one of its task rows is still relaying a hover.
    setWorkHighlightFiles(null);
  }, [mapState.selectedId]);

  // Rail task rows (Recent work / Shaped by) report their edited files on
  // hover/focus and null on leave/blur, preserving keyboard parity.
  const handleWorkHighlight = useCallback((editedFiles: readonly string[] | null) => {
    setWorkHighlightFiles(editedFiles && editedFiles.length > 0 ? editedFiles : null);
  }, []);

  const highlightedIds = useMemo(() => {
    // The canvas lifts file-grain ids to visible ancestors.
    if (workHighlightFiles) return new Set(workHighlightFiles);
    return undefined;
  }, [workHighlightFiles]);

  const clearSelection = useCallback(() => handleSelect(null), [handleSelect]);

  // Scrub to a day. Scrubbing to the newest day (the rightmost bar) is
  // "live" — it clears the scrub so the graph returns to HEAD.
  const handleScrub = useCallback(
    (index: number) => {
      setScrubIndex(index >= days.length - 1 ? null : index);
      // A past graph is a different structure — drop any selection that may
      // not exist there; the rail returns to the project panel.
      handleSelect(null);
    },
    [days.length, handleSelect],
  );
  const backToNow = useCallback(() => setScrubIndex(null), []);
  const mountedLocation = typeof window === 'undefined'
    ? mapHref(projectHash)
    : `${window.location.pathname}${window.location.search}`;
  const returnTo = showLedger ? returnLocation(mountedLocation) ?? undefined : undefined;

  // The strip only becomes interactive when there is a default-branch commit
  // timeline to walk; otherwise it stays the static v1 sparkline.
  const canScrub = (review?.recentCommits?.length ?? 0) > 0;
  // Scrubbed-day label for the "viewing as of" note (commit-resolved day).
  const scrubDayLabel =
    scrubIndex !== null && days[scrubIndex] ? days[scrubIndex].date : null;

  // -- Canvas region ------------------------------------------------------------
  let canvasBody: React.ReactNode;
  if (graphState === 'error') {
    canvasBody = (
      <div className="flex h-full items-center justify-center px-5">
        <p className="text-[13px] text-danger">Could not load the map graph.</p>
      </div>
    );
  } else if (!codeMapPayload) {
    canvasBody = <CanvasShimmer note="Building the structure map&hellip;" />;
  } else canvasBody = null;

  // -- Rail ----------------------------------------------------------------------
  const rail = mapState.selectedId ? (
    <NodeRail
      projectHash={projectHash}
      projectName={projectName}
      nodeId={mapState.selectedId}
      detail={detail}
      error={detailError}
      sessions={mySessions}
      coupling={coupling}
      onSelectNode={handleSelect}
      onClose={clearSelection}
      onWorkHighlight={handleWorkHighlight}
      nowMs={nowMs}
      returnTo={returnTo}
    />
  ) : (
    <ProjectRail
      projectHash={projectHash}
      projectName={projectName}
      sessions={mySessions}
      coverage={coverage}
      // Only "loading" when a hash exists so a fetch is actually pending —
      // otherwise (cold / no repo) show the plain no-data line, not a shimmer.
      coverageLoading={!!projectHash && (graphState === 'loading' || graphState === 'idle')}
      recentTasks={tasksState === 'done' ? (tasks?.tasks ?? []) : null}
      recentTasksLoading={!!projectHash && (tasksState === 'loading' || tasksState === 'idle')}
      recentTasksError={tasksState === 'error' ? 'Could not load recent work.' : null}
      onWorkHighlight={handleWorkHighlight}
      nowMs={nowMs}
      returnTo={returnTo}
    />
  );

  return (
    <div
      role="region"
      className="flex flex-col gap-4"
      aria-label={`Map of ${displayProject(projectName)}`}
      data-tour="project-map"
    >
      {/* "What am I looking at?" — the toggle sits on its own line and the box
          below spans the content column instead of collapsing inside a flex header. */}
      <ExplainerToggle explainer={explainer} />
      <Explainer explainer={explainer} title="What am I looking at?">
        <p>
          The whole codebase as boxes and lines. Each box is a{' '}
          <Term k="node">code area</Term> (a folder or file); each line is a{' '}
          <Term k="structureEdge">connection</Term>, meaning one area uses another.
        </p>
        <p>
          Bright boxes were <Term k="coverage">built with AI on record</Term>;{' '}
          <Term k="darkMatter">dim ones</Term> predate it. Red marks a{' '}
          <Term k="violation">tangle</Term>. Click a box to see the conversations
          that shaped it, or use the map&rsquo;s <span className="text-ink-2">detail</span>{' '}
          control to open every area down to its folders.
        </p>
      </Explainer>

      {showLedger && mySessions.length > 0 && (
        <p className="text-sm text-ink-3">
          <span className="font-mono tabular-nums">{mySessions.length.toLocaleString()}</span>{' '}
          AI conversation{mySessions.length !== 1 ? 's' : ''}, on your machine. Nothing has left it.
        </p>
      )}

      {/* One-line state notes keep stale or degraded data unambiguous. */}
      {sessionsError ? <StatusNote>Live conversation updates are unavailable. The code map remains available; retry the Peasant connection to refresh the conversation rail.</StatusNote> : null}
      {!sessionsError && !sessionsData ? <StatusNote>{connected ? 'Waiting for live conversation updates. The code map is ready independently.' : CONNECTION.staleNote}</StatusNote> : null}
      {!sessionsError && !connected && sessionsData && <StatusNote>{CONNECTION.staleNote}</StatusNote>}
      {graph && !graph.repoFound && (
        <StatusNote>
          The project path didn&rsquo;t resolve to a git repository. The map is built from
          recorded activity.
        </StatusNote>
      )}
      {graph && graph.repoFound && graph.parsedLanguages.length === 0 && (
        <StatusNote>
          Structure parsing is not yet available for this project&rsquo;s languages. The map
          is built from recorded activity.
        </StatusNote>
      )}

      {/* Canvas + rail — the shared <CodeMapComposition> (full-lift): the
          RailShell frame and the canvas's own grain/search toolbar are ONE
          implementation the fairtrade demo mounts too — not a peasant-only
          reimplementation. `rail` stays host-owned (real API data);
          `canvasSlot` stays host-owned (this route's WS/REST loading-state
          machine — shimmer/error/no-hash/etc. — culminating in the same
          <CodeMap> call the demo's default path renders). The canvas height
          matches the demo's CodeMap height={480} exactly (was a
          viewport-relative h-[min(72vh,760px)] clamp that could reach 760px
          of mostly-empty canvas — pushing the RailShell's rendered height
          far down the page compared to the demo's compact composition).

          legend={false}: the teaching legend renders explicitly BELOW the
          TimeStrip instead (see DefaultMapLegend further down) — this route
          places the scrub timeline directly under the canvas, between it and
          the legend paragraph, rather than the demo's canvas+legend-then-
          timestrip grouping (a peasant-specific ordering call the demo's own
          composition doesn't need, since it has no page-level TimeStrip sibling
          competing for that same "right under the canvas" position). */}
      <CodeMapComposition
        payload={codeMapPayload ?? undefined}
        state={mapState}
        onStateChange={setMapState}
        highlightedIds={highlightedIds}
        height={480}
        ariaLabel={`Code map of ${displayProject(projectName)}`}
        rail={rail}
        sheetTitle={
          mapState.selectedId
            ? (mapState.selectedId.split('/').filter(Boolean).pop() ?? mapState.selectedId)
            : displayProject(projectName)
        }
        canvasSlot={codeMapPayload ? undefined : (
          <div data-testid="map-canvas-region" className="relative h-[480px]">
            {canvasBody}
          </div>
        )}
        legend={false}
      />

      {/* While scrubbed, say which point in history the map shows + a way
          back to now. The graph payload's atCommit confirms the server honored
          the ?commit= (otherwise we silently show HEAD). */}
      {canScrub && scrubDayLabel && (
        <StatusNote>
          Showing the map as it stood on{' '}
          <span className="font-mono tabular-nums">{scrubDayLabel}</span>
          {graph?.atCommit ? (
            <>
              {' '}(commit{' '}
              <span className="font-mono">{graph.atCommit.slice(0, 8)}</span>)
            </>
          ) : null}
          .{' '}
          <button
            type="button"
            onClick={backToNow}
            className="font-mono text-ink hover:underline focus-mono cursor-pointer"
          >
            Back to now
          </button>
        </StatusNote>
      )}

      {/* Time strip: sparkline + open-branch chips. Becomes a scrub control once
          a default-branch commit timeline exists. Sits
          directly under the canvas (above the teaching legend below) — see the
          CodeMapComposition comment above for why this route places it here. */}
      <TimeStrip
        days={days}
        branches={branches}
        defaultBranch={review?.defaultBranch}
        onBranchClick={(branch) => {
          const location = typeof window === 'undefined'
            ? null
            : returnLocation(`${window.location.pathname}${window.location.search}`);
          navigate(reviewHref(projectHash, { branch, returnLocation: location ?? undefined }));
        }}
        scrubbable={canScrub}
        playheadIndex={canScrub ? (scrubIndex ?? days.length - 1) : undefined}
        onScrub={handleScrub}
      />

      {/* The persistent visual-channel teaching legend — the SAME
          <DefaultMapLegend> the demo renders (lifted, not reimplemented),
          just positioned after the TimeStrip on this route instead of
          immediately after the canvas (see the CodeMapComposition
          legend={false} comment above). */}
      {mapState.presentation === 'canvas' ? <DefaultMapLegend /> : null}
    </div>
  );
}

/** The /map/{project} page: container + breadcrumbs + the map shell. */
export function MapPageClient({ projectHash, projectName: requestedProjectName }: { projectHash?: ProjectHash; projectName?: string }) {
  const { state, retry } = useProjectIdentity(projectHash ?? requestedProjectName ?? null);
  if (state.phase === 'resolving') return <CanvasShimmer note="resolving project identity…" />;
  if (state.phase === 'missing' || state.phase === 'error') {
    return <div role="alert" className="p-6"><p>{state.message}</p><button type="button" onClick={retry}>retry project resolution</button></div>;
  }
  const projectName = state.label;
  const cleanName = displayProject(projectName);
  return (
    <div className="max-w-[1600px] mx-auto px-6 pt-6 pb-12 flex flex-col gap-6 animate-fade-up">
      <Breadcrumbs items={[{ label: 'map', href: '/map' }, { label: cleanName }]} />

      <div>
        <h1 className="font-[family-name:var(--font-display)] text-2xl font-semibold tracking-tight text-ink">
          {cleanName}
        </h1>
        <p className="text-sm text-ink-3 mt-1">
          Browse code areas first, then open the spatial map to inspect their relationships.
        </p>
      </div>

      {/* Keyed by project: /map/A → /map/B stays in the same route segment,
          so without the key React would keep the old shell's state (stale
          selection, canvas, fetches) across the switch. */}
      <MapShell key={state.projectHash} projectHash={state.projectHash} projectName={projectName} showLedger />
    </div>
  );
}
