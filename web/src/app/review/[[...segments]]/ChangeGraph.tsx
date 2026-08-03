'use client';

/* DEPRECATED — prior-version review surface (the lane git-graph (CommitGraph + TipCards + merged/reverted chips)).
   SUPERSEDED by the lifted <Changes>/<ChangeDetail> from @peasant-labs/fairtrade/graph,
   mounted via ReviewSurface.tsx — the route no longer renders this. Retained DORMANT
   (not imported by the new /review path) as a deprecation candidate; its tests still
   exercise it. Do not extend; remove once the lifted surface is settled (tracked non-blocking, deprecation candidate). */

import { useMemo, useState } from 'react';
import Link from 'next/link';
import { useRouter } from 'next/navigation';
import { TriangleAlert, ArrowRight } from 'lucide-react';
import type { ChangeSummary, ReviewListPayload } from '@peasant-labs/schema';
import { Term } from '@/components/Term';
import { Explainer, ExplainerToggle, useExplainer, type ExplainerState } from '@/components/Explainer';
import { CommitGraph, type Commit } from '@/lib/ft-ui';
import { cn } from '@/lib/utils';
import { reviewHref, type ProjectHash } from '@/lib/navigation/projectRoutes';
import { MINUS, formatRelative, plural, shortHash } from './format';
import { openChangeLifecycle, type LifecycleBadge } from './signals';
import {
  buildChangeGraph,
  type ChangeGraph as Graph,
} from './changeGraphLayout';

/**
 * "feat/map-review-contribute" → "Map review contribute" — a humane lead line
 * for the branch card; the raw name is kept (demoted) in the monospace line.
 */
function humanizeBranch(branch: string): string {
  const last = branch.split('/').pop() ?? branch;
  const words = last.replace(/[-_]+/g, ' ').trim();
  return words ? words.charAt(0).toUpperCase() + words.slice(1) : branch;
}

/**
 * Map buildChangeGraph rows to CommitGraph's Commit[] shape.
 *
 * Lane-0 commits don't need explicit parents — CommitGraph infers the vertical
 * from firstSeen/lastSeen on the lane. Parents are only used for cross-lane
 * elbows:
 *   - Open branch tip: parents=[baseHash] → fork elbow pointing down to the
 *     merge-base (the commit where the branch diverged from lane 0).
 *   - Merged branches: no separate lane (we don't have their individual commits
 *     in the payload). The merge commit on lane 0 gets merged=true; linked
 *     chips live in the "Already merged in" section below the CommitGraph.
 *
 * CommitGraph tip rows show the humanised branch name and the session sparkle
 * for lane context — they do NOT pass the raw branch name as the `branch` prop
 * (which would add a duplicate .cg-branch chip). The raw name already appears
 * in the TipCard section above; duplicating it here confuses text queries.
 *
 * The `id` encodes the branch for navigation: handleSelect reads `commit.id`
 * when `commit.branch` is absent, so the row still routes to the detail page.
 *
 * Gaps vs the old hand-rolled gutter:
 *   - TailBand ("started before this view") is not rendered.
 *   - Merged-branch chips are consolidated in the bottom section (not inline).
 */
function toCommitGraphCommits(graph: Graph): Commit[] {
  return graph.rows.map((row) => {
    if (row.kind === 'tip') {
      const { change, lane } = row;
      // No `branch` field: CommitGraph would render a .cg-branch chip with the
      // raw branch name, duplicating the text already in the TipCard section.
      // Navigation is handled in handleSelect by parsing 'tip-${branch}' from id.
      return {
        id: `tip-${change.branch}`,
        lane,
        parents: change.baseHash ? [change.baseHash] : [],
        message: humanizeBranch(change.branch),
        tip: true,
        session: (change.sessionCount ?? 0) > 0,
        time:
          !row.undated && change.tipCommitMs != null
            ? formatRelative(change.tipCommitMs)
            : undefined,
      };
    }

    const { commit, joins } = row;
    return {
      id: commit.hash,
      lane: 0,
      parents: [],
      message: commit.subject,
      session: commit.hasSession,
      merged: joins.length > 0,
      time:
        commit.timeMs != null ? formatRelative(commit.timeMs) : shortHash(commit.hash),
    };
  });
}

/**
 * The Changes graph: the per-project list shows open lines of work
 * as rich cards first (lifecycle badge, fact fragments, violations), then the
 * ft-ui CommitGraph for the lane/temporal context (session dots, merge markers,
 * branch fork elbows), followed by merged branches in a chip list.
 *
 * The split between TipCards and CommitGraph means no capability is dropped:
 *   - TipCards: "What is this branch about?" — lifecycle, richness, action link
 *   - CommitGraph: "Where does it sit in the timeline?" — lane geometry, dots
 */
export function ChangeGraph({
  projectHash,
  projectName,
  payload,
  explainer: explainerProp,
}: {
  projectHash: ProjectHash;
  projectName: string;
  payload: ReviewListPayload;
  explainer?: ExplainerState;
}) {
  const router = useRouter();
  const [expanded, setExpanded] = useState(false);
  const ownExplainer = useExplainer('changes-embed');
  const explainer = explainerProp ?? ownExplainer;
  const ownsToggle = explainerProp === undefined;
  const nowMs = useMemo(() => Date.now(), []);

  const graph = useMemo(
    () =>
      buildChangeGraph(payload.recentCommits ?? [], payload.changes, {
        maxCommits: expanded ? Number.POSITIVE_INFINITY : undefined,
      }),
    [payload, expanded],
  );

  const commits = useMemo(() => toCommitGraphCommits(graph), [graph]);

  const openChanges = useMemo(
    () => payload.changes.filter((c) => !c.merged),
    [payload.changes],
  );

  const allMerged = useMemo(
    () => payload.changes.filter((c) => c.merged),
    [payload.changes],
  );

  if (!payload.repoFound) {
    return (
      <div className="border border-rule bg-surface px-5 py-8 flex flex-col items-center gap-3">
        <p className="text-sm font-medium text-ink">
          This folder doesn&rsquo;t keep a change history we can read.
        </p>
        <p className="text-[13px] text-ink-3 text-center max-w-md leading-relaxed">
          It isn&rsquo;t version-controlled, so there are no separate lines of work
          to show. Your recorded AI conversations are still available — use{' '}
          <span className="text-ink">Open the Map</span> above.
        </p>
      </div>
    );
  }

  if (payload.changes.length === 0) {
    return (
      <div className="border border-rule bg-surface px-5 py-8 text-center">
        <p className="text-sm text-ink-2 max-w-lg mx-auto leading-relaxed">
          No separate lines of work right now — every update went straight to the
          project&rsquo;s main line (
          <span className="font-mono text-[13px]">
            {payload.defaultBranch ?? 'develop'}
          </span>
          ). Open the Map to see that activity over time.
        </p>
      </div>
    );
  }

  // Tip rows encode the branch in their id ('tip-${branch}'); main-line commits
  // don't have a branch, so only tip rows navigate on click.
  const handleSelect = (commit: Commit) => {
    if (commit.id.startsWith('tip-')) {
      const branch = commit.id.slice('tip-'.length);
      router.push(reviewHref(projectHash, { branch }));
    }
  };

  return (
    <div className="flex flex-col gap-3">
      {ownsToggle && (
        <div className="flex items-center justify-end">
          <ExplainerToggle explainer={explainer} />
        </div>
      )}
      <Explainer explainer={explainer} title="What am I looking at?">
        <p>
          This is the project&rsquo;s timeline, newest at the top. The line down the
          left is{' '}
          <span className="font-mono text-ink-2">
            {payload.defaultBranch ?? 'develop'}
          </span>
          , this project&rsquo;s <Term k="defaultBranch">main line</Term> — everything
          else branches off it.
        </p>
        <p>
          Each square on that line is a <Term k="commit" /> —{' '}
          <span className="text-ink">filled</span> means we have the{' '}
          <Term k="session">AI conversation</Term> that produced it,{' '}
          <span className="text-ink">hollow</span> means none was captured.
        </p>
        <p>
          The cards above the graph are separate{' '}
          <Term k="change">lines of work</Term> still in progress. Click a card
          to see what it changed and the conversations that did it. The plain
          rows are saved history — reference only.
        </p>
        <p>
          Each card carries a freshness badge:{' '}
          <span className="text-ink">active</span> (worked on in the last 3 days),{' '}
          <span className="text-ink">idle</span> (paused — no work for 3 to 14
          days), or <span className="text-ink">stale</span> (untouched for over 2
          weeks).
        </p>
      </Explainer>

      {/* open lines of work — TipCards preserve the full branch richness that
          CommitGraph rows don't carry: lifecycle badge (active/idle/stale), fact
          fragments (commits ahead / files / sessions / requests / connections),
          and the violations indicator. This capability is kept from the original
          ChangeGraph; CommitGraph covers the lane + dot + merge visualization. */}
      {openChanges.length > 0 && (
        <div className="border border-rule bg-surface">
          <div className="px-5 py-2 border-b border-rule">
            <span className="v2-eyebrow">open lines of work</span>
          </div>
          <div className="divide-y divide-rule">
            {openChanges.map((change) => (
              <TipCard
                key={change.branch}
                change={change}
                projectHash={projectHash}
                nowMs={nowMs}
                undated={change.tipCommitMs === undefined}
              />
            ))}
          </div>
        </div>
      )}

      {/* CommitGraph — ft-ui lane visualization. Tip rows give the lane/temporal
          context for each open branch (fork point, session sparkle, time chip).
          Clicking a tip row navigates to the branch detail, same as TipCard. */}
      <div className="border border-rule bg-surface">
        <CommitGraph
          commits={commits}
          onSelect={handleSelect}
          hasMore={graph.hasMore}
          onShowOlder={() => setExpanded(true)}
          label={`Change history for ${projectName}`}
        />

        {allMerged.length > 0 && (
          <div className="flex flex-col gap-1.5 px-5 py-2 border-t border-rule">
            <p className="text-[11px] text-ink-3">
              <span className="v2-eyebrow mr-1.5 inline-block">Already merged in</span>
              Finished lines of work, now part of the main line&rsquo;s history. Open one to see what it changed.
            </p>
            <div className="flex flex-wrap items-center gap-2">
              {allMerged.map((m) => (
                <MergedChip key={m.branch} change={m} projectHash={projectHash} />
              ))}
            </div>
          </div>
        )}
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// TipCard — rich open-branch card (preserved from the original ChangeGraph).
// Carries lifecycle badge, fact line, violations indicator, and a "View" link.
// The CommitGraph above provides the lane/temporal context; TipCard answers the
// "what is this branch, and how fresh is it?" question.
// ---------------------------------------------------------------------------

/** Tone per lifecycle key — weight only, never hue. */
const LIFECYCLE_CLASS: Record<LifecycleBadge['key'], string> = {
  active: 'border-rule-strong text-ink',
  idle:   'border-rule text-ink-3',
  stale:  'border-rule text-ink-4',
};

function TipCard({
  change,
  undated,
  projectHash,
  nowMs,
}: {
  change: ChangeSummary;
  undated: boolean;
  projectHash: ProjectHash;
  nowMs: number;
}) {
  const facts = factFragments(change);
  const lifecycle = openChangeLifecycle(change, nowMs);

  return (
    <Link
      href={reviewHref(projectHash, { branch: change.branch })}
      aria-label={`Open the line of work "${change.branch}"`}
      className="block w-full bg-surface px-5 py-2 hover:bg-surface-hover transition-colors focus-mono cursor-pointer"
    >
      <span className="flex items-center gap-2">
        <span className="min-w-0 flex-1 truncate text-[13px] font-medium text-ink">
          {humanizeBranch(change.branch)}
        </span>
        <span className="flex shrink-0 items-center gap-2">
          {lifecycle && (
            <span
              className={cn(
                'border px-1.5 py-px font-mono text-[10px] uppercase tracking-wide tabular-nums',
                LIFECYCLE_CLASS[lifecycle.key],
              )}
              title={
                lifecycle.key === 'active'
                  ? 'Active — worked on in the last 3 days'
                  : lifecycle.key === 'idle'
                    ? 'Idle — paused; no work for 3 to 14 days'
                    : 'Stale — untouched for over 2 weeks'
              }
            >
              {lifecycle.label}
            </span>
          )}
          <span className="inline-flex items-center gap-1 text-[12px] text-ink-3">
            View <ArrowRight size={12} aria-hidden />
          </span>
        </span>
      </span>
      <span
        className="block font-mono text-[11px] text-ink-4 truncate mt-0.5"
        title="the technical name engineers gave this line of work"
      >
        {change.branch}
        {undated && <span className="text-ink-4"> · undated</span>}
      </span>
      <span className="flex flex-wrap items-center gap-x-2 gap-y-0.5 font-mono text-xs tabular-nums text-ink-2 mt-1">
        <span>{facts.length > 0 ? facts.join(' · ') : 'no changes'}</span>
        {change.violations > 0 && (
          <span
            className="inline-flex items-center gap-1 text-danger"
            aria-label={`${plural(change.violations, 'rule break')} — connections that go against the project's layering`}
          >
            <TriangleAlert size={12} aria-hidden />
            {plural(change.violations, 'rule break')}
          </span>
        )}
        {change.lastWorkMs != null && (
          <span className="text-ink-3">last worked {formatRelative(change.lastWorkMs)}</span>
        )}
      </span>
    </Link>
  );
}

/**
 * Compact fact line: '8 new updates · 136 files · 70 conversations · 98 requests · +39/−22 connections'
 */
function factFragments(c: ChangeSummary): string[] {
  const out: string[] = [];
  if (c.aheadCount > 0)
    out.push(`${c.aheadCount} new ${c.aheadCount === 1 ? 'update' : 'updates'}`);
  if (c.filesChanged > 0) out.push(plural(c.filesChanged, 'file'));
  if (c.sessionCount > 0) out.push(plural(c.sessionCount, 'conversation'));
  if (c.taskCount > 0) out.push(plural(c.taskCount, 'request'));
  const edges: string[] = [];
  if (c.newEdges > 0) edges.push(`+${c.newEdges}`);
  if (c.removedEdges > 0) edges.push(`${MINUS}${c.removedEdges}`);
  if (edges.length > 0) out.push(`${edges.join('/')} connections`);
  return out;
}

/** Dimmed chip for a merged change — folded into the main line. */
function MergedChip({
  change,
  projectHash,
}: {
  change: ChangeSummary;
  projectHash: ProjectHash;
}) {
  return (
    <Link
      href={reviewHref(projectHash, { branch: change.branch })}
      aria-label={`Open the ${change.reverted ? 'reverted' : 'folded-in'} line of work "${change.branch}"`}
      className="shrink-0 border border-rule bg-surface px-2 py-0.5 text-xs text-ink-3 hover:bg-surface-hover transition-colors focus-mono cursor-pointer"
    >
      {change.reverted ? <span className="text-ink-2">reverted</span> : 'folded in'}
      {' · '}
      {humanizeBranch(change.branch)}
      {change.reverted && <span className="text-ink-4"> · then undone</span>}
      {!change.reverted && change.mergedAtMs != null && (
        <span className="text-ink-4"> · {formatRelative(change.mergedAtMs)}</span>
      )}
    </Link>
  );
}
