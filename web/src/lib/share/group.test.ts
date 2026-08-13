import { describe, it, expect } from 'vitest';
import { groupByProject, isSelectable } from './group';
import type { ShareSession } from './types';

function session(over: Partial<ShareSession> & { id: string }): ShareSession {
  return {
    provider: 'claude-code',
    projectName: 'proj',
    projectHash: 'ph',
    hostSlug: 'host',
    startTime: '2026-02-20T09:00:00Z',
    durationMins: 10,
    totalTokens: 1000,
    turnCount: 5,
    model: 'claude-sonnet-4-6',
    shareStatus: 'new',
    preview: 'Test preview message',
    ...over,
  };
}

describe('groupByProject', () => {
  it('groups by projectHash and aggregates rollups, tokens, and date range', () => {
    const sessions = [
      session({ id: 'a', projectHash: 'h1', projectName: 'one', totalTokens: 100, startTime: '2026-02-20T09:00:00Z', shareStatus: 'new' }),
      session({ id: 'b', projectHash: 'h1', projectName: 'one', totalTokens: 300, startTime: '2026-02-22T09:00:00Z', shareStatus: 'shared' }),
      session({ id: 'c', projectHash: 'h2', projectName: 'two', totalTokens: 50, startTime: '2026-02-21T09:00:00Z', shareStatus: 'updated' }),
    ];

    const projects = groupByProject(sessions);

    expect(projects).toHaveLength(2);

    const one = projects.find((p) => p.key === 'h1')!;
    expect(one.sessionCount).toBe(2);
    expect(one.selectableCount).toBe(1); // only the 'new' one
    expect(one.totalTokens).toBe(400);
    expect(one.dateRange).toEqual({
      start: '2026-02-20T09:00:00Z',
      end: '2026-02-22T09:00:00Z',
    });
    expect(one.statusRollup.new).toBe(1);
    expect(one.statusRollup.shared).toBe(1);
  });

  it('fails safely when canonical project identity is missing', () => {
    expect(() => groupByProject([session({ id: 'a', projectHash: '' })])).toThrow(/projectHash is empty/);
  });

  it('orders projects by most recent activity first', () => {
    const projects = groupByProject([
      session({ id: 'old', projectHash: 'old', startTime: '2026-01-01T00:00:00Z' }),
      session({ id: 'new', projectHash: 'new', startTime: '2026-03-01T00:00:00Z' }),
    ]);
    expect(projects.map((p) => p.key)).toEqual(['new', 'old']);
  });
});

describe('isSelectable', () => {
  it('is true only for new and updated', () => {
    expect(isSelectable(session({ id: '1', shareStatus: 'new' }))).toBe(true);
    expect(isSelectable(session({ id: '2', shareStatus: 'updated' }))).toBe(true);
    expect(isSelectable(session({ id: '3', shareStatus: 'shared' }))).toBe(false);
    expect(isSelectable(session({ id: '4', shareStatus: 'held' }))).toBe(false);
  });
});
