'use client';

/* DEPRECATED — prior-version review surface. This is peasant's original, richer
   Changes UI (this client + ChangeGraph + ChangeTreemap + ChangeDetail + the
   TipCards / WorkSection / structure-impact / signals sub-features). It has been
   SUPERSEDED by the lifted <Changes>/<ChangeDetail> from @peasant-labs/fairtrade/graph,
   mounted via ReviewSurface.tsx — the route no longer renders this. Retained
   DORMANT (not imported by the new /review path) as a deprecation candidate; its
   tests still exercise it. Do not extend; remove once the lifted surface is settled (tracked non-blocking, deprecation candidate). */

import { useEffect, useMemo, useState, type ReactNode } from 'react';
import Link from 'next/link';
import { Map as MapIcon } from 'lucide-react';
import { useChannel } from '@/contexts/WebSocketContext';
import { Breadcrumbs } from '@/components/Breadcrumbs';
import { FeedbackPanel, Skeleton } from '@/lib/ft-ui';
import { Skeleton as SkeletonBox } from '@/lib/skeleton';
import { ConnectionStatus } from '@/components/ConnectionState';
import { displayProject } from '@/lib/quality/utils';
import {
  fetchChangeDetail,
  fetchReviewChanges,
  type DecodedChangeDetailPayload,
} from '@/lib/api/map';
import type { ReviewListPayload } from '@peasant-labs/schema';
import type { SessionsPayload, SessionSummary } from '@/types/messages';
import { mapHref, reviewHref, type ProjectHash } from '@/lib/navigation/projectRoutes';
import { ChangeGraph } from './ChangeGraph';
import { ChangeDetail } from './ChangeDetail';
import { ExplainerToggle, useExplainer, type ExplainerState } from '@/components/Explainer';
import { projectNames, resolveProjectHash } from './sessions';

const CHANNELS: ['sessions'] = ['sessions'];

/**
 * The Changes surface keeps the `/review` route:
 *
 *   - list  (no ?branch=): changes — local branches vs the default branch.
 *   - detail (?branch=):   caption · the work · changed slice · footnotes.
 *
 * Project resolution: an explicit /review/{project} segment wins; otherwise,
 * when the sessions channel shows exactly one project it is used directly.
 * With more than one project this page does not duplicate a picker — Home
 * (`/`) is the project picker, so the no-project state points
 * back there. The display name is resolved to the opaque projectHash from the
 * sessions channel for the REST calls.
 */
export function ReviewPageClient({
  projectName,
  branch,
}: {
  projectName: string | null;
  branch: string | null;
}) {
  const { data, connected } = useChannel<SessionsPayload>(CHANNELS);

  const sessions: SessionSummary[] = useMemo(() => data?.sessions ?? [], [data]);
  const projects = useMemo(() => projectNames(sessions), [sessions]);
  const sessionsReady = data !== undefined;

  // One explainer per active view (list vs detail), its "?" toggle lifted into
  // the page title to match the other surfaces. The content box itself
  // renders at the top of whichever child view is active.
  const explainer = useExplainer(branch ? 'change-detail' : 'changes');

  const effectiveProject =
    projectName ?? (projects.length === 1 ? projects[0] : null);
  const projectHash = useMemo(
    () => (effectiveProject ? resolveProjectHash(sessions, effectiveProject) : null),
    [sessions, effectiveProject],
  );

  const cleanName = effectiveProject ? displayProject(effectiveProject) : null;
  const crumbs = [
    { label: 'changes', href: cleanName || branch ? '/review' : undefined },
    ...(cleanName && effectiveProject
      ? [
          {
            label: cleanName,
            href: branch && projectHash ? reviewHref(projectHash) : undefined,
          },
        ]
      : []),
    ...(branch ? [{ label: branch }] : []),
  ];

  let body: ReactNode;
  if (!sessionsReady) {
    body = branch ? <ChangeDetailSkeleton /> : <ChangeListSkeleton />;
  } else if (!effectiveProject) {
    body = <ChooseFromHome anyProjects={projects.length > 0} />;
  } else if (!projectHash) {
    body = (
      <ErrorPill
        message={`No project hash is recorded for ${displayProject(effectiveProject)} — the server may predate this surface, or the project has not been ingested yet.`}
      />
    );
  } else {
    body = (
      <ReviewData
        projectName={effectiveProject}
        projectHash={projectHash}
        branch={branch}
        explainer={explainer}
      />
    );
  }

  // The "?" sits beside the title only when a graph/detail view is actually
  // rendered (the same condition that produces ReviewData) — never over the
  // skeleton, error, or choose-from-home states.
  const showExplainerToggle = Boolean(sessionsReady && effectiveProject && projectHash);

  return (
    <div className="max-w-[1600px] mx-auto px-6 pt-6 pb-12 flex flex-col gap-6 animate-fade-up">
      <Breadcrumbs items={crumbs} />

      <div className="flex items-start justify-between gap-4">
        <div>
          <div className="flex items-center gap-2">
            <h1 className="font-[family-name:var(--font-display)] text-2xl font-semibold tracking-tight text-ink">
              {branch ?? 'changes'}
            </h1>
            {showExplainerToggle && <ExplainerToggle explainer={explainer} />}
          </div>
          {branch ? (
            <p className="text-sm text-ink-3 mt-1">
              What this line of work changes, and the conversations behind it.
            </p>
          ) : (
            <p className="text-sm text-ink-3 mt-1">
              Each project&rsquo;s lines of work, compared with its main version.
            </p>
          )}
        </div>
        {effectiveProject && projectHash && (
          <Link
            href={mapHref(projectHash)}
            aria-label={`Open the map of ${displayProject(effectiveProject)}`}
            className="shrink-0 border border-rule bg-surface px-3 py-1.5 text-[13px] text-ink hover:bg-surface-hover transition-colors focus-mono cursor-pointer inline-flex items-center gap-1.5"
          >
            <MapIcon size={14} aria-hidden />
            Open the Map
          </Link>
        )}
      </div>

      {/* One disconnected strip (null while connected) — the nav pill is the
          steady-state indicator. Replaces the old stacking strips. */}
      <ConnectionStatus connected={connected} hasData={sessions.length > 0} />

      {body}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Data loader — one REST call per view (list or detail).
// ---------------------------------------------------------------------------

function ReviewData({
  projectName,
  projectHash,
  branch,
  explainer,
}: {
  projectName: string;
  projectHash: ProjectHash;
  branch: string | null;
  /** Shared explainer state — toggle is in the page title, box renders here. */
  explainer: ExplainerState;
}) {
  const [list, setList] = useState<ReviewListPayload | null>(null);
  const [detail, setDetail] = useState<DecodedChangeDetailPayload | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  // Open-branch order, for prev/next navigation while viewing one change (4.2).
  const [navBranches, setNavBranches] = useState<string[]>([]);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setError(null);
    setList(null);
    setDetail(null);

    const request = branch
      ? fetchChangeDetail(projectHash, branch).then((p) => {
          if (!cancelled) setDetail(p);
        })
      : fetchReviewChanges(projectHash).then((p) => {
          if (!cancelled) setList(p);
        });
    request
      .catch((e: unknown) => {
        if (!cancelled) setError(e instanceof Error ? e.message : String(e));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [projectHash, branch]);

  // Best-effort open-branch order for prev/next (only while viewing a change).
  useEffect(() => {
    if (!branch) {
      setNavBranches([]);
      return;
    }
    let cancelled = false;
    fetchReviewChanges(projectHash)
      .then((p) => {
        if (!cancelled) {
          setNavBranches(p.changes.filter((c) => !c.merged).map((c) => c.branch));
        }
      })
      .catch(() => {
        /* nav is optional — the detail still renders without it */
      });
    return () => {
      cancelled = true;
    };
  }, [projectHash, branch]);

  if (error) {
    return <ErrorPill message={`Couldn't load changes: ${error}`} />;
  }
  if (loading) {
    return branch ? <ChangeDetailSkeleton /> : <ChangeListSkeleton />;
  }
  if (branch && detail) {
    return (
      <ChangeDetail
        projectName={projectName}
        projectHash={projectHash}
        branch={branch}
        payload={detail}
        navBranches={navBranches}
        explainer={explainer}
      />
    );
  }
  if (list) {
    // data-tour: the first-run tour's "changes list" step lands here from the
    // home picker (the single-project home embed carries its own anchor).
    return (
      <div data-tour="changes-list">
        <ChangeGraph projectHash={projectHash} projectName={projectName} payload={list} explainer={explainer} />
      </div>
    );
  }
  return null;
}

// ---------------------------------------------------------------------------
// No-project state: Home is the project picker; this page
// points back instead of duplicating one.
// ---------------------------------------------------------------------------

function ChooseFromHome({ anyProjects }: { anyProjects: boolean }) {
  return (
    <div className="border border-rule bg-surface px-5 py-8 text-center">
      <p className="text-sm text-ink-2">
        {anyProjects ? 'Changes are listed per project.' : 'No recorded projects yet.'}
      </p>
      <p className="text-[13px] text-ink-3 mt-1">
        <Link href="/" className="text-ink hover:underline focus-mono">
          Choose a project from Home
        </Link>{' '}
        — each project row opens its changes.
      </p>
    </div>
  );
}

function ErrorPill({ message }: { message: string }) {
  return <FeedbackPanel variant="error">{message}</FeedbackPanel>;
}

// ---------------------------------------------------------------------------
// Skeletons — the shared primitives (one pulse, one fill, square everywhere).
// ---------------------------------------------------------------------------

/** Change-list shimmer for the bordered header and branch rows. */
function ChangeListSkeleton() {
  return <Skeleton avatar={false} lines={4} label="Loading changes" />;
}

/** Story-first change-detail shimmer: caption · slice · the work · footnotes.
 *  A custom granular layout (a sized graph/treemap anchor + caption/work/footnote
 *  bars), kept on the token-only local skeleton — fairtrade's fixed avatar+lines
 *  panel can't express these sized regions without a parity regression. */
function ChangeDetailSkeleton() {
  return (
    <div className="flex flex-col gap-4" aria-busy="true" aria-label="Loading change">
      {/* Caption line */}
      <SkeletonBox className="h-8 w-3/4" />
      {/* Changed slice (leads, visual anchor) */}
      <SkeletonBox className="h-[420px]" />
      {/* The work (below the slice) */}
      <SkeletonBox className="h-[220px]" />
      {/* Footnotes strip */}
      <SkeletonBox className="h-10" />
    </div>
  );
}
