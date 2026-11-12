import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import { parse } from 'yaml';
import { groupedSyncPayload } from '../../../scripts/visual/grouped-sync-payload.mjs';
import { zLocalSessionListPayload } from '@peasant-labs/schema';
import { decodeGroupedSyncList } from './grouped-sync';

// The visual scripts answer the share chooser's list route with this builder.
// It is decoded here by the decoder the chooser uses, on the fixture those
// scripts feed it, so a contract change that the builder misses fails a test
// and not a mounted capture.
const fixture = parse(readFileSync(join(process.cwd(), 'scripts/visual/testdata/search-share.yaml'), 'utf8'));

describe('the grouped sync list the visual scripts serve', () => {
  it('is decoded by the share chooser and carries only fields of the contract', () => {
    const payload = groupedSyncPayload(fixture.sessions);
    const decoded = decodeGroupedSyncList(payload);
    expect(decoded.items.map((item) => item.transcript?.session.id)).toEqual(fixture.sessions.map((session: { id: string }) => session.id));
    expect(decoded.total).toBe(fixture.sessions.length);
    expect(Object.keys(payload).sort()).toEqual(Object.keys(zLocalSessionListPayload.shape).sort());
  });
});
