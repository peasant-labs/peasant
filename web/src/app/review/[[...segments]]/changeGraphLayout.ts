/**
 * Pure layout for the Changes graph (VS Code git-graph idiom): time flows
 * down, lane 0 carries the default branch's commits, each OPEN change forks
 * out on its own lane and rejoins visually at its merge-base, MERGED changes
 * attach as join chips at their merge commit. No React, no rendering — the
 * output is a deterministic row list with lane geometry that ChangeGraph.tsx
 * draws and tests assert on directly.
 */

import type { ChangeSummary, CommitRef } from '@peasant-labs/schema';

/** Initial commit-row window; 'Show older' expands past it. */
export const INITIAL_COMMIT_CAP = 50;

/** An open change's lane elbowing into a commit (its merge-base fork anchor). */
export interface ForkAnchor {
  lane: number;
  branch: string;
}

/** A default-branch commit row (lane 0). */
export interface CommitGraphRow {
  kind: 'commit';
  commit: CommitRef;
  /** Open-change lanes whose fork anchor (baseHash) is this commit. */
  forks: ForkAnchor[];
  /** Merged changes whose mergeCommitHash is this commit (join chips). */
  joins: ChangeSummary[];
  /** Open lanes (>=1) drawing a plain full-height vertical through this band. */
  passLanes: number[];
  /** Lane 0 vertical above / below the dot. */
  laneZeroUp: boolean;
  laneZeroDown: boolean;
}

/** An open change's TIP row — the square card, positioned by tipCommitMs. */
export interface TipGraphRow {
  kind: 'tip';
  change: ChangeSummary;
  lane: number;
  /** tipCommitMs missing — pinned above all commits. */
  undated: boolean;
  /** Other open lanes passing through this band. */
  passLanes: number[];
  /** Lane 0 passes through this band (tip sits inside the commit run). */
  laneZero: boolean;
}

export type ChangeGraphRow = CommitGraphRow | TipGraphRow;

/** An open change whose baseHash is not in the payload — the lane runs to the
 *  bottom edge and ends in a dashed 'forked earlier' tail. */
export interface TailFlag {
  lane: number;
  change: ChangeSummary;
}

export interface ChangeGraph {
  /** Top-to-bottom = newest-to-oldest. */
  rows: ChangeGraphRow[];
  /** Total lanes including lane 0 (max lane index + 1). */
  laneCount: number;
  /** 'forked earlier' dashed tails at the bottom edge, lane ascending. */
  tails: TailFlag[];
  /** Merged changes whose mergeCommitHash is missing / not in the payload —
   *  pinned at the bottom as 'merged earlier' chips. */
  earlierMerges: ChangeSummary[];
  /** Commits beyond the window exist — show the 'Show older' expander. */
  hasMore: boolean;
}

/** Lane-assignment key: tip time desc; missing tip sorts as newest. */
function tipKey(c: ChangeSummary): number {
  return c.tipCommitMs ?? Number.POSITIVE_INFINITY;
}

/**
 * Build the graph. `recentCommits` is the default-branch window from
 * ReviewListPayload (newest first, cap 200 — payload contract). Fully
 * deterministic: shuffling `changes` yields an identical graph.
 *
 * Window rule: cap commit rows at `maxCommits` (default 50), but ALWAYS
 * extend far enough to include every anchor (baseHash / mergeCommitHash)
 * that exists in the payload, so visible lanes never lose their anchor to
 * the cap.
 */
export function buildChangeGraph(
  recentCommits: CommitRef[],
  changes: ChangeSummary[],
  opts: { maxCommits?: number } = {},
): ChangeGraph {
  const cap = opts.maxCommits ?? INITIAL_COMMIT_CAP;

  // Deterministic ordering regardless of input order.
  const open = changes
    .filter((c) => !c.merged)
    .sort((a, b) => tipKey(b) - tipKey(a) || a.branch.localeCompare(b.branch));
  const merged = changes
    .filter((c) => c.merged)
    .sort(
      (a, b) =>
        (b.mergedAtMs ?? Number.NEGATIVE_INFINITY) -
          (a.mergedAtMs ?? Number.NEGATIVE_INFINITY) ||
        a.branch.localeCompare(b.branch),
    );

  const idxByHash = new Map<string, number>();
  recentCommits.forEach((c, i) => {
    if (!idxByHash.has(c.hash)) idxByHash.set(c.hash, i);
  });
  const anchorIdx = (hash?: string): number =>
    hash !== undefined && idxByHash.has(hash) ? idxByHash.get(hash)! : -1;

  // Window: cap, extended to cover every anchor present in the payload.
  let windowLen = Math.min(cap, recentCommits.length);
  for (const c of open) {
    const i = anchorIdx(c.baseHash);
    if (i >= 0) windowLen = Math.max(windowLen, i + 1);
  }
  for (const c of merged) {
    const i = anchorIdx(c.mergeCommitHash);
    if (i >= 0) windowLen = Math.max(windowLen, i + 1);
  }
  const window = recentCommits.slice(0, windowLen);
  const hasMore = windowLen < recentCommits.length;

  // Lane plan for open changes: lane = 1 + sorted index.
  interface LanePlan {
    change: ChangeSummary;
    lane: number;
    undated: boolean;
    /** Tip row goes immediately above the commit at this index ([0..len]). */
    insertIdx: number;
    /** Fork-anchor commit index, -1 = dangling ('forked earlier'). */
    forkIdx: number;
  }
  const plans: LanePlan[] = open.map((change, i) => {
    const lane = i + 1;
    const undated = change.tipCommitMs === undefined;
    const forkIdx = anchorIdx(change.baseHash);
    let insertIdx: number;
    if (undated) {
      insertIdx = 0; // above all commits
    } else {
      insertIdx = window.findIndex(
        (c) => (c.timeMs ?? Number.NEGATIVE_INFINITY) <= change.tipCommitMs!,
      );
      if (insertIdx === -1) insertIdx = window.length;
    }
    // A tip never renders below its own fork anchor (clock skew guard).
    if (forkIdx >= 0 && insertIdx > forkIdx) insertIdx = forkIdx;
    return { change, lane, undated, insertIdx, forkIdx };
  });

  // Join chips per commit index; the rest pin at the bottom.
  const joinsAt = new Map<number, ChangeSummary[]>();
  const earlierMerges: ChangeSummary[] = [];
  for (const m of merged) {
    const i = anchorIdx(m.mergeCommitHash);
    if (i >= 0) {
      const list = joinsAt.get(i) ?? [];
      list.push(m);
      joinsAt.set(i, list);
    } else {
      earlierMerges.push(m);
    }
  }

  // Assemble rows: for each commit slot, tips first (lane order = time desc),
  // then the commit itself.
  const rows: ChangeGraphRow[] = [];
  const tipRowIdx = new Map<number, number>(); // lane -> row index
  const commitRowIdx: number[] = []; // commit index -> row index
  for (let ci = 0; ci <= window.length; ci++) {
    for (const p of plans) {
      if (p.insertIdx !== ci) continue;
      tipRowIdx.set(p.lane, rows.length);
      rows.push({
        kind: 'tip',
        change: p.change,
        lane: p.lane,
        undated: p.undated,
        passLanes: [],
        laneZero: ci > 0 && (ci < window.length || hasMore),
      });
    }
    if (ci < window.length) {
      commitRowIdx[ci] = rows.length;
      rows.push({
        kind: 'commit',
        commit: window[ci],
        forks: plans
          .filter((p) => p.forkIdx === ci)
          .map((p) => ({ lane: p.lane, branch: p.change.branch })),
        joins: joinsAt.get(ci) ?? [],
        passLanes: [],
        laneZeroUp: ci > 0,
        laneZeroDown: ci < window.length - 1 || hasMore,
      });
    }
  }

  // Pass-through verticals: each open lane spans (tip row, end row), where the
  // end row is its fork-anchor commit row, or the bottom edge when dangling.
  for (const p of plans) {
    const start = tipRowIdx.get(p.lane)!;
    const end = p.forkIdx >= 0 ? commitRowIdx[p.forkIdx] : rows.length;
    for (let r = start + 1; r < end; r++) rows[r].passLanes.push(p.lane);
  }

  const tails: TailFlag[] = plans
    .filter((p) => p.forkIdx < 0)
    .map((p) => ({ lane: p.lane, change: p.change }));

  return {
    rows,
    laneCount: open.length + 1,
    tails,
    earlierMerges,
    hasMore,
  };
}
