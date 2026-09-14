// Standalone test for the license classifier — the copyleft guard's brain: it
// decides which npm licenses may ship (PERMISSIVE), which ship with a
// source-availability offer (WEAK), and which fail the build (STRONG / UNKNOWN).
// Run: node scripts/gen-web-notices.test.mjs  (wired into the notices CI gate).
// Follows the repo's node-script test convention (scripts/*.mutations.mjs), not
// vitest (whose include globs cover src/ only). Cases live in a fixture.
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { join, dirname } from 'node:path';
import { classify } from './gen-web-notices.mjs';

const here = dirname(fileURLToPath(import.meta.url));
const cases = JSON.parse(
  readFileSync(join(here, 'testdata', 'license-classification-cases.json'), 'utf8'),
);

let failures = 0;
for (const { spdx, bucket } of cases) {
  try {
    assert.equal(classify(spdx), bucket, `${JSON.stringify(spdx)} → expected ${bucket}, got ${classify(spdx)}`);
  } catch (e) {
    failures += 1;
    console.error('FAIL:', e.message);
  }
}

// Guard contract: only PERMISSIVE and WEAK are shippable; STRONG and UNKNOWN
// block the build. Assert the buckets that must fail actually fail.
assert.equal(classify('GPL-3.0-only'), 'STRONG');
assert.equal(classify('AGPL-3.0-or-later'), 'STRONG');
assert.equal(classify('UNLICENSED'), 'UNKNOWN');
assert.ok(['PERMISSIVE', 'WEAK'].includes(classify('MIT')));
assert.ok(['PERMISSIVE', 'WEAK'].includes(classify('LGPL-3.0-or-later')));

if (failures > 0) {
  console.error(`gen-web-notices.test: ${failures} classification case(s) failed`);
  process.exit(1);
}
console.log(`gen-web-notices.test: ${cases.length} classification cases passed`);
