import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { resolve } from 'node:path';
import { spawnSync } from 'node:child_process';

const casesPath = resolve('scripts/visual/testdata/transcript-segments.yaml');
const testFile = 'src/components/session-detail/v2/SessionDetailV2.segments.test.tsx';
const needle = 'servePreBoundInline: false';
const failedName = 'per-field-bound';
const failurePattern = /per-field bound/;

function runFile(env, label) {
  const reportPath = `/tmp/peasant-segment-mutation-${process.pid}-${Math.random().toString(36).slice(2)}.json`;
  const result = spawnSync(process.execPath, [
    resolve('node_modules/vitest/vitest.mjs'), 'run', testFile,
    '--reporter=json', `--outputFile=${reportPath}`,
  ], { cwd: process.cwd(), encoding: 'utf8', maxBuffer: 32 * 1024 * 1024, env: { ...process.env, ...env } });
  const output = `${result.stdout}\n${result.stderr}`;
  if (result.error || result.signal || result.status === null) {
    throw new Error(`${label}: the segment host test did not finish: ${result.error?.message ?? output.trim()}`);
  }
  let report;
  try { report = JSON.parse(readFileSync(reportPath, 'utf8')); }
  catch (error) { throw new Error(`${label}: Vitest wrote no assertion inventory: ${error instanceof Error ? error.message : String(error)}`); }
  finally { rmSync(reportPath, { force: true }); }
  const assertions = Array.isArray(report?.testResults)
    ? report.testResults.flatMap((suite) => Array.isArray(suite.assertionResults) ? suite.assertionResults : [])
    : [];
  if (assertions.length === 0) throw new Error(`${label}: the segment host test reported no assertions:\n${output.trim()}`);
  return { status: result.status, assertions };
}

const source = readFileSync(casesPath, 'utf8');
if (source.split(needle).length - 1 !== 1) {
  throw new Error('the committed host fixture must contain servePreBoundInline: false exactly once');
}

const baseline = runFile({}, 'baseline');
if (baseline.status !== 0 || baseline.assertions.some((assertion) => assertion.status !== 'passed')) {
  throw new Error(`baseline must pass before the pre-bound mutation runs; received ${JSON.stringify(baseline.assertions.map((assertion) => [assertion.title, assertion.status]))}`);
}
if (!baseline.assertions.some((assertion) => assertion.title === failedName)) {
  throw new Error('baseline inventory has no per-field bound case for the mutation to redden');
}

const dir = mkdtempSync(resolve(tmpdir(), 'peasant-segment-mutation-'));
const mutatedPath = resolve(dir, 'transcript-segments.yaml');
try {
  writeFileSync(mutatedPath, source.replace(needle, 'servePreBoundInline: true'));
  const mutated = runFile({ PEASANT_TRANSCRIPT_SEGMENTS_CASES: mutatedPath }, 'servePreBoundInline');
  if (mutated.assertions.length !== baseline.assertions.length) {
    throw new Error(`servePreBoundInline: expected the full ${baseline.assertions.length}-test inventory, received ${mutated.assertions.length}`);
  }
  const failed = mutated.assertions.filter((assertion) => assertion.status === 'failed').map((assertion) => assertion.title).sort();
  if (JSON.stringify(failed) !== JSON.stringify([failedName])) {
    throw new Error(`servePreBoundInline: expected only ${failedName} to fail, received [${failed.join(', ')}]`);
  }
  const passed = mutated.assertions.filter((assertion) => assertion.status === 'passed').map((assertion) => assertion.title).sort();
  const expectedPassed = baseline.assertions.map((assertion) => assertion.title).filter((title) => title !== failedName).sort();
  if (JSON.stringify(passed) !== JSON.stringify(expectedPassed)) {
    throw new Error(`servePreBoundInline: every other case must still pass; received [${passed.join(', ')}]`);
  }
  const failure = mutated.assertions
    .filter((assertion) => assertion.status === 'failed')
    .map((assertion) => (Array.isArray(assertion.failureMessages) ? assertion.failureMessages.join('\n') : ''))
    .join('\n');
  if (!failurePattern.test(failure)) {
    throw new Error(`servePreBoundInline: the bound case failed for the wrong reason: ${failure}`);
  }
} finally {
  rmSync(dir, { recursive: true, force: true });
}

const jumpFind = 'await loadListed(descriptor, signal);';
const jumpHost = readFileSync(resolve('src/components/session-detail/v2/lib/useTranscriptSegments.ts'), 'utf8');
if (jumpHost.split(jumpFind).length - 1 !== 1) {
  throw new Error('the jump load must contain its mutation anchor exactly once');
}
const jumpFailed = ['jump-window', 'load-more-prepends', 'repeat-jump', 'revision-refetch'];
const jumpMutation = {
  name: 'jump path fetches an extra segment',
  target: 'useTranscriptSegments.ts',
  find: jumpFind,
  replace: `${jumpFind}\n    await loadListed({ ...descriptor, segments: [...descriptor.segments, descriptor.segments.includes(0) ? 1 : 0] }, signal);`,
};
const jumped = runFile({ PEASANT_SOURCE_MUTATION_JSON: JSON.stringify(jumpMutation) }, jumpMutation.name);
if (jumped.assertions.length !== baseline.assertions.length) {
  throw new Error(`${jumpMutation.name}: expected the full ${baseline.assertions.length}-test inventory, received ${jumped.assertions.length}`);
}
const jumpFailures = jumped.assertions.filter((assertion) => assertion.status === 'failed').map((assertion) => assertion.title).sort();
if (JSON.stringify(jumpFailures) !== JSON.stringify([...jumpFailed].sort())) {
  throw new Error(`${jumpMutation.name}: expected [${jumpFailed.join(', ')}] to fail, received [${jumpFailures.join(', ')}]`);
}
const jumpText = jumped.assertions
  .filter((assertion) => assertion.title === 'jump-window')
  .map((assertion) => (Array.isArray(assertion.failureMessages) ? assertion.failureMessages.join('\n') : ''))
  .join('\n');
if (!/jump-window mounted bound/.test(jumpText)) {
  throw new Error(`${jumpMutation.name}: the jump case failed for the wrong reason: ${jumpText}`);
}

console.log(`transcript-segments mutations: servePreBoundInline reddened only ${failedName}; extra jump fetch reddened ${jumpFailed.join(', ')} in a ${baseline.assertions.length}-test inventory`);
