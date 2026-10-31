import { describe, expect, it } from 'vitest';
import type { LocalSyncSummary } from '@/lib/api/publications';
import { ALPHA_HASH } from '@/app/review/[[...segments]]/test-fixtures';
import { visibleRootRows } from './publishState';

function syncRow(id: string): LocalSyncSummary {
  return {
    id,
    harness: 'claude-code',
    projectName: 'alpha-project',
    projectHash: ALPHA_HASH,
    hostSlug: 'fixture-host',
    startTime: '2026-06-01T10:00:00Z',
    durationMs: BigInt(60_000),
    totalTokens: 10,
    turnCount: 2,
    model: 'fixture-model',
    syncStatus: 'new',
    previouslyPushed: false,
  };
}

describe('visibleRootRows', () => {
  it('keeps a stored row whose publication marks it outside the saved selection', () => {
    const rows = visibleRootRows(
      [syncRow('stored')],
      [{ sessionId: 'stored', state: 'unpublished', autoPublish: false, outsideSelection: true }],
    );
    expect(rows.map((row) => row.sync.id)).toEqual(['stored']);
    expect(rows[0].publication.outsideSelection).toBe(true);
  });

  it('leaves out a sync row with no publication record, because its publish state is unknown', () => {
    const rows = visibleRootRows([syncRow('stored'), syncRow('unread')], [
      { sessionId: 'stored', state: 'unpublished', autoPublish: false, outsideSelection: false },
    ]);
    expect(rows.map((row) => row.sync.id)).toEqual(['stored']);
  });
});
