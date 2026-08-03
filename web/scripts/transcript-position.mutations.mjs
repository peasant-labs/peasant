import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import YAML from 'yaml';
import { exactFields, requireSingleOccurrence, runMutationSuite } from './lib/vitest-mutation-runner.mjs';

const productionPath = resolve('src/components/session-detail/v2/SessionDetailV2.tsx');
const manifestPaths = [
  resolve('src/components/session-detail/v2/testdata/transcript_target_recovery.manifest.yaml'),
  resolve('src/components/session-detail/v2/testdata/transcript_mounted_position.manifest.yaml'),
  resolve('src/components/session-detail/v2/testdata/schema_contract.manifest.yaml'),
];
const production = readFileSync(productionPath, 'utf8');
const mutationFields = [
  'name',
  'find',
  'replace',
  'expectedTestFile',
  'expectedFailedTestNames',
  'expectedFailurePattern',
];

const mutations = manifestPaths.flatMap((manifestPath) => {
  const document = YAML.parseDocument(readFileSync(manifestPath, 'utf8'), { strict: true, uniqueKeys: true });
  if (document.errors.length) throw new Error(`transcript-position mutation manifest is invalid: ${document.errors.map((error) => error.message).join('; ')}`);
  const manifest = document.toJS();
  if (
    !manifest || typeof manifest !== 'object' || Array.isArray(manifest) ||
    !Array.isArray(manifest.mutations) ||
    !Number.isSafeInteger(manifest.expectedMutationCount) ||
    manifest.expectedMutationCount !== manifest.mutations.length
  ) {
    throw new Error('transcript-position mutation manifest must contain its exact mutation inventory');
  }
  return manifest.mutations;
});
if (new Set(mutations.map((row) => row?.name)).size !== mutations.length) throw new Error('transcript-position production mutation names must be globally unique');

for (const [index, mutation] of mutations.entries()) {
  exactFields(mutation, mutationFields, `mutation ${index}`);
  for (const field of ['name', 'find', 'replace', 'expectedTestFile', 'expectedFailurePattern']) {
    if (typeof mutation[field] !== 'string' || mutation[field].length === 0) throw new Error(`mutation ${index} has an invalid ${field}`);
  }
  if (!Array.isArray(mutation.expectedFailedTestNames) || mutation.expectedFailedTestNames.length === 0 || mutation.expectedFailedTestNames.some((name) => typeof name !== 'string' || name.length === 0) || new Set(mutation.expectedFailedTestNames).size !== mutation.expectedFailedTestNames.length) {
    throw new Error(`mutation ${index} (${mutation.name}) requires a non-empty array of unique nonempty expectedFailedTestNames`);
  }
  if (/^(?:AssertionError|Expected)$/.test(mutation.expectedFailurePattern)) throw new Error(`${mutation.name}: expectedFailurePattern must identify the violated invariant`);
  requireSingleOccurrence(production, mutation.find, mutation.name);
}

// PEASANT_TRANSCRIPT_MUTATION_JSON is read by web/vitest.config.ts's
// isolatedTranscriptMutationPlugin, always targeting SessionDetailV2.tsx.
const { testFiles, baselines } = runMutationSuite({ mutations, envKey: 'PEASANT_TRANSCRIPT_MUTATION_JSON' });

console.log(`transcript-position mutations: ${mutations.length} isolated executable production mutations were killed without modifying tracked sources, each proven against its file's full baseline inventory (${testFiles.map((testFile) => `${testFile}: ${baselines.get(testFile).assertions.length}`).join(', ')})`);
