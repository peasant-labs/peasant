import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import { parse } from 'yaml';
import { groupedSyncPayload } from '../../../scripts/visual/grouped-sync-payload.mjs';
import { zHelperGroupSummary, zLocalSessionListItem, zLocalSessionListPayload, zLocalSessionRow, zLocalSyncSummary, zSessionSummary } from '@peasant-labs/schema';
import { decodeGroupedSyncList } from './grouped-sync';

// search-share-visual.mjs and share-geometry.mjs answer the chooser's list route
// with this builder. Its test lives alongside the chooser's decoder so contract
// drift fails on their shared fixture before either script is run.
const fixture = parse(readFileSync(join(process.cwd(), 'scripts/visual/testdata/search-share.yaml'), 'utf8'));

function expectOnlySchemaKeys(value: object, shape: object, location: string) {
  expect(Object.keys(value).filter((key) => !Object.prototype.hasOwnProperty.call(shape, key)), `${location} has fields outside the schema`).toEqual([]);
}

describe('the grouped sync list the visual scripts serve', () => {
  it('is decoded by the share chooser and carries only fields of the contract', () => {
    const payload = groupedSyncPayload(fixture.sessions);
    const decoded = decodeGroupedSyncList(payload);
    expect(decoded.items.map((item) => item.transcript?.session.id)).toEqual(fixture.sessions.map((session: { id: string }) => session.id));
    expect(decoded.items[0].helperGroups?.[0].groupId).toBe(fixture.sessions[0].helperGroups[0].groupId);
    expect(decoded.total).toBe(fixture.sessions.length);
    expect(Object.keys(payload).sort()).toEqual(Object.keys(zLocalSessionListPayload.shape).sort());
  });

  it('carries only schema fields throughout each grouped item', () => {
    const payload = groupedSyncPayload(fixture.sessions);
    expect(payload.items.some((item: { helperGroups: object[] }) => item.helperGroups.length > 0)).toBe(true);
    for (const item of payload.items) {
      expectOnlySchemaKeys(item, zLocalSessionListItem.shape, 'item');
      expectOnlySchemaKeys(item.transcript, zLocalSessionRow.shape, 'item.transcript');
      expectOnlySchemaKeys(item.transcript.session, zSessionSummary.shape, 'item.transcript.session');
      expectOnlySchemaKeys(item.transcript.sync, zLocalSyncSummary.shape, 'item.transcript.sync');
      for (const group of item.helperGroups) expectOnlySchemaKeys(group, zHelperGroupSummary.shape, 'item.helperGroups[]');
    }
  });
});
