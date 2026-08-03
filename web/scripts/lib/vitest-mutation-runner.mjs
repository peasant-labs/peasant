import { existsSync, readFileSync, unlinkSync } from 'node:fs';
import { resolve } from 'node:path';
import { spawnSync } from 'node:child_process';

/**
 * Shared "baseline capture -> mutate -> compare" core for this repo's
 * isolated-source-mutation gates (web/scripts/contract-boundary.mutations.mjs,
 * web/scripts/transcript-position.mutations.mjs). Both runners drive Vitest
 * against a real production file mutated in-memory by the matching
 * `web/vitest.config.ts` transform plugin (matched by env-var name, see
 * `envKey` below), and both need the exact same structural proof: the WHOLE
 * test file runs to completion (never `-t`-filtered to one test), a clean
 * unmutated baseline is captured first, and each mutant must reproduce the
 * baseline's full test count with exactly its declared failing-test set and
 * every other baseline test still passing.
 *
 * This module is per-repo, not cross-repo: fairtrade's
 * provider-harnesses.mutations.mjs mutates a compiled bundle and drives a
 * bespoke assertion-counter script rather than Vitest, so it is a
 * structurally different mechanism and does not belong here.
 */

const SETUP_FAILURE_PATTERN = /mutation anchor must occur exactly once|failed to load config/i;

/** Verify `value` has exactly the fields in `expected` -- no more, no fewer. */
export function exactFields(value, expected, label) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error(`${label} must be an object`);
  const keys = Object.keys(value);
  const unknown = keys.filter((field) => !expected.includes(field));
  const missing = expected.filter((field) => !(field in value));
  if (unknown.length || missing.length) throw new Error(`${label} fields are invalid; unknown=${unknown.join(',')} missing=${missing.join(',')}`);
}

/** Verify `find` occurs exactly once in `source` before it is safe to mutate. */
export function requireSingleOccurrence(source, find, label) {
  const occurrences = source.split(find).length - 1;
  if (occurrences !== 1) throw new Error(`${label}: mutation target must occur exactly once, received ${occurrences}`);
}

/**
 * Run one Vitest test file to completion under the JSON reporter, WITHOUT a
 * `-t` name filter, so every test in the file always executes (never just
 * the one a mutation is meant to break). The JSON report is the sole source
 * of truth for which tests ran and which passed or failed; combined
 * stdout/stderr text is never grepped for a bare substring.
 */
export function runTestFile(testFile, env, label) {
  const reportPath = `/tmp/peasant-mutation-runner-${process.pid}-${Math.random().toString(36).slice(2)}.json`;
  const result = spawnSync(process.execPath, [
    resolve('node_modules/vitest/vitest.mjs'), 'run', testFile,
    '--reporter=json', `--outputFile=${reportPath}`,
  ], { cwd: process.cwd(), encoding: 'utf8', maxBuffer: 32 * 1024 * 1024, env: { ...process.env, ...env } });
  const output = `${result.stdout}\n${result.stderr}`;
  if (result.error || result.signal || result.status === null || SETUP_FAILURE_PATTERN.test(output)) throw new Error(`${label}: setup failed before any assertion ran: ${result.error?.message ?? output.trim()}`);
  if (!existsSync(reportPath)) throw new Error(`${label}: Vitest JSON reporter did not produce an assertion inventory`);
  let report;
  try { report = JSON.parse(readFileSync(reportPath, 'utf8')); }
  catch (error) { throw new Error(`${label}: Vitest JSON reporter produced malformed JSON: ${error instanceof Error ? error.message : String(error)}`); }
  finally { unlinkSync(reportPath); }
  const assertions = Array.isArray(report?.testResults)
    ? report.testResults.flatMap((suite) => Array.isArray(suite.assertionResults) ? suite.assertionResults.map((assertion) => ({ suite, assertion })) : [])
    : [];
  return { status: result.status, assertions };
}

/**
 * Setup control: prove each affected test file is clean, unfiltered and in
 * full, against the real unmutated production source BEFORE trusting any
 * mutant. This rules out a pre-existing failure masquerading as a
 * mutation's designated failure and establishes the exact total test
 * inventory every mutant run against that file must match.
 */
export function captureBaselines(testFiles) {
  const baselines = new Map();
  for (const testFile of testFiles) {
    const { status, assertions } = runTestFile(testFile, {}, `baseline (${testFile})`);
    if (status !== 0 || assertions.length === 0 || assertions.some(({ assertion }) => assertion.status !== 'passed')) {
      throw new Error(`baseline ${testFile} must pass cleanly with at least one assertion before any mutation runs; received ${JSON.stringify(assertions.map(({ assertion }) => [assertion.title, assertion.status]))}`);
    }
    baselines.set(testFile, { assertions, titles: new Set(assertions.map(({ assertion }) => assertion.title)) });
  }
  return baselines;
}

/**
 * Verify one mutation's mutated run against its file's captured baseline:
 * the full baseline test count ran (nothing silently skipped), the exact
 * SET of failed titles equals the mutation's declared
 * `expectedFailedTestNames`, every other baseline title is still 'passed',
 * and the failure text matches `expectedFailurePattern`.
 *
 * Diagnostic-pattern matching is deliberately CASE-SENSITIVE (no `i` flag):
 * every `expectedFailurePattern` in these manifests is authored in the same
 * change as the production error/assertion message it is meant to match, so
 * there is no legitimate source of case drift to tolerate here -- a
 * case-insensitive match would silently accept an accidental capitalization
 * change to either side of that pairing as still "the same" diagnostic,
 * which is exactly the kind of drift this hardening exists to catch.
 */
export function verifyMutation({ mutation, envKey, baselines, label = mutation.name }) {
  const baseline = baselines.get(mutation.expectedTestFile);
  if (!baseline) throw new Error(`${label}: no captured baseline for ${mutation.expectedTestFile}`);
  const missing = mutation.expectedFailedTestNames.filter((name) => !baseline.titles.has(name));
  if (missing.length > 0) throw new Error(`${label}: expectedFailedTestNames not present in the ${mutation.expectedTestFile} baseline inventory: ${missing.join(', ')}`);

  const { status, assertions } = runTestFile(mutation.expectedTestFile, { [envKey]: JSON.stringify(mutation) }, label);
  if (status === 0) throw new Error(`${label}: mutation survived the focused gate`);
  if (assertions.length !== baseline.assertions.length) {
    throw new Error(`${label}: expected the full baseline inventory (${baseline.assertions.length} tests in ${mutation.expectedTestFile}) to run under the mutation, received ${assertions.length}; the mutation likely short-circuited the file before every test executed`);
  }
  const failed = assertions.filter(({ assertion }) => assertion.status === 'failed');
  const failedTitles = failed.map(({ assertion }) => assertion.title).sort();
  const expectedTitles = [...mutation.expectedFailedTestNames].sort();
  if (JSON.stringify(failedTitles) !== JSON.stringify(expectedTitles)) {
    throw new Error(`${label}: expected exactly [${expectedTitles.join(', ')}] to fail and every other test in the ${baseline.assertions.length}-test inventory to pass, received [${failedTitles.join(', ')}] failed`);
  }
  const passedTitles = assertions.filter(({ assertion }) => assertion.status === 'passed').map(({ assertion }) => assertion.title).sort();
  const expectedPassedTitles = [...baseline.titles].filter((title) => !mutation.expectedFailedTestNames.includes(title)).sort();
  if (JSON.stringify(passedTitles) !== JSON.stringify(expectedPassedTitles)) {
    throw new Error(`${label}: every non-designated test must still pass; expected passing [${expectedPassedTitles.join(', ')}], received passing [${passedTitles.join(', ')}]`);
  }
  const failure = failed.map(({ assertion }) => (Array.isArray(assertion.failureMessages) ? assertion.failureMessages.join('\n') : '')).join('\n');
  if (!new RegExp(mutation.expectedFailurePattern).test(failure)) {
    throw new Error(`${label}: designated failure(s) carried the wrong diagnostic; expected ${mutation.expectedFailurePattern}, received ${failure}`);
  }
}

/**
 * Run the full baseline-then-compare gate for a flat list of mutations
 * (each carrying its own `expectedTestFile`). Returns the unique test files
 * and their captured baselines so the caller can print an evidence summary.
 */
export function runMutationSuite({ mutations, envKey }) {
  const testFiles = [...new Set(mutations.map((mutation) => mutation.expectedTestFile))];
  const baselines = captureBaselines(testFiles);
  for (const mutation of mutations) verifyMutation({ mutation, envKey, baselines });
  return { testFiles, baselines };
}
