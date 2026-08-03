import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { SessionOutcome, type AnnotationSummary, type QualitySession as SchemaQualitySession } from '@peasant-labs/schema';
import { adaptQualitySessions, deriveLabels, outcomeValueToLabel, type DerivedLabels } from './types';
import { parseStrictYAML, requireExactRequiredFields, requireRecord, requireUniqueNames } from '@/test/strictYaml';

type OutcomeCase = { name: string; outcome: string; valid: boolean };
type EmptyOutcomeCase = { name: string; outcome: string };
type OutcomeLabelCase = { name: string; value: string; expected: 'positive' | 'negative' | null };
type LabelCase = { name: string; annotations: AnnotationSummary[] | null; expected: DerivedLabels };
type OutcomeFixture = { baseSession: Omit<SchemaQualitySession, 'outcome'>; cases: OutcomeCase[]; emptyOutcomeCases: EmptyOutcomeCase[]; outcomeLabelCases: OutcomeLabelCase[]; labelCases: LabelCase[] };

const outcomeManifestSource = readFileSync(resolve(process.cwd(), 'src/lib/quality/testdata/session_outcomes.manifest.yaml'), 'utf8');
const outcomeCasesSource = readFileSync(resolve(process.cwd(), 'src/lib/quality/testdata/session_outcomes.yaml'), 'utf8');

function replaceExactlyOnce(source: string, find: string, replace: string, name: string): string {
  const count = source.split(find).length - 1;
  if (count !== 1) throw new Error(`${name} mutation anchor must occur exactly once, received ${count}`);
  return source.replace(find, replace);
}

function loadOutcomeFixture(manifestSource = outcomeManifestSource, casesSource = outcomeCasesSource): OutcomeFixture {
  const manifest = requireRecord(parseStrictYAML(manifestSource, 'quality outcome manifest'), 'quality outcome manifest');
  requireExactRequiredFields(manifest, ['expectedRootFields', 'expectedCaseCount', 'expectedEmptyOutcomeCaseCount', 'expectedOutcomeLabelCount', 'expectedLabelCount', 'validOutcomes', 'invalidOutcomes', 'requiredOutcomeNames', 'requiredEmptyOutcomeNames', 'requiredOutcomeLabelNames', 'requiredLabelNames', 'expectedLoaderMutationCount', 'loaderMutations', 'expectedMutationCount', 'mutations'], 'quality outcome manifest');
  const root = requireRecord(parseStrictYAML(casesSource, 'quality outcome fixture'), 'quality outcome fixture');
  if (!Array.isArray(manifest.expectedRootFields) || JSON.stringify(Object.keys(root)) !== JSON.stringify(manifest.expectedRootFields)) throw new Error('quality outcome fixture root fields do not match their independent manifest');
  requireExactRequiredFields(root, manifest.expectedRootFields as string[], 'quality outcome fixture');
  const collectionFields = ['cases', 'emptyOutcomeCases', 'outcomeLabelCases', 'labelCases'];
  for (const field of collectionFields) {
    if (!Array.isArray(root[field])) throw new Error(`quality outcome fixture ${field} must be an array`);
    const rows = (root[field] as unknown[]).map((row, index) => requireRecord(row, `quality outcome fixture ${field}[${index}]`));
    requireUniqueNames(rows, `quality outcome fixture ${field}`);
  }
  const cases = root.cases as OutcomeCase[];
  cases.forEach((row, index) => {
    requireExactRequiredFields(row as unknown as Record<string, unknown>, ['name', 'outcome', 'valid'], `quality outcome fixture cases[${index}]`);
    if (typeof row.name !== 'string' || typeof row.outcome !== 'string' || typeof row.valid !== 'boolean') throw new Error(`quality outcome fixture cases[${index}] has invalid fields`);
  });
  const emptyOutcomeCases = root.emptyOutcomeCases as EmptyOutcomeCase[];
  emptyOutcomeCases.forEach((row, index) => {
    requireExactRequiredFields(row as unknown as Record<string, unknown>, ['name', 'outcome'], `quality outcome fixture emptyOutcomeCases[${index}]`);
    if (typeof row.name !== 'string' || typeof row.outcome !== 'string') throw new Error(`quality outcome fixture emptyOutcomeCases[${index}] has invalid fields`);
  });
  const valid = cases.filter((row) => row.valid).map((row) => row.outcome);
  const invalid = cases.filter((row) => !row.valid).map((row) => row.outcome);
  if (cases.length !== manifest.expectedCaseCount || emptyOutcomeCases.length !== manifest.expectedEmptyOutcomeCaseCount || (root.outcomeLabelCases as unknown[]).length !== manifest.expectedOutcomeLabelCount || (root.labelCases as unknown[]).length !== manifest.expectedLabelCount || JSON.stringify(valid) !== JSON.stringify(manifest.validOutcomes) || JSON.stringify(invalid) !== JSON.stringify(manifest.invalidOutcomes) || JSON.stringify(cases.map((row) => row.name)) !== JSON.stringify(manifest.requiredOutcomeNames) || JSON.stringify(emptyOutcomeCases.map((row) => row.name)) !== JSON.stringify(manifest.requiredEmptyOutcomeNames)) throw new Error('quality outcome cases do not match their independent manifest');
  if (JSON.stringify((root.outcomeLabelCases as OutcomeLabelCase[]).map((row) => row.name)) !== JSON.stringify(manifest.requiredOutcomeLabelNames) || JSON.stringify((root.labelCases as LabelCase[]).map((row) => row.name)) !== JSON.stringify(manifest.requiredLabelNames)) throw new Error('quality label cases do not match their independent manifest');
  if (!Array.isArray(manifest.loaderMutations) || manifest.loaderMutations.length !== manifest.expectedLoaderMutationCount) throw new Error('quality outcome loader mutation inventory is incomplete');
  return root as unknown as OutcomeFixture;
}

const fixture = loadOutcomeFixture();

describe('quality wire outcome boundary', () => {
  it('rejects every fixture-loader mutation', () => {
    const manifest = requireRecord(parseStrictYAML(outcomeManifestSource, 'quality outcome manifest'), 'quality outcome manifest');
    for (const mutationValue of manifest.loaderMutations as unknown[]) {
      const mutation = requireRecord(mutationValue, 'quality outcome loader mutation');
      const target = mutation.target === 'manifest' ? outcomeManifestSource : outcomeCasesSource;
      const mutated = replaceExactlyOnce(target, String(mutation.find), String(mutation.replace), String(mutation.name));
      expect(() => loadOutcomeFixture(mutation.target === 'manifest' ? mutated : outcomeManifestSource, mutation.target === 'cases' ? mutated : outcomeCasesSource), String(mutation.name)).toThrow(new RegExp(String(mutation.expectedError)));
    }
  });
  it('keeps the fixture valid-outcome inventory exactly equal to the generated schema runtime', () => {
    expect(fixture.cases.filter((row) => row.valid).map((row) => row.outcome)).toEqual(Object.values(SessionOutcome));
  });

  for (const row of fixture.cases) {
    it(row.name, () => {
      const run = () => adaptQualitySessions([{ ...fixture.baseSession, outcome: row.outcome }]);
      if (row.valid) expect(run()[0]).toEqual({ ...fixture.baseSession, outcome: row.outcome });
      else expect(run).toThrow(/unknown outcome[\s\S]*adaptQualitySessions[\s\S]*quality channel[\s\S]*stopped[\s\S]*Regenerate/);
    });
  }

  // A session whose quality metrics never reached a resolution verdict wires
  // an empty outcome string on local stores.
  // It must pass through as `outcome: undefined` (the "not yet computed" state
  // AnalyticsSessionRecord already buckets under "unknown"), never throw.
  for (const row of fixture.emptyOutcomeCases) {
    it(row.name, () => {
      const result = adaptQualitySessions([{ ...fixture.baseSession, outcome: row.outcome }]);
      expect(result[0]).toEqual({ ...fixture.baseSession, outcome: undefined });
      expect(result[0]!.outcome).toBeUndefined();
    });
  }
});

describe('quality label derivation', () => {
  for (const row of fixture.outcomeLabelCases) {
    it(row.name, () => {
      expect(outcomeValueToLabel(row.value)).toBe(row.expected ?? undefined);
    });
  }
  for (const row of fixture.labelCases) {
    it(row.name, () => {
      expect(deriveLabels(row.annotations ?? undefined)).toEqual(row.expected);
    });
  }
});
