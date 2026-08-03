/**
 * Shared test fixtures for the Map page. One project's worth of
 * sessions-channel rows plus the four REST
 * payloads the surface consumes — used by both the pure-helper tests and the
 * MapShell component tests. Test-only; not exported from any barrel.
 */

import type { CommitRef, ReviewListPayload, SessionAssociation } from '@peasant-labs/schema';
import type {
  DecodedMapGraphPayload,
  DecodedMapNodeDetailPayload,
  DecodedProjectSummariesPayload,
  DecodedProjectTasksPayload,
  DecodedTaskSummary,
} from '@/lib/api/map';
import type { Harness, SessionSummary } from '@/types/messages';
import type { ProjectHash } from '@/lib/navigation/projectRoutes';

export const PROJECT = 'alpha-project';
export const OTHER_PROJECT = 'beta-project';
export const PROJECT_HASH = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' as ProjectHash;
export const OTHER_PROJECT_HASH = 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb' as ProjectHash;

// Local-time fixture instants (local Date components so the sessions/day
// bucketing is deterministic in any timezone).
export const NOW_MS = new Date(2026, 5, 9, 12, 0, 0).getTime(); // 2026-06-09 local
export const T_NEWEST = new Date(2026, 5, 8, 10, 0, 0); // 2026-06-08 local
export const T_OLDER = new Date(2026, 5, 1, 10, 0, 0); // 2026-06-01 local

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

export function makeCommitRef(
  hash: string,
  subject: string,
  timeMs: number | null,
  sessionIds: string[],
): CommitRef {
  return {
    hash,
    subject,
    timeMs,
    hasSession: sessionIds.length > 0,
    sessionIds,
    associations: sessionIds.map((sessionId) => recordedCommitAssociation(hash, sessionId)),
  };
}

export function makeSession(over: Partial<SessionSummary>): SessionSummary {
  return {
    id: 'sess-0000',
    harness: 'claude-code' as Harness,
    startTime: T_NEWEST.toISOString(),
    durationMins: 30,
    totalTokens: 10_000,
    turnCount: 10,
    toolCallCount: 5,
    project: PROJECT,
    projectHash: PROJECT_HASH,
    ...over,
  };
}

/**
 * Three sessions with canonical identities: two share the selected project
 * (including the older zero-touch row); one has the same wire shape but a
 * distinct project hash.
 */
export const SESSIONS: SessionSummary[] = [
  makeSession({
    id: 'sess-new',
    preview: 'fix the ingest retry loop',
    startTime: T_NEWEST.toISOString(),
  }),
  makeSession({
    id: 'sess-old',
    startTime: T_OLDER.toISOString(),
    toolCallCount: 0,
    turnCount: 2,
  }),
  makeSession({
    id: 'sess-beta',
    project: OTHER_PROJECT,
    projectHash: OTHER_PROJECT_HASH,
    startTime: new Date(2026, 5, 7, 10, 0, 0).toISOString(),
  }),
];

/**
 * Graph: two root modules (cmd 2/2 recorded, internal 6/10) and one child
 * package — root-only coverage is 8 of 12 files. One structure edge, two
 * activity edges (one touching internal/ingest, for the node panel's
 * "Often edited with" rows), one cycle violation.
 */
export const GRAPH: DecodedMapGraphPayload = {
  projectHash: PROJECT_HASH,
  repoFound: true,
  repoPath: '/Users/test/alpha-project',
  parsedLanguages: ['go'],
  nodes: [
    {
      ...MAP_NODE_DEFAULTS,
      id: 'cmd',
      kind: 'module',
      name: 'cmd',
      layer: 0,
      order: 0,
      loc: 400,
      fileCount: 2,
      recordedFiles: 2,
      totalFiles: 2,
      touchCount: 4,
      effortDensity: 0,
    },
    {
      ...MAP_NODE_DEFAULTS,
      id: 'internal',
      kind: 'module',
      name: 'internal',
      layer: 1,
      order: 0,
      loc: 3000,
      fileCount: 10,
      recordedFiles: 6,
      totalFiles: 10,
      touchCount: 20,
      effortDensity: 0.4,
    },
    {
      ...MAP_NODE_DEFAULTS,
      id: 'internal/ingest',
      parent: 'internal',
      kind: 'package',
      name: 'ingest',
      layer: 1,
      order: 0,
      loc: 1200,
      fileCount: 7,
      recordedFiles: 6,
      totalFiles: 7,
      touchCount: 18,
      effortDensity: 0.8,
    },
  ],
  structureEdges: [{ from: 'cmd', to: 'internal', count: 2 }],
  activityEdges: [
    { from: 'cmd', to: 'internal', taskCount: 3 },
    { from: 'internal/ingest', to: 'cmd', taskCount: 5 },
  ],
  violations: [{ kind: 'cycle', from: 'internal', to: 'cmd' }],
  generatedAtMs: BigInt(NOW_MS),
};

/** Graph variant with a file child for real CodeMap search/grain composition. */
export const GRAPH_WITH_FILE: DecodedMapGraphPayload = {
  ...GRAPH,
  nodes: [
    ...GRAPH.nodes,
    {
      ...MAP_NODE_DEFAULTS,
      id: 'internal/ingest/pipeline.go',
      parent: 'internal/ingest',
      kind: 'file',
      name: 'pipeline.go',
      language: 'go',
      layer: 2,
      order: 0,
      loc: 240,
      fileCount: 1,
      recordedFiles: 1,
      totalFiles: 1,
      touchCount: 4,
      effortDensity: 0.9,
    },
  ],
  structureEdges: [
    ...GRAPH.structureEdges,
    { from: 'internal/ingest/pipeline.go', to: 'cmd', count: 1 },
  ],
};

/** Graph variant for the no-parsed-language fallback. */
export const GRAPH_NO_PARSE: DecodedMapGraphPayload = {
  ...GRAPH,
  parsedLanguages: [],
  structureEdges: [],
  violations: [],
};

export function makeTask(over: Partial<DecodedTaskSummary>): DecodedTaskSummary {
  return {
    sessionId: 'sess-new',
    entryIndex: 12,
    title: 'fix ingest retry loop',
    startMs: T_NEWEST.getTime(),
    outcome: 'resolved',
    editedFiles: ['internal/ingest/pipeline.go'],
    readCount: 3,
    readFiles: [],
    retryLoop: true,
    labels: ['bug'],
    ...over,
  };
}

export const TASK_INGEST = makeTask({});
export const TASK_WEB = makeTask({
  sessionId: 'sess-old',
  entryIndex: 4,
  title: 'tweak web styles',
  startMs: T_OLDER.getTime(),
  outcome: 'partial',
  editedFiles: ['web/src/app/page.tsx'],
  retryLoop: false,
  labels: [],
});

export const NODE_DETAIL: DecodedMapNodeDetailPayload = {
  path: 'internal/ingest',
  kind: 'package',
  language: 'go',
  loc: 1200,
  recordedFiles: 6,
  totalFiles: 7,
  sessionCount: 3,
  taskCount: 5,
  lastTouchMs: T_NEWEST.getTime(),
  dependsOn: ['internal/store', 'internal/queue'],
  usedBy: ['internal/api'],
  shapedBy: [
    TASK_INGEST,
    makeTask({
      sessionId: 'sess-old',
      entryIndex: 4,
      title: 'add diff classifier',
      startMs: T_OLDER.getTime(),
      outcome: 'partial',
      retryLoop: false,
      labels: [],
    }),
  ],
  recentCommits: [
    makeCommitRef('ffff111eeee2222', 'manual hotfix', new Date(2026, 5, 8, 16, 0, 0).getTime(), []),
    makeCommitRef('abc1234deadbee', 'fix ingest', new Date(2026, 5, 2, 10, 0, 0).getTime(), ['sess-old']),
  ],
  retryLoops: 3,
  reEdits: 2,
  costUsd: 4.1,
  rewrittenCommits: [],
  insights: [],
};

export const TASKS: DecodedProjectTasksPayload = {
  projectHash: PROJECT_HASH,
  tasks: [TASK_INGEST, TASK_WEB],
};

/**
 * Home-picker / hash-resolution summary: one row per project, each
 * carrying its `projectHash`. The Map prefers this over the sessions-channel
 * hash, so a named project whose sessions lack the hash still resolves.
 */
export const SUMMARIES: DecodedProjectSummariesPayload = {
  projects: [
    {
      projectHash: PROJECT_HASH,
      project: PROJECT,
      sessions: 3,
      recordedFiles: 8,
      totalFiles: 12,
      lastWorkMs: T_NEWEST.getTime(),
      openChanges: 1,
    },
    {
      projectHash: OTHER_PROJECT_HASH,
      project: OTHER_PROJECT,
      sessions: 1,
      recordedFiles: 2,
      totalFiles: 4,
      lastWorkMs: T_OLDER.getTime(),
      openChanges: 0,
    },
  ],
  selection: { active: false, hiddenProjects: 0, hiddenSessions: 0 },
};

export const REVIEW: ReviewListPayload = {
  projectHash: PROJECT_HASH,
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
      lastWorkMs: T_NEWEST.getTime(),
      merged: false,
    },
    {
      branch: 'feat/already-merged',
      aheadCount: 0,
      behindCount: 0,
      filesChanged: 22,
      sessionCount: 4,
      taskCount: 35,
      newEdges: 5,
      removedEdges: 0,
      violations: 0,
      merged: true,
      mergedAtMs: T_OLDER.getTime(),
    },
  ],
  recentCommits: [],
  sessions: [],
  rewrittenCommits: [],
};
