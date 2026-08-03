'use client';

import { useEffect, useRef, useState } from 'react';
import { useRouter } from 'next/navigation';
import { Changes, ChangeDetail } from '@peasant-labs/fairtrade/graph';
import { displayProject } from '@/lib/quality/utils';
import { fetchChangeDiff, fetchChangeDetail, fetchReviewChanges } from '@/lib/api/map';
import {
  adaptChangeDetail,
  adaptChangeDiff,
  adaptChanges,
} from '@/lib/adapters/changes';
import type { ReviewListPayload } from '@peasant-labs/schema';
import { mapHref, returnLocation as makeReturnLocation, type ProjectHash, type ReturnLocation } from '@/lib/navigation/projectRoutes';
import { useProjectIdentity } from '@/lib/navigation/useProjectIdentity';
import { discoveryErrorMessage } from '@/lib/selectionGuidance';
import { resolveTimelineNavigation } from './timelineNavigation';

/** A fetch's three observable phases. `error` carries a message for the actionable state. */
type Load<T> =
  | { phase: 'loading' }
  | { phase: 'error'; message: string }
  | { phase: 'ready'; data: T };

type CookedDiff = ReturnType<typeof adaptChangeDiff>;

/**
 * The Changes surface, mounting the LIFTED `<Changes>` / `<ChangeDetail>` from
 * `@peasant-labs/fairtrade/graph` over peasant's `/api/v1/review` data — the thin
 * app-adapter pattern: peasant owns the data layer (project resolution + the REST
 * client) and feeds the cooked payloads in; the package owns all rendering. This
 * supersedes the prior-version review surface (ReviewPageClient + ChangeGraph +
 * ChangeTreemap + ChangeDetail); those files are retained, dormant, as deprecation
 * candidates and are NOT imported here.
 *
 * Route shape (unchanged): `/review/{project}` = the change list;
 * `/review/{project}?branch=<name>` = one change's detail (the branch rides in the
 * query string because branch names contain slashes).
 *
 * Every fetch (the review list, the change detail, and each lazy per-file diff) has an
 * explicit loading / error / ready phase: a 4xx/5xx/network failure renders an ACTIONABLE
 * error state with a retry, never a perpetual spinner.
 */
export function ReviewSurface({
  projectHash,
  projectName,
  branch,
  returnLocation,
  returnTo,
}: {
  projectHash?: ProjectHash | null;
  projectName?: string | null;
  branch: string | null;
  returnLocation?: ReturnLocation | null;
  returnTo?: string | null;
}) {
  const router = useRouter();
  const identity = useProjectIdentity(projectHash ?? projectName ?? null);
  const requestedIdentity = projectHash ?? projectName ?? null;
  const readyIdentity = identity.state.phase === 'ready' && identity.state.requestedIdentity === requestedIdentity
    ? identity.state
    : null;
  const effectiveHash = readyIdentity?.projectHash ?? null;
  const effectiveReturn = returnLocation ?? makeReturnLocation(returnTo ?? '') ?? null;
  const effectiveProject = readyIdentity?.label ?? null;

  // ── the review list (changes + recent commits) ──────────────────────────────
  // The matching ChangeSummary also supplies `filesChanged` to the detail payload
  // (the wire detail omits it), so the detail view waits for this to resolve.
  const [reviewState, setReviewState] = useState<Load<ReviewListPayload>>({ phase: 'loading' });
  const [reviewReload, setReviewReload] = useState(0);
  useEffect(() => {
    if (!effectiveHash) return;
    let alive = true;
    setReviewState({ phase: 'loading' });
    fetchReviewChanges(effectiveHash)
      .then((r) => alive && setReviewState({ phase: 'ready', data: r }))
      .catch((e) => alive && setReviewState({ phase: 'error', message: discoveryErrorMessage(e) }));
    return () => {
      alive = false;
    };
  }, [effectiveHash, reviewReload]);

  // ── the change detail (only in the ?branch= view) ───────────────────────────
  type Detail = Awaited<ReturnType<typeof fetchChangeDetail>>;
  const [detailState, setDetailState] = useState<Load<Detail>>({ phase: 'loading' });
  const [detailReload, setDetailReload] = useState(0);
  useEffect(() => {
    if (!effectiveHash || !branch) return;
    let alive = true;
    setDetailState({ phase: 'loading' });
    fetchChangeDetail(effectiveHash, branch)
      .then((d) => alive && setDetailState({ phase: 'ready', data: d }))
      .catch((e) => alive && setDetailState({ phase: 'error', message: discoveryErrorMessage(e) }));
    return () => {
      alive = false;
    };
  }, [effectiveHash, branch, detailReload]);

  // ── lazy per-file diffs ─────────────────────────────────────────────────────
  // The lifted <ChangeDetail> calls `getDiff(file)` for each OPEN file during render;
  // we return the cached cooked diff (null while it loads), an error sentinel for files
  // whose fetch failed (the surface renders an error, not a spinner — and we do NOT
  // retry them), and fetch any requested-but-unresolved file after render. The cache +
  // error set are BRANCH-SCOPED: they reset on a branch change so a stale diff from a
  // prior branch can never be served.
  const requested = useRef<Set<string>>(new Set());
  const [diffCache, setDiffCache] = useState<Record<string, CookedDiff>>({});
  const [diffErrors, setDiffErrors] = useState<Record<string, string>>({});
  useEffect(() => {
    // branch (or project) changed → drop everything keyed to the old branch.
    requested.current = new Set();
    setDiffCache({});
    setDiffErrors({});
  }, [effectiveHash, branch]);

  useEffect(() => {
    if (!effectiveHash || !branch) return;
    const pending = [...requested.current].filter(
      (p) => !(p in diffCache) && !(p in diffErrors),
    );
    if (pending.length === 0) return;
    let alive = true;
    for (const p of pending) {
      fetchChangeDiff(effectiveHash, branch, p)
        .then((wire) => {
          if (alive) setDiffCache((c) => ({ ...c, [p]: adaptChangeDiff(wire) }));
        })
        .catch((e) => {
          if (alive) setDiffErrors((m) => ({ ...m, [p]: discoveryErrorMessage(e) }));
        });
    }
    return () => {
      alive = false;
    };
  });

  // Project resolution is the trust boundary for every canonical Changes route.
  // Keep this after all hooks so retry/error rendering never changes hook order,
  // and before list/detail rendering so downstream REST state cannot hide a 404
  // or transient identity failure.
  if (requestedIdentity && identity.state.phase === 'resolving') {
    return <ReviewState label="resolving project…" />;
  }
  if (requestedIdentity && (identity.state.phase === 'missing' || identity.state.phase === 'error')) {
    return <ReviewError label={identity.state.message} onRetry={identity.retry} />;
  }

  // ── detail view ─────────────────────────────────────────────────────────────
  if (branch) {
    if (detailState.phase === 'error') {
      return (
        <ReviewError
          label="couldn’t load this change."
          detail={detailState.message}
          onRetry={() => setDetailReload((n) => n + 1)}
        />
      );
    }
    // Hold the render until the detail is ready AND the review list has resolved (ready
    // or error) — so `summary.filesChanged` (the authoritative count) is available and
    // we never flash the capped `detail.files.length` fallback while the list loads.
    if (detailState.phase !== 'ready' || reviewState.phase === 'loading') {
      return <ReviewState label="loading the change…" />;
    }
    const detail = detailState.data;
    const summary =
      reviewState.phase === 'ready'
        ? reviewState.data.changes.find((c) => c.branch === branch)
        : undefined;
    const filesChanged = summary?.filesChanged ?? detail.files.length;
    const payload = adaptChangeDetail(detail, filesChanged);
    const shareSessionIds = [...new Set(payload.work.map((ws) => ws.sessionId))];
    return (
      <ChangeDetail
        payload={payload}
        getDiff={(f) => {
          if (f.path in diffErrors) {
            // error sentinel: a valid (empty) diff shape carrying `error`, which the
            // lifted surface renders as an error row instead of a spinner.
            return {
              branch,
              file: f.path,
              oldPath: f.oldPath ?? null,
              status: f.status,
              binary: false,
              truncated: false,
              hunks: [],
              error: diffErrors[f.path] || 'couldn’t load this file’s diff.',
            };
          }
          requested.current.add(f.path);
          return diffCache[f.path] ?? null;
        }}
        onOpenMap={() =>
          effectiveHash && router.push(effectiveReturn?.href ?? mapHref(effectiveHash))
        }
        onCopyRecap={() => {
          // copy a concise text recap to the clipboard (best-effort; no-op if the
          // clipboard API is unavailable, e.g. a non-secure context).
          void navigator.clipboard?.writeText(buildRecap(detail, filesChanged));
        }}
        onShare={() => {
          router.push(
            shareSessionIds.length > 0
              ? `/share?sessions=${shareSessionIds.map(encodeURIComponent).join(',')}`
              : '/share',
          );
        }}
      />
    );
  }

  // ── list view ──────────────────────────────────────────────────────────────
  if (!effectiveHash || !effectiveProject) return <ReviewState label="pick a project from Home to see its changes." />;
  if (reviewState.phase === 'error') {
    return (
      <ReviewError
        label="couldn’t load this project’s changes."
        detail={reviewState.message}
        onRetry={() => setReviewReload((n) => n + 1)}
      />
    );
  }
  if (reviewState.phase !== 'ready') return <ReviewState label="loading changes…" />;
  const review = reviewState.data;
  return (
    <Changes
      payload={adaptChanges(review)}
      projectLabel={displayProject(effectiveProject)}
      onNavigate={(action) => {
        const command = resolveTimelineNavigation(action, {
          projectHash: effectiveHash,
          defaultBranch: review.defaultBranch,
          returnLocation: effectiveReturn,
          pagination: {
            cursorAvailable: false,
            handlerAvailable: false,
          },
        });
        if (command.kind === 'navigate') {
          router.push(command.href);
        }
      }}
    />
  );
}

/** A concise, copyable recap of one change (used by the share / copy-recap exits). */
function buildRecap(
  detail: Awaited<ReturnType<typeof fetchChangeDetail>>,
  filesChanged: number,
): string {
  return (
    `${detail.branch}: ${filesChanged} files across ${detail.work.length} conversations · ` +
    `git diff ${detail.defaultBranch}...${detail.branch}`
  );
}

/** Minimal state stand-in (loading / empty) for the surface frame. */
function ReviewState({ label }: { label: string }) {
  return (
    <div className="gmp-root gmp-changes-root">
      <div className="gmp-changes-body">
        <p className="gmp-diff-state mono">{label}</p>
      </div>
    </div>
  );
}

/** Actionable error state — a fetch failed; offer a retry instead of an endless spinner. */
function ReviewError({
  label,
  detail,
  onRetry,
}: {
  label: string;
  detail?: string;
  onRetry: () => void;
}) {
  return (
    <div className="gmp-root gmp-changes-root">
      <div className="gmp-changes-body gmp-error-body" role="alert">
        <p className="gmp-diff-state gmp-diff-error mono">{label}</p>
        {detail && <p className="gmp-diff-state mono">{detail}</p>}
        <button type="button" className="btn btn-secondary btn-sm" onClick={onRetry}>
          retry
        </button>
      </div>
    </div>
  );
}
