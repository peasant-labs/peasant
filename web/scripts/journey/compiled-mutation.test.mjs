import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { parse } from 'yaml';
import { mutateCompiledAsset } from './lib/compiled-mutation.mjs';
const fixtures = parse(readFileSync(new URL('./testdata/compiled-mutation.yaml', import.meta.url), 'utf8'));
const mutations = parse(readFileSync(new URL('./testdata/collective-mutations.yaml', import.meta.url), 'utf8'));
test('compiled mutation fixtures retain their named controls', () => assert.deepEqual(new Set(fixtures.cases.map(row => row.name)), new Set(fixtures.requiredNames)));
for (const fixture of fixtures.cases) test(fixture.name, () => {
  const mutation = mutations.cases.find(row => row.name === (fixture.mutation ?? 'typed-partial-report-discarded'));
  assert.ok(mutation);
  if (fixture.error) { assert.throws(() => mutateCompiledAsset(fixture.source, mutation), new RegExp(fixture.error)); return; }
  const changed = mutateCompiledAsset(fixture.source, mutation);
  if (fixture.missing) { assert.equal(changed, null); return; }
  assert.notEqual(changed, fixture.source);
  assert.ok(changed.includes(fixture.guard));
});
