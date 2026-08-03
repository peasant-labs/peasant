import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import YAML from 'yaml';
import { exactFields, requireSingleOccurrence, runMutationSuite } from './lib/vitest-mutation-runner.mjs';

const manifestPaths = [
  resolve('src/lib/api/testdata/runtime_contract.manifest.yaml'),
  resolve('src/lib/quality/testdata/session_outcomes.manifest.yaml'),
  resolve('src/components/session-detail/v2/testdata/quality_median_bridge.manifest.yaml'),
  resolve('src/test/testdata/graph_adapter_contract.manifest.yaml'),
];
const mutationFields = ['name', 'target', 'find', 'replace', 'expectedTestFile', 'expectedFailedTestNames', 'expectedFailurePattern'];
const mutations = manifestPaths.flatMap((manifestPath) => {
  const document = YAML.parseDocument(readFileSync(manifestPath, 'utf8'), { strict: true, uniqueKeys: true });
  if (document.errors.length) throw new Error(`contract-boundary mutation manifest is invalid: ${document.errors.map((error) => error.message).join('; ')}`);
  const manifest = document.toJS();
  if (!manifest || typeof manifest !== 'object' || !Array.isArray(manifest.mutations) || manifest.expectedMutationCount !== manifest.mutations.length) throw new Error('contract-boundary manifest must contain an exact mutation inventory');
  return manifest.mutations;
});
if (new Set(mutations.map((mutation) => mutation?.name)).size !== mutations.length) throw new Error('contract-boundary mutation names must be globally unique');

for (const [index, mutation] of mutations.entries()) {
  exactFields(mutation, mutationFields, `contract-boundary mutation ${index}`);
  for (const field of ['name', 'target', 'find', 'replace', 'expectedTestFile', 'expectedFailurePattern']) {
    if (typeof mutation[field] !== 'string' || mutation[field].length === 0) throw new Error(`contract-boundary mutation ${index} has an invalid ${field}`);
  }
  if (!Array.isArray(mutation.expectedFailedTestNames) || mutation.expectedFailedTestNames.length === 0 || mutation.expectedFailedTestNames.some((name) => typeof name !== 'string' || name.length === 0) || new Set(mutation.expectedFailedTestNames).size !== mutation.expectedFailedTestNames.length) {
    throw new Error(`contract-boundary mutation ${index} (${mutation.name}) requires a non-empty array of unique nonempty expectedFailedTestNames`);
  }
  const source = readFileSync(resolve(`.${mutation.target}`), 'utf8');
  requireSingleOccurrence(source, mutation.find, mutation.name);
}

// PEASANT_SOURCE_MUTATION_JSON is read by web/vitest.config.ts's
// isolatedSourceMutationPlugin, matched against `mutation.target`.
const { testFiles, baselines } = runMutationSuite({ mutations, envKey: 'PEASANT_SOURCE_MUTATION_JSON' });

console.log(`contract-boundary mutations: ${mutations.length} isolated production mutations were killed, each proven against its file's full baseline inventory (${testFiles.map((testFile) => `${testFile}: ${baselines.get(testFile).assertions.length}`).join(', ')})`);
