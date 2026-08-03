import { describe, it, expect } from 'vitest';
import type { ChangeSummary } from '@peasant-labs/schema';
import type { DecodedChangeDetailPayload } from '@/lib/api/map';
type ChangeSession = DecodedChangeDetailPayload['work'][number];
import {
  filesWithConversations,
  groupChangedFiles,
  openChangeLifecycle,
  summarizeChange,
} from './signals';

const NOW = new Date(2026, 5, 14, 12, 0, 0).getTime();
const dayAgo = (n: number) => NOW - n * 86_400_000;

function change(over: Partial<ChangeSummary>): ChangeSummary {
  return {
    branch: 'feat/x',
    aheadCount: 3,
    behindCount: 0,
    filesChanged: 5,
    sessionCount: 1,
    taskCount: 2,
    newEdges: 0,
    removedEdges: 0,
    violations: 0,
    merged: false,
    ...over,
  };
}

function session(tasks: ChangeSession['tasks']): ChangeSession {
  return { sessionId: 's', title: 't', harness: 'claude-code', binding: 'bound', tasks };
}

function task(over: Partial<ChangeSession['tasks'][number]>) {
  return {
    sessionId: 's',
    entryIndex: 1,
    title: 'do x',
    editedFiles: [],
    readCount: 0,
    readFiles: [],
    retryLoop: false,
    labels: [],
    ...over,
  };
}

describe('summarizeChange signal band', () => {
  it('sums sessions, tasks, retry loops, violations, and last activity', () => {
    const payload = {
      work: [
        session([task({ startMs: dayAgo(2), retryLoop: true }), task({ startMs: dayAgo(5) })]),
        session([task({ startMs: dayAgo(1) })]),
      ],
      violations: [
        { kind: 'cycle', from: 'a', to: 'b' },
        { kind: 'wrong_way', from: 'c', to: 'd' },
      ],
    } as unknown as DecodedChangeDetailPayload;

    expect(summarizeChange(payload)).toEqual({
      sessions: 2,
      tasks: 3,
      retryLoops: 1,
      violations: 2,
      lastActivityMs: dayAgo(1),
    });
  });

  it('reports null last activity when no task is dated', () => {
    const payload = {
      work: [session([task({ startMs: undefined })])],
      violations: [],
    } as unknown as DecodedChangeDetailPayload;
    expect(summarizeChange(payload).lastActivityMs).toBeNull();
    expect(summarizeChange(payload).retryLoops).toBe(0);
  });
});

describe('filesWithConversations / groupChangedFiles (file-first)', () => {
  const payload = {
    files: [
      { path: 'internal/ingest/a.go', status: 'M' },
      { path: 'internal/api/b.go', status: 'A' },
      { path: 'README.md', status: 'M' },
    ],
    work: [session([task({ editedFiles: ['internal/ingest/a.go'] })])],
  } as unknown as DecodedChangeDetailPayload;

  it('pairs each file with the conversations that edited its EXACT path', () => {
    const fwc = filesWithConversations(payload);
    expect(fwc.find((f) => f.file.path === 'internal/ingest/a.go')!.sessions).toHaveLength(1);
    expect(fwc.find((f) => f.file.path === 'internal/api/b.go')!.sessions).toHaveLength(0);
    expect(fwc.find((f) => f.file.path === 'README.md')!.sessions).toHaveLength(0);
  });

  it('groups by first-two-segment module; root files under (root); ties sort by name', () => {
    const groups = groupChangedFiles(payload);
    // Each module has 1 file → tie on count → name ascending.
    expect(groups.map((g) => g.dir)).toEqual(['(root)', 'internal/api', 'internal/ingest']);
    expect(groups.find((g) => g.dir === 'internal/ingest')!.files).toHaveLength(1);
  });

  it('orders groups by file count (descending) before name', () => {
    const many = {
      files: [
        { path: 'internal/ingest/a.go', status: 'M' },
        { path: 'internal/ingest/b.go', status: 'M' },
        { path: 'web/src/x.ts', status: 'M' },
      ],
      work: [],
    } as unknown as DecodedChangeDetailPayload;
    expect(groupChangedFiles(many).map((g) => g.dir)).toEqual(['internal/ingest', 'web/src']);
  });
});

describe('openChangeLifecycle', () => {
  it('classifies by recency of last work', () => {
    expect(openChangeLifecycle(change({ lastWorkMs: dayAgo(1) }), NOW)?.key).toBe('active');
    expect(openChangeLifecycle(change({ lastWorkMs: dayAgo(7) }), NOW)?.key).toBe('idle');
    expect(openChangeLifecycle(change({ lastWorkMs: dayAgo(40) }), NOW)?.key).toBe('stale');
  });

  it('falls back to the tip commit time when last work is unknown', () => {
    expect(
      openChangeLifecycle(change({ lastWorkMs: undefined, tipCommitMs: dayAgo(2) }), NOW)?.key,
    ).toBe('active');
  });

  it('returns null when the change has no timestamp at all', () => {
    expect(openChangeLifecycle(change({ lastWorkMs: undefined, tipCommitMs: undefined }), NOW)).toBeNull();
  });
});
