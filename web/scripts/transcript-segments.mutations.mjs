import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { resolve } from 'node:path';
import { spawnSync } from 'node:child_process';

const casesPath = resolve('scripts/visual/testdata/transcript-segments.yaml');
const testFile = 'src/components/session-detail/v2/SessionDetailV2.segments.test.tsx';
const needle = 'servePreBoundInline: false';
const failedName = 'per-field-bound';
// The earlier-history limitation case mounts the same bounded turn and reads
// its truncation notice, so serving the pre-bound body reddens it as well.
// Each case must fail on its own missing truncation notice.
const failureReasons = {
  [failedName]: /per-field bound/,
  'truncated-turn-limitation': /this turn.s text is truncated/,
};
const failedNames = Object.keys(failureReasons).sort();

function runFile(env, label, file = testFile) {
  const reportPath = `/tmp/peasant-segment-mutation-${process.pid}-${Math.random().toString(36).slice(2)}.json`;
  const result = spawnSync(process.execPath, [
    resolve('node_modules/vitest/vitest.mjs'), 'run', file,
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
  if (JSON.stringify(failed) !== JSON.stringify(failedNames)) {
    throw new Error(`servePreBoundInline: expected only [${failedNames.join(', ')}] to fail, received [${failed.join(', ')}]`);
  }
  const passed = mutated.assertions.filter((assertion) => assertion.status === 'passed').map((assertion) => assertion.title).sort();
  const expectedPassed = baseline.assertions.map((assertion) => assertion.title).filter((title) => !failedNames.includes(title)).sort();
  if (JSON.stringify(passed) !== JSON.stringify(expectedPassed)) {
    throw new Error(`servePreBoundInline: every other case must still pass; received [${passed.join(', ')}]`);
  }
  for (const [name, reason] of Object.entries(failureReasons)) {
    const failure = mutated.assertions
      .filter((assertion) => assertion.status === 'failed' && assertion.title === name)
      .map((assertion) => (Array.isArray(assertion.failureMessages) ? assertion.failureMessages.join('\n') : ''))
      .join('\n');
    if (!reason.test(failure)) {
      throw new Error(`servePreBoundInline: ${name} failed for the wrong reason; expected ${reason}, received: ${failure}`);
    }
  }
} finally {
  rmSync(dir, { recursive: true, force: true });
}

const jumpFind = 'await loadListed(descriptor, signal);';
const jumpHost = readFileSync(resolve('src/components/session-detail/v2/lib/useTranscriptSegments.ts'), 'utf8');
if (jumpHost.split(jumpFind).length - 1 !== 1) {
  throw new Error('the jump load must contain its mutation anchor exactly once');
}
const jumpFailed = ['jump-window', 'load-more-prepends', 'positions-mounted', 'repeat-jump', 'revision-refetch'];
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

// The directory parser's conversation turn positions. Each mutation must redden
// exactly the fixture rows that state the rule it breaks, for the stated reason.
const parserTest = 'src/components/session-detail/v2/lib/transcriptSegments.test.ts';
const parserTarget = 'src/components/session-detail/v2/lib/transcriptSegments.ts';
const parserSource = readFileSync(resolve(parserTarget), 'utf8');
const parserBaseline = runFile({}, 'directory parser baseline', parserTest);
if (parserBaseline.status !== 0 || parserBaseline.assertions.some((assertion) => assertion.status !== 'passed')) {
  throw new Error(`the directory parser test must pass before its mutations run; received ${JSON.stringify(parserBaseline.assertions.map((assertion) => [assertion.title, assertion.status]))}`);
}
const parserMutations = [
  {
    name: 'a malformed span is kept',
    find: 'const span = zConversationTurnSpan.safeParse(rawSpan);',
    replace: 'const span = { success: true, data: rawSpan };',
    failed: [
      'a fractional count drops every position field and warns once',
      'a null span drops every position field and warns once',
      'a span missing its count drops every position field and warns once',
      'malformed span drops every position field and warns once',
    ],
    reason: /to strictly equal|toStrictEqual/,
  },
  {
    name: 'the dropped positions are not reported',
    find: 'console.warn(',
    replace: 'String(',
    failed: [
      'a fractional count drops every position field and warns once',
      'a null total drops every position field and warns once',
      'a null span drops every position field and warns once',
      'a span missing its count drops every position field and warns once',
      'malformed span drops every position field and warns once',
      'malformed total drops every position field and warns once',
      'two malformed fields warn once',
    ],
    reason: /to be called 1 times, but got 0 times/,
  },
  {
    name: 'an absent total is treated as malformed',
    find: 'if (rawTotal !== undefined) {',
    replace: '{',
    failed: [
      'a span beside no total is kept',
      'fields absent',
    ],
    // A directory with no position field at all parses to the same shape once
    // stripped, so that row is caught by its unexpected warning.
    reason: /to strictly equal|toStrictEqual|to not be called at all/,
  },
  {
    name: 'a malformed field drops only itself',
    find: 'return { sessionId: value.sessionId, revision, totalTurns, totalSegments, segments: refs };',
    replace: 'return { sessionId: value.sessionId, revision, totalTurns, totalSegments, segments: value.segments.map((raw, at) => { const kept = zConversationTurnSpan.safeParse(raw?.conversationTurns); return kept.success ? { ...refs[at], conversationTurns: kept.data } : refs[at]; }) };',
    failed: [
      'a fractional count drops every position field and warns once',
      'a null total drops every position field and warns once',
      'a null span drops every position field and warns once',
      'a span missing its count drops every position field and warns once',
      'malformed span drops every position field and warns once',
      'malformed total drops every position field and warns once',
      'two malformed fields warn once',
    ],
    reason: /to strictly equal|toStrictEqual/,
  },
];
for (const mutation of parserMutations) {
  if (parserSource.split(mutation.find).length - 1 !== 1) {
    throw new Error(`${mutation.name}: the directory parser must contain its mutation anchor exactly once: ${mutation.find}`);
  }
  const mutated = runFile({
    PEASANT_SOURCE_MUTATION_JSON: JSON.stringify({ name: mutation.name, target: parserTarget, find: mutation.find, replace: mutation.replace }),
  }, mutation.name, parserTest);
  if (mutated.assertions.length !== parserBaseline.assertions.length) {
    throw new Error(`${mutation.name}: expected the full ${parserBaseline.assertions.length}-test inventory, received ${mutated.assertions.length}`);
  }
  const failures = mutated.assertions.filter((assertion) => assertion.status === 'failed');
  const failedTitles = failures.map((assertion) => assertion.title).sort();
  if (JSON.stringify(failedTitles) !== JSON.stringify([...mutation.failed].sort())) {
    throw new Error(`${mutation.name}: expected [${mutation.failed.join(', ')}] to fail, received [${failedTitles.join(', ')}]`);
  }
  for (const failure of failures) {
    const text = Array.isArray(failure.failureMessages) ? failure.failureMessages.join('\n') : '';
    if (!mutation.reason.test(text)) {
      throw new Error(`${mutation.name}: ${failure.title} failed for the wrong reason: ${text}`);
    }
  }
}

console.log(`transcript-segments mutations: ${parserMutations.length} directory parser mutations each reddened only their named rows in a ${parserBaseline.assertions.length}-test inventory`);
console.log(`transcript-segments mutations: servePreBoundInline reddened only ${failedNames.join(' and ')}; extra jump fetch reddened ${jumpFailed.join(', ')} in a ${baseline.assertions.length}-test inventory`);
