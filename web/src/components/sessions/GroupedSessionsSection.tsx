'use client';

import { useCallback, useEffect, useState, type ReactNode } from 'react';
import { FeedbackPanel } from '@/lib/ft-ui';
import { SkeletonList } from '@/lib/skeleton';
import {
  assertGroupedProjectScope,
  fetchGroupedLocalSearch,
  fetchGroupedLocalSessions,
  isGroupedProjectScopeError,
  type LocalSessionListPayload,
} from '@/lib/api/grouped';
import { GroupedLocalSessions, type GroupedSelection } from './GroupedLocalSessions';

export interface GroupedSessionsSectionProps {
  /** The originating route: the session list or the full-text search. */
  variant: 'sessions' | 'search';
  /** Search query; required for the search variant. */
  query?: string;
  /**
   * Scope the sessions variant to one project. The project hash is sent as the
   * grouped route's `project` filter, so the server scopes the candidates,
   * the counts and every issued member scope; the section refuses a response
   * that still carries another project's rows rather than rendering a
   * cross-project list under a project heading. Ignored by the search variant.
   */
  projectHash?: string;
  /**
   * Any value that changes when the WebSocket sessions channel delivers an
   * update. The grouped list is a REST read; an existing WS update invalidates
   * it and refetches, so the list never drifts from the live session set.
   */
  invalidationKey?: unknown;
  /** sessionId -> generated title, from useSessionTitles(). */
  titles?: ReadonlyMap<string, string>;
  /** Optional explicit per-member selection. */
  selection?: GroupedSelection;
  /** Heading above the list. */
  heading?: string;
  /** Shown when the route returns no items. */
  emptyState?: ReactNode;
  /**
   * Called when a project-scoped read is refused because the server did not
   * apply the grouped project filter. The section then renders NOTHING so the
   * host can fall back to a project list it can trust; without this callback
   * the refusal is shown as an error instead.
   */
  onScopeUnavailable?: () => void;
}

/**
 * Fetch and mount one grouped local list/search surface.
 *
 * It renders exactly the route's items and counts — ordinary transcripts with
 * their saved helpers collapsed underneath, and explicit context containers for
 * helper-only or owner-excluded results. An expired member scope refreshes THIS
 * list, never a broader one.
 */
export function GroupedSessionsSection({
  variant,
  query,
  projectHash,
  invalidationKey,
  titles,
  selection,
  heading,
  emptyState,
  onScopeUnavailable,
}: GroupedSessionsSectionProps) {
  const [payload, setPayload] = useState<LocalSessionListPayload | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [loading, setLoading] = useState(true);
  const [scopeUnavailable, setScopeUnavailable] = useState(false);
  const [reload, setReload] = useState(0);

  const refresh = useCallback(() => setReload((value) => value + 1), []);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setError(null);
    const request =
      variant === 'search'
        ? fetchGroupedLocalSearch(query ?? '', 20)
        : fetchGroupedLocalSessions({ projectHash });
    request
      .then((next) => {
        if (cancelled) return;
        if (variant === 'sessions' && projectHash) {
          assertGroupedProjectScope(next, projectHash);
        }
        setPayload(next);
        setLoading(false);
      })
      .catch((cause: unknown) => {
        if (cancelled) return;
        if (variant === 'sessions' && projectHash && onScopeUnavailable && isGroupedProjectScopeError(cause)) {
          setPayload(null);
          setError(null);
          setScopeUnavailable(true);
          setLoading(false);
          onScopeUnavailable?.();
          return;
        }
        setPayload(null);
        setError(cause);
        setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [variant, query, projectHash, invalidationKey, reload, onScopeUnavailable]);

  if (scopeUnavailable) return null;

  if (loading && payload === null) {
    return <SkeletonList rows={4} label="Loading grouped sessions" />;
  }

  if (error !== null) {
    return (
      <FeedbackPanel variant="error">
        {error instanceof Error ? error.message : String(error)}
      </FeedbackPanel>
    );
  }

  if (payload === null) return null;

  return (
    <section className="flex flex-col gap-2 w-full max-w-none mx-0 px-0" data-grouped-sessions-section="true">
      {heading && (
        <div className="flex items-baseline justify-between gap-4">
          <h2 className="font-[family-name:var(--font-display)] text-lg font-semibold text-ink">
            {heading}
          </h2>
          <span className="shrink-0 font-mono text-xs text-ink-3 tabular-nums" aria-live="polite">
            {payload.ordinarySessionTotal.toLocaleString()} session
            {payload.ordinarySessionTotal !== 1 ? 's' : ''}
            {payload.helperThreadTotal > 0
              ? ` · ${payload.helperThreadTotal.toLocaleString()} helper thread${payload.helperThreadTotal !== 1 ? 's' : ''}`
              : ''}
          </span>
        </div>
      )}
      <GroupedLocalSessions
        payload={payload}
        onRefreshList={refresh}
        titles={titles}
        selection={selection}
        emptyState={emptyState}
      />
    </section>
  );
}
