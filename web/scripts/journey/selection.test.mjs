import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { parse } from 'yaml';
import { selectionRecord } from './lib/selection.mjs';

const f = parse(readFileSync(new URL('./testdata/selection.yaml', import.meta.url), 'utf8'));
assert.deepEqual(new Set(f.cases.map(c => c.name)), new Set(f.requiredNames));
for (const row of f.cases) test(row.name, () => {
  const record = () => selectionRecord({ counterfactual: row.counterfactual === true, args: row.args });
  if (row.error) { assert.throws(record, new RegExp(row.error)); return; }
  const selection = record();
  assert.deepEqual(selection.requestedSelection.arguments, row.args);
  assert.deepEqual(selection.requestedSelection.forwardedArguments, row.forwarded);
  assert.equal(selection.fullCatalogRequested, row.fullCatalog === true);
});
