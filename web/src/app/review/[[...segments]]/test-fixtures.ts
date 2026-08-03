/**
 * Shared fixtures for the Changes surface tests. One payload shape change
 * should require updating this file, not N test files.
 */

import type {
  ChangeSummary,
  CommitRef,
  ReviewListPayload,
  SessionAssociation,
} from '@peasant-labs/schema';
import type { DecodedChangeDetailPayload, DecodedTaskSummary } from '@/lib/api/map';
import type { SessionSummary } from '@/types/messages';
import type { ProjectHash } from '@/lib/navigation/projectRoutes';

type ChangeSession = DecodedChangeDetailPayload['work'][number];
type TaskSummary = DecodedTaskSummary;

const MAP_NODE_DEFAULTS = {
  agentEditedCount: 0,
  readCount: 0,
  readAttribution: 'unavailable',
  readState: 'none',
  changedRegionCount: 0,
  attributedRegionCount: 0,
  reviewedRegionCount: 0,
} as const;

function recordedCommitAssociation(hash: string, sessionId: string): SessionAssociation {
  return {
    id: `fixture:${hash}:${sessionId}`,
    sessionId,
    conclusion: 'confirmed',
    confidence: 'high',
    evidence: [{ kind: 'recorded_commit', recordedCommitHash: hash }],
  };
}

function commitRef(hash: string, subject: string, timeMs: number, sessionIds: string[]): CommitRef {
  return {
    hash,
    subject,
    timeMs,
    hasSession: sessionIds.length > 0,
    sessionIds,
    associations: sessionIds.map((sessionId) => recordedCommitAssociation(hash, sessionId)),
  };
}

export const ALPHA_HASH = 'a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90' as ProjectHash;
export const BETA_HASH = 'b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90a1' as ProjectHash;

/** A sessions-channel row carrying its opaque `projectHash`. */
export function makeSession(
  over: Partial<SessionSummary>,
): SessionSummary {
  return {
    id: 'sess-0001',
    harness: 'claude-code',
    startTime: '2026-06-01T09:00:00Z',
    durationMins: 30,
    totalTokens: 10_000,
    turnCount: 10,
    toolCallCount: 5,
    project: 'alpha-project',
    projectHash: ALPHA_HASH,
    ...over,
  };
}

export function makeTask(
  sessionId: string,
  entryIndex: number,
  over: Partial<TaskSummary> = {},
): TaskSummary {
  return {
    sessionId,
    entryIndex,
    title: `task ${entryIndex} of ${sessionId}`,
    editedFiles: [],
    readCount: 0,
    readFiles: [],
    retryLoop: false,
    labels: [],
    ...over,
  };
}

function tasks(sessionId: string, count: number): TaskSummary[] {
  return Array.from({ length: count }, (_, i) => makeTask(sessionId, i));
}

// ---------------------------------------------------------------------------
// Change-graph factories (deterministic; changeGraph tests build tables off
// these instead of repeating literals).
// ---------------------------------------------------------------------------

/** Fixed "now" for graph fixtures whose layout must be time-deterministic. */
export const COMMIT_BASE_MS = Date.parse('2026-06-09T12:00:00Z');

/** A default-branch commit, `i` hours old; even indexes carry a session. */
export function makeCommitRef(i: number, over: Partial<CommitRef> = {}): CommitRef {
  const sessionIds = i % 2 === 0 ? [`session-${i}`] : [];
  return {
    ...commitRef(
      `${i.toString(16).padStart(4, '0')}${'0'.repeat(36)}`,
      `commit ${i}`,
      COMMIT_BASE_MS - i * 3_600_000,
      sessionIds,
    ),
    ...over,
  };
}

/** An open (unmerged) change with quiet counts; override per test. */
export function makeChange(
  branch: string,
  over: Partial<ChangeSummary> = {},
): ChangeSummary {
  return {
    branch,
    aheadCount: 1,
    behindCount: 0,
    filesChanged: 1,
    sessionCount: 1,
    taskCount: 1,
    newEdges: 0,
    removedEdges: 0,
    violations: 0,
    merged: false,
    ...over,
  };
}

// ---------------------------------------------------------------------------
// Changes list: two open changes + one merged,
// over a 3-commit default-branch window. Anchors (baseHash / tipCommitMs /
// mergeCommitHash) are pinned to these commits so the graph fixtures exercise
// fork, join, AND the missing-anchor paths (fix/ingest-retry has none).
// ---------------------------------------------------------------------------

/** Default-branch commits, newest first (payload contract). */
export const COMMIT_NEWEST = commitRef(
  'c0ffee0011223344556677889900aabbccddeeff',
  'feat(web): land the graph gutter',
  Date.now() - 1 * 3_600_000,
  ['sess-abcdef12'],
);
export const COMMIT_MID = commitRef(
  'c1bada5500112233445566778899aabbccddeeff',
  'chore: bump deps',
  Date.now() - 26 * 3_600_000,
  [],
);
export const COMMIT_OLDEST = commitRef(
  'c2feed7700112233445566778899aabbccddeeff',
  'fix(api): serve entry-level annotations',
  Date.now() - 50 * 3_600_000,
  ['sess-abcdef12'],
);

export const REVIEW_LIST_PAYLOAD: ReviewListPayload = {
  projectHash: ALPHA_HASH,
  repoFound: true,
  defaultBranch: 'develop',
  changes: [
    {
      branch: 'feat/graph-cache',
      aheadCount: 9,
      behindCount: 0,
      filesChanged: 14,
      sessionCount: 3,
      taskCount: 21,
      newEdges: 2,
      removedEdges: 0,
      violations: 1,
      lastWorkMs: Date.now() - 2 * 3_600_000,
      merged: false,
      // Tip between the two newest commits; forks from the oldest.
      tipCommitMs: Date.now() - 2 * 3_600_000,
      baseHash: COMMIT_OLDEST.hash,
    },
    {
      // No tipCommitMs (undated) and no baseHash (forked earlier).
      branch: 'fix/ingest-retry',
      aheadCount: 2,
      behindCount: 1,
      filesChanged: 3,
      sessionCount: 1,
      taskCount: 6,
      newEdges: 0,
      removedEdges: 0,
      violations: 0,
      merged: false,
    },
    {
      branch: 'feat/project-overview',
      aheadCount: 0,
      behindCount: 0,
      filesChanged: 22,
      sessionCount: 4,
      taskCount: 35,
      newEdges: 5,
      removedEdges: 0,
      violations: 0,
      lastWorkMs: Date.now() - 3 * 86_400_000,
      merged: true,
      mergedAtMs: Date.now() - 3 * 86_400_000,
      mergeCommitHash: COMMIT_MID.hash,
    },
  ],
  recentCommits: [COMMIT_NEWEST, COMMIT_MID, COMMIT_OLDEST],
  sessions: [
    {
      sessionId: 'sess-abcdef12',
      title: 'Build the graph cache',
      harness: 'claude-code',
      startMs: Date.now() - 3 * 3_600_000,
      hasCommitBinding: true,
    },
  ],
  rewrittenCommits: [],
};

// ---------------------------------------------------------------------------
// Change detail matches the caption exact-string test:
// "+2 import edges (1 wrong-way) · 14 files in internal/ingest, internal/api ·
//  3 sessions, 21 tasks · retry loops in 2 tasks"
// ---------------------------------------------------------------------------

export const SESSION_A = 'sess-aaaa0001';
export const SESSION_B = 'sess-bbbb0002';
export const SESSION_C = 'sess-cccc0003';

const WORK: ChangeSession[] = [
  {
    sessionId: SESSION_A,
    title: 'add caching to ingest',
    harness: 'claude-code',
    startMs: Date.parse('2026-06-07T10:00:00Z'),
    binding: 'bound',
    tasks: [
      ...tasks(SESSION_A, 12),
      makeTask(SESSION_A, 12, {
        title: 'tests are failing',
        retryLoop: true,
        // Two of the actual changed FILES below, so the file-first changed-file
        // list links these rows to this conversation and the hover-relay lights
        // them up.
        editedFiles: ['internal/ingest/file0.go', 'internal/ingest/file1.go'],
      }),
    ],
  },
  {
    sessionId: SESSION_B,
    title: 'wire the cache into the api',
    harness: 'opencode',
    startMs: Date.parse('2026-06-08T09:00:00Z'),
    binding: 'bound',
    tasks: [
      ...tasks(SESSION_B, 5),
      makeTask(SESSION_B, 5, { title: 'retry the flaky fetch', retryLoop: true }),
    ],
  },
  {
    sessionId: SESSION_C,
    title: 'unrelated branch poke',
    harness: 'claude-code',
    binding: 'candidate',
    tasks: tasks(SESSION_C, 2),
  },
];

/** 14 files: 8 in internal/ingest, 4 in internal/api, 1 in web/src/lib, 1 root. */
const FILES: DecodedChangeDetailPayload['files'] = [
  ...Array.from({ length: 8 }, (_, i) => ({
    path: `internal/ingest/file${i}.go`,
    status: 'M' as const,
    linesAdded: 10 + i * 3,
    linesRemoved: i,
  })),
  { path: 'internal/api/server.go', status: 'M' as const, linesAdded: 40, linesRemoved: 12 },
  { path: 'internal/api/provider.go', status: 'A' as const, linesAdded: 90, linesRemoved: 0 },
  { path: 'internal/api/messages.go', status: 'M' as const, linesAdded: 7, linesRemoved: 4 },
  {
    path: 'internal/api/handler.go',
    status: 'R' as const,
    oldPath: 'internal/api/handlers.go',
    linesAdded: 3,
    linesRemoved: 2,
  },
  { path: 'web/src/lib/api.ts', status: 'M' as const, linesAdded: 22, linesRemoved: 5 },
  { path: 'README.md', status: 'M' as const, linesAdded: 2, linesRemoved: 1 },
];

export const CHANGE_DETAIL_PAYLOAD: DecodedChangeDetailPayload = {
  branch: 'feat/graph-cache',
  baseRef: 'deadbeef00112233445566778899aabbccddeeff',
  defaultBranch: 'develop',
  files: FILES,
  slice: {
    nodes: [
      {
        ...MAP_NODE_DEFAULTS,
        id: 'internal/ingest',
        kind: 'package',
        name: 'ingest',
        layer: 1,
        order: 0,
        loc: 4200,
        fileCount: 12,
        recordedFiles: 10,
        totalFiles: 12,
        touchCount: 40,
        effortDensity: 0.2,
      },
      {
        ...MAP_NODE_DEFAULTS,
        id: 'internal/cache',
        kind: 'package',
        name: 'cache',
        layer: 1,
        order: 1,
        loc: 300,
        fileCount: 2,
        recordedFiles: 2,
        totalFiles: 2,
        touchCount: 8,
        effortDensity: 0,
      },
      {
        ...MAP_NODE_DEFAULTS,
        id: 'internal/api',
        kind: 'package',
        name: 'api',
        layer: 0,
        order: 0,
        loc: 2100,
        fileCount: 9,
        recordedFiles: 7,
        totalFiles: 9,
        touchCount: 22,
        effortDensity: 0.1,
      },
    ],
    structureEdges: [{ from: 'internal/api', to: 'internal/ingest', count: 3 }],
    activityEdges: [{ from: 'internal/ingest', to: 'internal/api', taskCount: 4 }],
  },
  newEdges: [
    { from: 'internal/ingest', to: 'internal/cache', count: 2 },
    { from: 'internal/api', to: 'internal/cache', count: 1 },
  ],
  removedEdges: [],
  newNodes: ['internal/cache'],
  removedNodes: [],
  violations: [{ kind: 'wrong_way', from: 'internal/api', to: 'cmd/peasant' }],
  work: WORK,
  unrecordedCommits: [
    { hash: 'abc1234def5678901234567890123456789012ab', subject: 'bump deps', hasSession: false, sessionIds: [], associations: [] },
  ],
  unusual: [],
  frictions: [],
  insights: [],
  linesAdded: 612,
  linesRemoved: 128,
  outputTokens: BigInt(412_000),
  costUsd: 4.1,
};
