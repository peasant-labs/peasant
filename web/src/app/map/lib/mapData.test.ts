import { describe, it, expect } from 'vitest';
import {
  MAX_COUPLED_NODES,
  MAX_SEARCH_RESULTS,
  MAX_TOUCHED_MODULES,
  commitAtOrBefore,
  contributeSessionsHref,
  coupledNodes,
  interleaveShapedBy,
  localDayEndMs,
  mapGraphToData,
  mapWireToDatum,
  projectCoverage,
  projectSessions,
  searchMapNodes,
  sessionsPerDay,
  shapedBySessionIds,
  touchedModules,
} from './mapData';
import {
  GRAPH,
  NODE_DETAIL,
  NOW_MS,
  OTHER_PROJECT_HASH,
  PROJECT,
  PROJECT_HASH,
  SESSIONS,
  makeCommitRef,
  makeSession,
  makeTask,
} from './test-fixtures';
import { graphAdapterContractFixture } from '@/test/fixtures/graphAdapterContract';

describe('mapWireToDatum / mapGraphToData (wire → canvas datum)', () => {
  it('maps a wire node field-for-field', () => {
    const wire = GRAPH.nodes[2]; // internal/ingest
    const datum = mapWireToDatum(wire);
    expect(datum).toEqual({
      id: 'internal/ingest',
      parent: 'internal',
      kind: 'package',
      name: 'ingest',
      language: undefined,
      layer: 1,
      order: 0,
      loc: 1200,
      fileCount: 7,
      recordedFiles: 6,
      totalFiles: 7,
      touchCount: 18,
      effortDensity: 0.8,
      agentEditedCount: 0,
      readCount: 0,
      readAttribution: 'unavailable',
      readState: 'none',
      changedRegionCount: 0,
      attributedRegionCount: 0,
      reviewedRegionCount: 0,
    });
  });

  it('normalizes an empty-string parent to undefined', () => {
    const datum = mapWireToDatum({ ...GRAPH.nodes[0], parent: '' });
    expect(datum.parent).toBeUndefined();
  });

  it('retains every non-default comprehension field from the shared contract fixture', () => {
    const datum = mapWireToDatum({ ...GRAPH.nodes[2], ...graphAdapterContractFixture.mapNode });
    expect(datum).toMatchObject(graphAdapterContractFixture.mapNode);
  });

  it('adapts the payload for the canvas: nodes, structure edges, violations — never activity edges', () => {
    const data = mapGraphToData(GRAPH);
    expect(data.nodes).toHaveLength(3);
    expect(data.structureEdges).toEqual([{ from: 'cmd', to: 'internal', count: 2 }]);
    expect(data.violations).toEqual([{ kind: 'cycle', from: 'internal', to: 'cmd' }]);
    // Activity edges feed coupledNodes rather than the structural canvas.
    expect(data).not.toHaveProperty('activityEdges');
  });
});

describe('projectSessions rail list', () => {
  it('filters to the project, reverse-chronological, zero-touch included', () => {
    const rows = projectSessions(SESSIONS, PROJECT_HASH);
    expect(rows.map((s) => s.id)).toEqual(['sess-new', 'sess-old']);
    expect(rows[1].toolCallCount).toBe(0);
  });

  it('does not widen an explicit hash to sessions with the same label', () => {
    const rows = projectSessions([...SESSIONS, makeSession({ id: 'same-label-other-hash', project: PROJECT, projectHash: OTHER_PROJECT_HASH })], PROJECT_HASH);
    expect(rows.map((s) => s.id)).not.toContain('same-label-other-hash');
  });
});

describe('sessionsPerDay (time-strip sparkline)', () => {
  it('produces one entry per day, oldest → newest, zero-filled', () => {
    const days = sessionsPerDay(projectSessions(SESSIONS, PROJECT_HASH), 10, NOW_MS);
    expect(days).toHaveLength(10);
    // Oldest → newest: the last entry is "today" (2026-06-09 local).
    expect(days[days.length - 1].date).toBe('2026-06-09');
    const byDate = new Map(days.map((d) => [d.date, d.sessions]));
    expect(byDate.get('2026-06-08')).toBe(1); // sess-new
    expect(byDate.get('2026-06-01')).toBe(1); // sess-old
    expect(byDate.get('2026-06-09')).toBe(0); // zero-filled
  });

  it('ignores unparseable timestamps instead of throwing', () => {
    const days = sessionsPerDay([makeSession({ startTime: 'not-a-date' })], 3, NOW_MS);
    expect(days.every((d) => d.sessions === 0)).toBe(true);
  });

  // Calendar-day iteration, not fixed 24h steps: local days are 23/25 hours
  // across DST, and millisecond stepping duplicates or skips day keys there.
  // Node honors runtime TZ changes (Node ≥13), so these run in a DST zone
  // regardless of the host's clock settings; the dates are constructed
  // directly from local components.
  describe('across DST transitions (simulated in America/New_York)', () => {
    const withTZ = (tz: string, fn: () => void) => {
      const prev = process.env.TZ;
      process.env.TZ = tz;
      try {
        fn();
      } finally {
        if (prev === undefined) delete process.env.TZ;
        else process.env.TZ = prev;
      }
    };

    it('does not skip the spring-forward day (23h local day)', () => {
      withTZ('America/New_York', () => {
        // US spring-forward: 2026-03-08. Just after midnight on the 9th,
        // nowMs − 24h lands on the 7th — the old fixed-step code skipped
        // 2026-03-08 entirely.
        const now = new Date(2026, 2, 9, 0, 30, 0).getTime();
        const onDstDay = makeSession({
          startTime: new Date(2026, 2, 8, 12, 0, 0).toISOString(),
        });
        const days = sessionsPerDay([onDstDay], 4, now);
        expect(days.map((d) => d.date)).toEqual([
          '2026-03-06',
          '2026-03-07',
          '2026-03-08',
          '2026-03-09',
        ]);
        expect(days[2].sessions).toBe(1);
      });
    });

    it('does not duplicate the fall-back day (25h local day)', () => {
      withTZ('America/New_York', () => {
        // US fall-back: 2026-11-01. Late on the 1st, nowMs − 24h is STILL
        // the 1st — the old fixed-step code emitted a duplicate key
        // (duplicate React keys in the TimeStrip).
        const now = new Date(2026, 10, 1, 23, 30, 0).getTime();
        const days = sessionsPerDay([], 3, now);
        expect(days.map((d) => d.date)).toEqual(['2026-10-30', '2026-10-31', '2026-11-01']);
        expect(new Set(days.map((d) => d.date)).size).toBe(days.length);
      });
    });
  });
});

describe('time scrub helpers', () => {
  it('localDayEndMs returns 23:59:59.999 local for a day key', () => {
    const ms = localDayEndMs('2026-06-08');
    const d = new Date(ms);
    expect(d.getFullYear()).toBe(2026);
    expect(d.getMonth()).toBe(5); // June (0-indexed)
    expect(d.getDate()).toBe(8);
    expect(d.getHours()).toBe(23);
    expect(d.getMinutes()).toBe(59);
    // End-of-day is strictly greater than any instant earlier that day.
    expect(ms).toBeGreaterThan(new Date(2026, 5, 8, 16, 0, 0).getTime());
  });

  it('commitAtOrBefore picks the most recent commit at or before the cutoff', () => {
    // NODE_DETAIL.recentCommits: hotfix @2026-06-08 16:00, fix ingest @2026-06-02 10:00.
    const commits = NODE_DETAIL.recentCommits;
    // Cutoff end of 2026-06-08 → the 06-08 hotfix wins (most recent ≤ cutoff).
    expect(commitAtOrBefore(commits, localDayEndMs('2026-06-08'))?.hash).toBe('ffff111eeee2222');
    // Cutoff end of 2026-06-03 → only the 06-02 commit qualifies.
    expect(commitAtOrBefore(commits, localDayEndMs('2026-06-03'))?.hash).toBe('abc1234deadbee');
  });

  it('commitAtOrBefore returns null when the cutoff predates every commit', () => {
    expect(commitAtOrBefore(NODE_DETAIL.recentCommits, localDayEndMs('2026-01-01'))).toBeNull();
  });

  it('commitAtOrBefore skips commits with no timestamp', () => {
    const commits = [makeCommitRef('x', 'no time', null, [])];
    expect(commitAtOrBefore(commits, Date.now())).toBeNull();
  });
});

describe('projectCoverage (rail coverage line)', () => {
  it('sums recorded/total over ROOT nodes only (no double counting)', () => {
    // cmd 2/2 + internal 6/10; the child package (6/7) must not be added.
    expect(projectCoverage(GRAPH.nodes)).toEqual({ recorded: 8, total: 12 });
  });

  it('is zero for an empty graph', () => {
    expect(projectCoverage([])).toEqual({ recorded: 0, total: 0 });
  });
});

describe('coupledNodes ("Often edited with")', () => {
  it('collects the OTHER endpoint of edges touching the node, sorted by taskCount desc', () => {
    // cmd touches both fixture edges: cmd↔internal (3) and internal/ingest↔cmd (5).
    expect(coupledNodes(GRAPH.activityEdges, 'cmd')).toEqual([
      { id: 'internal/ingest', taskCount: 5 },
      { id: 'internal', taskCount: 3 },
    ]);
    // Direction does not matter — from/to are both matched.
    expect(coupledNodes(GRAPH.activityEdges, 'internal/ingest')).toEqual([
      { id: 'cmd', taskCount: 5 },
    ]);
  });

  it('returns nothing for a node with no co-edit observations', () => {
    expect(coupledNodes(GRAPH.activityEdges, 'web')).toEqual([]);
    expect(coupledNodes([], 'cmd')).toEqual([]);
  });

  it('caps at MAX_COUPLED_NODES, payload order breaking ties', () => {
    const edges = Array.from({ length: 7 }, (_, i) => ({
      from: 'cmd',
      to: `pkg/p${i}`,
      taskCount: 2,
    }));
    const rows = coupledNodes(edges, 'cmd');
    expect(rows).toHaveLength(MAX_COUPLED_NODES);
    expect(rows.map((r) => r.id)).toEqual(['pkg/p0', 'pkg/p1', 'pkg/p2', 'pkg/p3', 'pkg/p4']);
  });
});

describe('interleaveShapedBy node panel', () => {
  it('merges tasks and commits reverse-chronologically', () => {
    const rows = interleaveShapedBy(NODE_DETAIL.shapedBy, NODE_DETAIL.recentCommits);
    expect(
      rows.map((r) => (r.kind === 'task' ? `t:${r.task.entryIndex}` : `c:${r.commit.hash}`)),
    ).toEqual([
      'c:ffff111eeee2222', // 2026-06-08 16:00 — the unrecorded hotfix
      't:12', //              2026-06-08 10:00
      'c:abc1234deadbee', //  2026-06-02
      't:4', //               2026-06-01
    ]);
  });

  it('sinks rows without a timestamp to the bottom', () => {
    const rows = interleaveShapedBy(
      [makeTask({ startMs: undefined, entryIndex: 99 })],
      NODE_DETAIL.recentCommits,
    );
    const last = rows[rows.length - 1];
    expect(last.kind).toBe('task');
  });
});

describe('shapedBySessionIds', () => {
  it('dedupes preserving first-seen order', () => {
    const ids = shapedBySessionIds([
      makeTask({ sessionId: 'a' }),
      makeTask({ sessionId: 'b' }),
      makeTask({ sessionId: 'a' }),
    ]);
    expect(ids).toEqual(['a', 'b']);
  });
});

describe('links (exact href contracts)', () => {
  it('contributeSessionsHref dedupes and comma-joins ids', () => {
    expect(contributeSessionsHref(['a', 'b', 'a'])).toBe('/share?sessions=a,b');
  });
});

describe('touchedModules (task-row touched summary)', () => {
  it('maps files to first-two-segment modules, deduped', () => {
    expect(
      touchedModules([
        'internal/codemap/review.go',
        'internal/codemap/graph.go',
        'web/src/app/page.tsx',
      ]),
    ).toEqual(['internal/codemap', 'web/src']);
  });

  it('ranks modules by edited-file count, first-seen order breaking ties', () => {
    expect(
      touchedModules([
        'web/src/a.ts',
        'internal/ingest/a.go',
        'internal/ingest/b.go',
        'cmd/peasant/main.go',
      ]),
    ).toEqual(['internal/ingest', 'web/src', 'cmd/peasant']);
  });

  it('caps at MAX_TOUCHED_MODULES', () => {
    const files = ['a/x/1.go', 'b/x/1.go', 'c/x/1.go', 'd/x/1.go'];
    expect(touchedModules(files)).toHaveLength(MAX_TOUCHED_MODULES);
    expect(touchedModules(files)).toEqual(['a/x', 'b/x', 'c/x']);
  });

  it('keeps single-segment (root) files as their own module', () => {
    expect(touchedModules(['Makefile', 'go.mod'])).toEqual(['Makefile', 'go.mod']);
  });

  it('is empty for no edited files (and skips empty paths)', () => {
    expect(touchedModules([])).toEqual([]);
    expect(touchedModules([''])).toEqual([]);
  });
});

describe('searchMapNodes toolbar search', () => {
  it('matches node ids and names case-insensitively', () => {
    expect(searchMapNodes(GRAPH.nodes, 'ingest').map((n) => n.id)).toEqual(['internal/ingest']);
    expect(searchMapNodes(GRAPH.nodes, 'INGEST').map((n) => n.id)).toEqual(['internal/ingest']);
    // Id substring: "internal" matches the module and its child package path.
    expect(searchMapNodes(GRAPH.nodes, 'internal').map((n) => n.id)).toEqual([
      'internal',
      'internal/ingest',
    ]);
  });

  it('returns nothing for an empty/whitespace query', () => {
    expect(searchMapNodes(GRAPH.nodes, '')).toEqual([]);
    expect(searchMapNodes(GRAPH.nodes, '   ')).toEqual([]);
  });

  it('returns nothing when no node matches', () => {
    expect(searchMapNodes(GRAPH.nodes, 'zzz-no-such-node')).toEqual([]);
  });

  it('caps results at MAX_SEARCH_RESULTS in payload order', () => {
    const many = Array.from({ length: 12 }, (_, i) => ({
      id: `pkg/file-${i}`,
      name: `file-${i}`,
    }));
    const hits = searchMapNodes(many, 'file');
    expect(hits).toHaveLength(MAX_SEARCH_RESULTS);
    expect(hits[0].id).toBe('pkg/file-0');
    expect(hits[MAX_SEARCH_RESULTS - 1].id).toBe(`pkg/file-${MAX_SEARCH_RESULTS - 1}`);
  });
});
