import { describe, it, expect } from 'vitest';
import type { ChangeSummary, CommitRef } from '@peasant-labs/schema';
import {
  buildChangeGraph,
  INITIAL_COMMIT_CAP,
  type ChangeGraph,
  type CommitGraphRow,
  type TipGraphRow,
} from './changeGraphLayout';
import { COMMIT_BASE_MS, makeChange, makeCommitRef } from './test-fixtures';

const HOUR = 3_600_000;

function commits(n: number): CommitRef[] {
  return Array.from({ length: n }, (_, i) => makeCommitRef(i));
}

/** Compact row signature for whole-shape assertions. */
function signature(g: ChangeGraph): string[] {
  return g.rows.map((r) =>
    r.kind === 'commit' ? `commit:${r.commit.subject}` : `tip:${r.change.branch}`,
  );
}

function tipRow(g: ChangeGraph, branch: string): TipGraphRow {
  const row = g.rows.find(
    (r): r is TipGraphRow => r.kind === 'tip' && r.change.branch === branch,
  );
  expect(row, `tip row for ${branch}`).toBeDefined();
  return row!;
}

function commitRow(g: ChangeGraph, hash: string): CommitGraphRow {
  const row = g.rows.find(
    (r): r is CommitGraphRow => r.kind === 'commit' && r.commit.hash === hash,
  );
  expect(row, `commit row for ${hash}`).toBeDefined();
  return row!;
}

function commitRowCount(g: ChangeGraph): number {
  return g.rows.filter((r) => r.kind === 'commit').length;
}

// -- Lane assignment ------------------------------------------------------------

describe('buildChangeGraph — lane assignment', () => {
  it('assigns lanes by tip time desc: lane 1 is the newest tip', () => {
    const g = buildChangeGraph(commits(6), [
      makeChange('older', { tipCommitMs: COMMIT_BASE_MS - 3.5 * HOUR }),
      makeChange('newer', { tipCommitMs: COMMIT_BASE_MS - 1.5 * HOUR }),
    ]);
    expect(tipRow(g, 'newer').lane).toBe(1);
    expect(tipRow(g, 'older').lane).toBe(2);
    expect(g.laneCount).toBe(3);
  });

  it('breaks tip-time ties by branch name', () => {
    const tie = { tipCommitMs: COMMIT_BASE_MS - 2.5 * HOUR };
    const g = buildChangeGraph(commits(6), [
      makeChange('zeta', tie),
      makeChange('alpha', tie),
    ]);
    expect(tipRow(g, 'alpha').lane).toBe(1);
    expect(tipRow(g, 'zeta').lane).toBe(2);
  });

  it('treats undated tips (no tipCommitMs) as newest for lane order', () => {
    const g = buildChangeGraph(commits(4), [
      makeChange('dated', { tipCommitMs: COMMIT_BASE_MS - 0.5 * HOUR }),
      makeChange('undated'),
    ]);
    expect(tipRow(g, 'undated').lane).toBe(1);
    expect(tipRow(g, 'dated').lane).toBe(2);
  });

  it('merged changes never take a lane', () => {
    const g = buildChangeGraph(commits(4), [
      makeChange('open-one', { tipCommitMs: COMMIT_BASE_MS - HOUR }),
      makeChange('was-merged', { merged: true, mergedAtMs: COMMIT_BASE_MS - HOUR }),
    ]);
    expect(g.laneCount).toBe(2); // lane 0 + one open lane
    expect(g.rows.filter((r) => r.kind === 'tip')).toHaveLength(1);
  });

  it('is fully deterministic under shuffled (and frozen) input', () => {
    const cs = commits(10);
    const changes: ChangeSummary[] = [
      makeChange('a', { tipCommitMs: COMMIT_BASE_MS - 1.5 * HOUR, baseHash: cs[6].hash }),
      makeChange('b', { tipCommitMs: COMMIT_BASE_MS - 4.5 * HOUR, baseHash: cs[8].hash }),
      makeChange('c'), // undated, dangling
      makeChange('m', { merged: true, mergedAtMs: COMMIT_BASE_MS, mergeCommitHash: cs[3].hash }),
      makeChange('m2', { merged: true }), // merged earlier
    ];
    const sorted = buildChangeGraph(cs, changes);
    const shuffled = buildChangeGraph(
      cs,
      Object.freeze([changes[3], changes[1], changes[4], changes[0], changes[2]]) as ChangeSummary[],
    );
    expect(shuffled).toEqual(sorted);
    expect(buildChangeGraph(cs, [...changes].reverse())).toEqual(sorted);
  });
});

// -- Tip positioning --------------------------------------------------------------

describe('buildChangeGraph — tip positioning', () => {
  it('places a tip row above the first commit that is not newer than its tip time', () => {
    const g = buildChangeGraph(commits(6), [
      makeChange('mid', { tipCommitMs: COMMIT_BASE_MS - 2.5 * HOUR }),
    ]);
    // 2.5h-old tip sits between commit 2 (2h) and commit 3 (3h).
    expect(signature(g)).toEqual([
      'commit:commit 0',
      'commit:commit 1',
      'commit:commit 2',
      'tip:mid',
      'commit:commit 3',
      'commit:commit 4',
      'commit:commit 5',
    ]);
    expect(tipRow(g, 'mid').undated).toBe(false);
    // The tip sits inside the commit run — lane 0 passes through its band.
    expect(tipRow(g, 'mid').laneZero).toBe(true);
  });

  it('pins undated tips above all commits, flagged undated, lane 0 not passing', () => {
    const g = buildChangeGraph(commits(3), [makeChange('mystery')]);
    expect(signature(g)[0]).toBe('tip:mystery');
    expect(tipRow(g, 'mystery').undated).toBe(true);
    expect(tipRow(g, 'mystery').laneZero).toBe(false);
  });

  it('stacks tips sharing a slot in lane order (undated first, then newest)', () => {
    const g = buildChangeGraph(commits(3), [
      makeChange('dated-new', { tipCommitMs: COMMIT_BASE_MS + HOUR }), // newer than all
      makeChange('undated'),
    ]);
    expect(signature(g).slice(0, 2)).toEqual(['tip:undated', 'tip:dated-new']);
  });

  it('clamps a tip directly above its fork anchor when timestamps invert', () => {
    const cs = commits(6);
    const g = buildChangeGraph(cs, [
      // Tip "older" than its own merge-base (clock skew) — never below it.
      makeChange('skewed', { tipCommitMs: COMMIT_BASE_MS - 100 * HOUR, baseHash: cs[2].hash }),
    ]);
    const sig = signature(g);
    expect(sig.indexOf('tip:skewed')).toBe(sig.indexOf('commit:commit 2') - 1);
  });

  it('places a dangling tip older than the whole window below the last commit', () => {
    const g = buildChangeGraph(commits(3), [
      makeChange('ancient', { tipCommitMs: COMMIT_BASE_MS - 100 * HOUR }),
    ]);
    expect(signature(g).at(-1)).toBe('tip:ancient');
  });
});

// -- Fork anchoring ----------------------------------------------------------------

describe('buildChangeGraph — fork anchoring', () => {
  it('elbows the lane into its baseHash commit and threads pass-throughs between', () => {
    const cs = commits(6);
    const g = buildChangeGraph(cs, [
      makeChange('feat/x', { tipCommitMs: COMMIT_BASE_MS - 0.5 * HOUR, baseHash: cs[4].hash }),
    ]);
    expect(signature(g)).toEqual([
      'commit:commit 0',
      'tip:feat/x',
      'commit:commit 1',
      'commit:commit 2',
      'commit:commit 3',
      'commit:commit 4',
      'commit:commit 5',
    ]);
    expect(commitRow(g, cs[4].hash).forks).toEqual([{ lane: 1, branch: 'feat/x' }]);
    // Rows strictly between tip and fork carry the vertical.
    expect(commitRow(g, cs[1].hash).passLanes).toEqual([1]);
    expect(commitRow(g, cs[2].hash).passLanes).toEqual([1]);
    expect(commitRow(g, cs[3].hash).passLanes).toEqual([1]);
    // Anchor row and rows below it do not.
    expect(commitRow(g, cs[4].hash).passLanes).toEqual([]);
    expect(commitRow(g, cs[5].hash).passLanes).toEqual([]);
    expect(g.tails).toEqual([]);
  });

  it('runs unanchored lanes to the bottom edge with a forked-earlier tail', () => {
    const cs = commits(4);
    const change = makeChange('feat/old', {
      tipCommitMs: COMMIT_BASE_MS - 0.5 * HOUR,
      baseHash: 'not-in-the-payload',
    });
    const g = buildChangeGraph(cs, [change]);
    expect(g.tails).toEqual([{ lane: 1, change }]);
    // Every row below the tip carries the lane — including the last one.
    expect(commitRow(g, cs[1].hash).passLanes).toEqual([1]);
    expect(commitRow(g, cs[3].hash).passLanes).toEqual([1]);
  });

  it('keeps pass-lane lists ascending with multiple lanes in flight', () => {
    const cs = commits(8);
    const g = buildChangeGraph(cs, [
      makeChange('one', { tipCommitMs: COMMIT_BASE_MS - 0.5 * HOUR, baseHash: cs[6].hash }),
      makeChange('two', { tipCommitMs: COMMIT_BASE_MS - 1.5 * HOUR, baseHash: cs[7].hash }),
    ]);
    expect(commitRow(g, cs[3].hash).passLanes).toEqual([1, 2]);
    // 'two' (lane 2) passes through rows below lane 1's fork anchor alone.
    expect(commitRow(g, cs[6].hash).passLanes).toEqual([2]);
  });
});

// -- Join chips ---------------------------------------------------------------------

describe('buildChangeGraph — merged joins', () => {
  it('attaches merged changes at their mergeCommitHash commit row, no lane', () => {
    const cs = commits(5);
    const m = makeChange('feat/done', {
      merged: true,
      mergedAtMs: COMMIT_BASE_MS - 3 * HOUR,
      mergeCommitHash: cs[3].hash,
    });
    const g = buildChangeGraph(cs, [m]);
    expect(commitRow(g, cs[3].hash).joins).toEqual([m]);
    expect(g.earlierMerges).toEqual([]);
    expect(g.laneCount).toBe(1);
  });

  it('orders multiple joins at one commit by mergedAt desc, then branch', () => {
    const cs = commits(3);
    const a = makeChange('aaa', { merged: true, mergeCommitHash: cs[1].hash });
    const b = makeChange('bbb', {
      merged: true,
      mergedAtMs: COMMIT_BASE_MS,
      mergeCommitHash: cs[1].hash,
    });
    const g = buildChangeGraph(cs, [a, b]);
    expect(commitRow(g, cs[1].hash).joins.map((j) => j.branch)).toEqual(['bbb', 'aaa']);
  });

  it('pins merges with a missing or unknown mergeCommitHash at the bottom', () => {
    const noHash = makeChange('lost-a', { merged: true, mergedAtMs: COMMIT_BASE_MS });
    const unknown = makeChange('lost-b', { merged: true, mergeCommitHash: 'gone' });
    const g = buildChangeGraph(commits(3), [unknown, noHash]);
    expect(g.earlierMerges.map((m) => m.branch)).toEqual(['lost-a', 'lost-b']);
    expect(g.rows.every((r) => r.kind !== 'commit' || r.joins.length === 0)).toBe(true);
  });
});

// -- Window cap and extension ----------------------------------------------------------

describe('buildChangeGraph — commit window', () => {
  it('caps the initial window at 50 commit rows and reports hasMore', () => {
    const g = buildChangeGraph(commits(80), [makeChange('any')]);
    expect(commitRowCount(g)).toBe(INITIAL_COMMIT_CAP);
    expect(g.hasMore).toBe(true);
  });

  it('extends the window past the cap to include a fork anchor in the payload', () => {
    const cs = commits(80);
    const g = buildChangeGraph(cs, [
      makeChange('deep', { tipCommitMs: COMMIT_BASE_MS - HOUR, baseHash: cs[64].hash }),
    ]);
    expect(commitRowCount(g)).toBe(65);
    expect(g.hasMore).toBe(true);
    expect(commitRow(g, cs[64].hash).forks).toEqual([{ lane: 1, branch: 'deep' }]);
    expect(g.tails).toEqual([]);
  });

  it('extends for merge anchors too, and clears hasMore at the payload end', () => {
    const cs = commits(60);
    const g = buildChangeGraph(cs, [
      makeChange('done', { merged: true, mergeCommitHash: cs[59].hash }),
    ]);
    expect(commitRowCount(g)).toBe(60);
    expect(g.hasMore).toBe(false);
    expect(commitRow(g, cs[59].hash).joins).toHaveLength(1);
  });

  it('does not extend for anchors absent from the payload', () => {
    const g = buildChangeGraph(commits(80), [
      makeChange('lost', { baseHash: 'nowhere' }),
    ]);
    expect(commitRowCount(g)).toBe(INITIAL_COMMIT_CAP);
    expect(g.tails).toHaveLength(1);
  });

  it('shows the full payload with maxCommits: Infinity (the Show older expansion)', () => {
    const g = buildChangeGraph(commits(80), [makeChange('any')], {
      maxCommits: Number.POSITIVE_INFINITY,
    });
    expect(commitRowCount(g)).toBe(80);
    expect(g.hasMore).toBe(false);
  });

  it('reports no hasMore when the payload fits the cap', () => {
    const g = buildChangeGraph(commits(10), [makeChange('any')]);
    expect(commitRowCount(g)).toBe(10);
    expect(g.hasMore).toBe(false);
  });
});

// -- Lane-0 continuity and degenerate payloads ------------------------------------------

describe('buildChangeGraph — lane 0 and degenerate payloads', () => {
  it('starts lane 0 at the first commit and ends it at the last (no hasMore)', () => {
    const cs = commits(3);
    const g = buildChangeGraph(cs, [makeChange('x', { tipCommitMs: COMMIT_BASE_MS })]);
    expect(commitRow(g, cs[0].hash).laneZeroUp).toBe(false);
    expect(commitRow(g, cs[0].hash).laneZeroDown).toBe(true);
    expect(commitRow(g, cs[2].hash).laneZeroDown).toBe(false);
  });

  it('continues lane 0 past the last visible commit when hasMore', () => {
    const cs = commits(60);
    const g = buildChangeGraph(cs, [makeChange('x')]);
    const last = g.rows.filter((r): r is CommitGraphRow => r.kind === 'commit').at(-1)!;
    expect(g.hasMore).toBe(true);
    expect(last.laneZeroDown).toBe(true);
  });

  it('degrades without commits: tips stack, lanes dangle, merges pin at the bottom', () => {
    const open1 = makeChange('a');
    const open2 = makeChange('b');
    const m = makeChange('m', { merged: true, mergedAtMs: COMMIT_BASE_MS });
    const g = buildChangeGraph([], [m, open2, open1]);
    expect(signature(g)).toEqual(['tip:a', 'tip:b']);
    expect(g.rows.every((r) => r.kind === 'tip' && !r.laneZero)).toBe(true);
    expect(g.tails.map((t) => t.change.branch)).toEqual(['a', 'b']);
    expect(g.earlierMerges).toEqual([m]);
    expect(g.hasMore).toBe(false);
  });
});
