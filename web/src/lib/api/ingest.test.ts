import { afterEach, describe, expect, it, vi } from 'vitest';
import { reindexSession, unreadableAfterReindex } from './ingest';

function answer(body: unknown) {
  vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } })));
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('unreadableAfterReindex', () => {
  it('keeps the lines the last re-index could not read, and forgets them after one that reads everything', async () => {
    const reason = 'capture JSONL record 13: malformed complete JSON';
    answer({ status: 'done', notes: [reason], unreadable: [{ line: 13, reason }] });
    await reindexSession('sess-unreadable');
    expect(unreadableAfterReindex('sess-unreadable')).toEqual([{ line: 13, reason }]);
    expect(unreadableAfterReindex('sess-other')).toEqual([]);

    answer({ status: 'done', notes: [], unreadable: [] });
    await reindexSession('sess-unreadable');
    expect(unreadableAfterReindex('sess-unreadable')).toEqual([]);
  });
});
