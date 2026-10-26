// @vitest-environment node
import { afterAll, beforeAll, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { parse } from 'yaml';
import * as contract from '@peasant-labs/schema';
import { startVillageFixture } from './lib/village-fixture.mjs';
const f = parse(readFileSync(new URL('./testdata/remote.yaml', import.meta.url), 'utf8'));
const exported = JSON.parse(readFileSync(process.env.PEASANT_FIXTURE_EXPORT, 'utf8'));
let fixture;
beforeAll(async () => { fixture = await startVillageFixture(exported, contract, { token: 'test-owner', actions: [], scenarios: ['complete', ...f.cases.map(row => row.scenario).filter(Boolean)], run() { throw new Error('unused control'); } }); });
afterAll(async () => { await fixture?.close(); });
it('retains exact named remote dependency cases', () => expect(new Set(f.cases.map(c => c.name))).toEqual(new Set(f.requiredNames)));
for (const row of f.cases) it(row.name, async () => {
  const scenario = new URL('/__fixture/control', fixture.origin); scenario.search = new URLSearchParams({ action: 'scenario', name: row.scenario ?? 'complete' }).toString();
  expect((await fetch(scenario, { method: 'POST', headers: { 'x-fixture-control': 'test-owner' } })).ok).toBe(true);
  const path = row.path.replace('{collective}', exported.fixtures.CollectiveID).replace('{transcript}', exported.fixtures.TranscriptID);
  const response = await fetch(`${fixture.origin}${path}`, { method: row.method ?? 'GET', headers: { authorization: 'Bearer disposable-observations-fixture' } });
  const body = await response.text();
  expect(response.status, body).toBe(row.status);
  if (row.name === 'pull-content') expect(response.headers.get('etag')).toBe(`"${exported.metadata.contentHash}"`);
});
