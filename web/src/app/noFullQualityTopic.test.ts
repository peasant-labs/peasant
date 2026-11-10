import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { parseStrictYAML, requireExactFields, requireExactRequiredFields, requireRecord, requireUniqueNames } from '@/test/strictYaml';

// The no-full-quality-topic guard: the transcript and the project-sessions and
// grouped surfaces this change rewrites must not reach for the retired full
// quality topic. The guard reads the production sources, so deleting the
// subscription cannot silently come back with the old behavior.

type GuardCase = { name: string; path: string; forbidden: string[] };
type GuardMutation = { name: string; path: string; find: string; replace: string; expectedError: string };

const source = readFileSync(resolve(process.cwd(), 'src/app/testdata/no_full_quality_topic.yaml'), 'utf8');

function loadFixture() {
  const root = requireRecord(parseStrictYAML(source, 'no full quality topic guard'), 'no full quality topic guard');
  requireExactRequiredFields(root, ['requiredNames', 'cases', 'mutations'], 'no full quality topic guard');
  if (!Array.isArray(root.requiredNames) || !Array.isArray(root.cases) || !Array.isArray(root.mutations)) {
    throw new Error('no full quality topic guard requires requiredNames, cases, and mutations');
  }
  const cases = root.cases.map((value, index) => {
    const row = requireRecord(value, `no full quality topic guard.cases[${index}]`);
    requireExactFields(row, ['name', 'path', 'forbidden'], `no full quality topic guard.cases[${index}]`);
    if (typeof row.name !== 'string' || typeof row.path !== 'string' || !Array.isArray(row.forbidden) || row.forbidden.some((token) => typeof token !== 'string' || token.length === 0)) {
      throw new Error(`no full quality topic guard.cases[${index}] is invalid`);
    }
    return row as unknown as GuardCase;
  });
  requireUniqueNames(cases, 'no full quality topic guard.cases');
  const required = root.requiredNames as string[];
  if (required.length !== cases.length || required.some((name) => !cases.some((row) => row.name === name))) {
    throw new Error('no full quality topic guard cases do not match their required names');
  }
  const mutations = root.mutations.map((value, index) => {
    const row = requireRecord(value, `no full quality topic guard.mutations[${index}]`);
    requireExactFields(row, ['name', 'path', 'find', 'replace', 'expectedError'], `no full quality topic guard.mutations[${index}]`);
    if (['name', 'path', 'find', 'replace', 'expectedError'].some((field) => typeof row[field] !== 'string' || (row[field] as string).length === 0)) {
      throw new Error(`no full quality topic guard.mutations[${index}] is invalid`);
    }
    return row as unknown as GuardMutation;
  });
  return { cases, mutations };
}

function forbiddenIn(sourceText: string, guard: GuardCase): string[] {
  return guard.forbidden.filter((token) => sourceText.includes(token));
}

const fixture = loadFixture();

describe('the full quality topic has no consumer on the rewritten surfaces', () => {
  for (const guard of fixture.cases) {
    it(guard.name, () => {
      const sourceText = readFileSync(resolve(process.cwd(), guard.path), 'utf8');
      expect(forbiddenIn(sourceText, guard)).toEqual([]);
    });
  }

  for (const mutation of fixture.mutations) {
    it(mutation.name, () => {
      const original = readFileSync(resolve(process.cwd(), mutation.path), 'utf8');
      const count = original.split(mutation.find).length - 1;
      expect(count, 'mutation anchor must occur exactly once').toBe(1);
      const mutated = original.replace(mutation.find, mutation.replace);
      const guard = fixture.cases.find((row) => row.path === mutation.path);
      expect(guard).toBeDefined();
      expect(forbiddenIn(mutated, guard as GuardCase)).toContain(mutation.expectedError);
    });
  }
});
