import { existsSync, readFileSync, readdirSync } from 'node:fs';
import { extname, join, resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import {
  parseStrictYAML,
  requireExactRequiredFields,
  requireRecord,
  requireUniqueNames,
} from '@/test/strictYaml';

const casesSource = readFileSync(
  resolve(process.cwd(), 'src/testdata/schema_runtime_architecture.yaml'),
  'utf8',
);
const manifestSource = readFileSync(
  resolve(process.cwd(), 'src/testdata/schema_runtime_architecture.manifest.yaml'),
  'utf8',
);

type ArchitectureCase = {
  name: string;
  scope: 'file' | 'tree' | 'absent-file';
  path: string;
  required: string[];
  forbidden: string[];
};

function stringArray(value: unknown, path: string): string[] {
  if (!Array.isArray(value) || value.some((entry) => typeof entry !== 'string' || entry.length === 0)) {
    throw new Error(`${path} must be an array of nonempty strings`);
  }
  return value as string[];
}

function loadCases(): ArchitectureCase[] {
  const manifest = requireRecord(
    parseStrictYAML(manifestSource, 'schema runtime architecture manifest'),
    'schema runtime architecture manifest',
  );
  requireExactRequiredFields(
    manifest,
    ['expectedCount', 'requiredNames'],
    'schema runtime architecture manifest',
  );
  const requiredNames = stringArray(
    manifest.requiredNames,
    'schema runtime architecture manifest.requiredNames',
  );
  if (!Number.isSafeInteger(manifest.expectedCount) || new Set(requiredNames).size !== requiredNames.length) {
    throw new Error('schema runtime architecture manifest requires an independent count and unique names');
  }

  const root = requireRecord(
    parseStrictYAML(casesSource, 'schema runtime architecture cases'),
    'schema runtime architecture cases',
  );
  requireExactRequiredFields(root, ['cases'], 'schema runtime architecture cases');
  if (!Array.isArray(root.cases)) throw new Error('schema runtime architecture cases must be an array');
  const rows = root.cases.map((row, index) =>
    requireRecord(row, `schema runtime architecture cases[${index}]`),
  );
  requireUniqueNames(rows, 'schema runtime architecture cases');
  rows.forEach((row, index) => {
    requireExactRequiredFields(
      row,
      ['name', 'scope', 'path', 'required', 'forbidden'],
      `schema runtime architecture cases[${index}]`,
    );
    if (
      typeof row.name !== 'string' ||
      typeof row.path !== 'string' ||
      !['file', 'tree', 'absent-file'].includes(String(row.scope))
    ) {
      throw new Error(`schema runtime architecture cases[${index}] has invalid scalar fields`);
    }
    stringArray(row.required, `schema runtime architecture cases[${index}].required`);
    stringArray(row.forbidden, `schema runtime architecture cases[${index}].forbidden`);
  });
  const names = rows.map((row) => row.name);
  if (
    rows.length !== manifest.expectedCount ||
    requiredNames.length !== rows.length ||
    requiredNames.some((name) => !names.includes(name))
  ) {
    throw new Error('schema runtime architecture cases do not match their independent manifest');
  }
  return rows as ArchitectureCase[];
}

function productionTree(path: string): string {
  const chunks: string[] = [];
  const visit = (directory: string) => {
    for (const entry of readdirSync(directory, { withFileTypes: true })) {
      const child = join(directory, entry.name);
      if (entry.isDirectory()) {
        if (entry.name !== 'testdata') visit(child);
      } else if (
        ['.ts', '.tsx'].includes(extname(entry.name)) &&
        !entry.name.includes('.test.') &&
        !entry.name.includes('.fixture.')
      ) {
        chunks.push(readFileSync(child, 'utf8'));
      }
    }
  };
  visit(resolve(process.cwd(), path));
  return chunks.join('\n');
}

describe('generated schema runtime architecture', () => {
  for (const fixture of loadCases()) {
    it(fixture.name, () => {
      const target = resolve(process.cwd(), fixture.path);
      if (fixture.scope === 'absent-file') {
        expect(existsSync(target)).toBe(false);
        return;
      }
      const source = fixture.scope === 'tree' ? productionTree(fixture.path) : readFileSync(target, 'utf8');
      for (const required of fixture.required) expect(source).toContain(required);
      for (const forbidden of fixture.forbidden) expect(source).not.toContain(forbidden);
    });
  }
});
