import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import {
  parseStrictYAML,
  requireExactFields,
  requireExactRequiredFields,
  requireRecord,
  requireUniqueNames,
} from '@/test/strictYaml';
import {
  DEFAULT_HELPER_MEMBER_LIMIT,
  firstHelperMemberPage,
  gotoHelperMemberPage,
  helperMemberPageWindow,
  withHelperMemberTotal,
  type HelperMemberPageWindow,
  type HelperMemberPaging,
} from './helperGroups';

const manifestSource = readFileSync(
  resolve(process.cwd(), 'src/lib/sessions/testdata/helper_member_paging.manifest.yaml'),
  'utf8',
);
const casesSource = readFileSync(
  resolve(process.cwd(), 'src/lib/sessions/testdata/helper_member_paging.yaml'),
  'utf8',
);

type PagingOperation = 'window' | 'firstPage' | 'gotoPage' | 'withTotal';

type PagingCase = {
  name: string;
  operation: PagingOperation;
  paging?: HelperMemberPaging;
  limit?: number;
  page?: number;
  served?: { page: number; total: number };
  expected?: HelperMemberPageWindow | Partial<HelperMemberPaging>;
  expectError?: boolean;
};

const CASE_FIELDS = ['name', 'operation', 'paging', 'limit', 'page', 'served', 'expected', 'expectError'] as const;
const OPERATIONS: readonly PagingOperation[] = ['window', 'firstPage', 'gotoPage', 'withTotal'];

const OPERATION_FIELDS: Record<PagingOperation, readonly string[]> = {
  window: ['name', 'operation', 'paging'],
  firstPage: ['name', 'operation', 'limit'],
  gotoPage: ['name', 'operation', 'paging', 'page'],
  withTotal: ['name', 'operation', 'paging', 'served'],
};
const MANIFEST_FIELDS = [
  'expectedCount',
  'requiredNames',
  'requiredOperations',
  'expectedLoaderMutationCount',
  'loaderMutations',
] as const;

function replaceExactlyOnce(source: string, find: string, replace: string, label: string): string {
  const count = source.split(find).length - 1;
  if (count !== 1) throw new Error(`${label} mutation anchor must occur exactly once, received ${count}`);
  return source.replace(find, replace);
}

function loadPagingFixture(
  manifestSourceValue = manifestSource,
  casesSourceValue = casesSource,
): PagingCase[] {
  const manifest = requireRecord(
    parseStrictYAML(manifestSourceValue, 'helper member paging manifest'),
    'helper member paging manifest',
  );
  requireExactRequiredFields(manifest, MANIFEST_FIELDS, 'helper member paging manifest');
  const requiredNames = manifest.requiredNames as unknown[];
  if (
    !Number.isSafeInteger(manifest.expectedCount)
    || !Array.isArray(requiredNames)
    || requiredNames.length !== manifest.expectedCount
    || requiredNames.some((name) => typeof name !== 'string' || name.length === 0)
    || new Set(requiredNames).size !== requiredNames.length
  ) {
    throw new Error('helper member paging manifest must carry one independent, unique required-name per expected case');
  }
  if (
    !Array.isArray(manifest.requiredOperations)
    || [...manifest.requiredOperations].sort().join(',') !== [...OPERATIONS].sort().join(',')
  ) {
    throw new Error('helper member paging manifest must name every closed paging operation exactly once');
  }

  const root = requireRecord(parseStrictYAML(casesSourceValue, 'helper member paging cases'), 'helper member paging cases');
  requireExactRequiredFields(root, ['cases'], 'helper member paging cases');
  if (!Array.isArray(root.cases)) throw new Error('helper member paging cases.cases must be an array');
  const cases = root.cases.map((value, index) => {
    const path = `helper member paging cases.cases[${index}]`;
    const row = requireRecord(value, path);
    requireExactFields(row, CASE_FIELDS, path);
    if (!OPERATIONS.includes(row.operation as PagingOperation)) {
      throw new Error(`${path}.operation must be one of ${OPERATIONS.join(', ')}`);
    }
    const operation = row.operation as PagingOperation;
    const required = [...OPERATION_FIELDS[operation]];
    if (row.expectError !== true) required.push('expected');
    const missing = required.filter((field) => !(field in row));
    if (missing.length > 0) throw new Error(`${path} is missing required fields: ${missing.join(', ')}`);
    const expectError = row.expectError === true;
    if (!expectError) {
      if (!row.expected) throw new Error(`${path} must carry expected output unless it expects an error`);
      if (operation === 'window' || operation === 'gotoPage' || operation === 'withTotal') {
        requireRecord(row.paging, `${path}.paging`);
      }
      if (operation === 'gotoPage' && !Number.isSafeInteger(row.page)) {
        throw new Error(`${path}.page must be an integer`);
      }
      if (operation === 'withTotal') requireRecord(row.served, `${path}.served`);
    }
    return row as unknown as PagingCase;
  });
  requireUniqueNames(cases as unknown as Record<string, unknown>[], 'helper member paging cases.cases');
  if (
    !Number.isSafeInteger(manifest.expectedLoaderMutationCount)
    || !Array.isArray(manifest.loaderMutations)
    || manifest.loaderMutations.length !== manifest.expectedLoaderMutationCount
  ) {
    throw new Error('helper member paging manifest must carry one loader mutation per expected mutation');
  }

  const names = new Set(cases.map((testCase) => testCase.name));
  if (cases.length !== manifest.expectedCount || requiredNames.some((name) => !names.has(name as string))) {
    throw new Error('helper member paging manifest must name exactly the required cases; a case is missing or renamed');
  }
  return cases;
}

function runPagingCase(testCase: PagingCase): unknown {
  switch (testCase.operation) {
    case 'window':
      return helperMemberPageWindow(testCase.paging!);
    case 'firstPage':
      return firstHelperMemberPage(testCase.limit);
    case 'gotoPage':
      return gotoHelperMemberPage(testCase.paging!, testCase.page!);
    case 'withTotal':
      return withHelperMemberTotal(testCase.paging!, testCase.served!);
  }
}

const fixture = loadPagingFixture();

describe('helper member paging fixture', () => {
  it('rejects an independent manifest count drift', () => {
    const mutation = manifestMutation('paging count drift is rejected');
    expect(() => loadPagingFixture(mutation.manifest, mutation.cases)).toThrow(/independent/);
  });

  it('rejects duplicate case names', () => {
    const mutation = manifestMutation('paging duplicate case names are rejected');
    expect(() => loadPagingFixture(mutation.manifest, mutation.cases)).toThrow(/duplicate/i);
  });

  it('rejects a missing or renamed required case', () => {
    const mutation = manifestMutation('paging missing required case is rejected');
    expect(() => loadPagingFixture(mutation.manifest, mutation.cases)).toThrow(/required case/);
  });

  for (const testCase of fixture) {
    it(`${testCase.operation}: ${testCase.name}`, () => {
      if (testCase.expectError) {
        expect(() => runPagingCase(testCase)).toThrow(/paging/);
        return;
      }
      expect(runPagingCase(testCase)).toEqual(testCase.expected);
    });
  }

  it('defaults the group limit to the server member page size', () => {
    expect(firstHelperMemberPage().limit).toBe(DEFAULT_HELPER_MEMBER_LIMIT);
    expect(firstHelperMemberPage().limit).toBe(20);
  });
});

function manifestMutation(name: string): { manifest?: string; cases?: string } {
  const manifest = requireRecord(
    parseStrictYAML(manifestSource, 'helper member paging manifest'),
    'helper member paging manifest',
  );
  const mutations = manifest.loaderMutations;
  if (!Array.isArray(mutations)) throw new Error('helper member paging manifest.loaderMutations must be an array');
  const mutation = mutations.map((value) => requireRecord(value, 'loader mutation')).find((value) => value.name === name);
  if (!mutation) throw new Error(`helper member paging manifest is missing loader mutation ${name}`);
  requireExactRequiredFields(
    mutation,
    ['name', 'target', 'find', 'replace', 'expectedError'],
    `loader mutation ${name}`,
  );
  return {
    manifest:
      mutation.target === 'manifest'
        ? replaceExactlyOnce(manifestSource, String(mutation.find), String(mutation.replace), name)
        : undefined,
    cases:
      mutation.target === 'cases'
        ? replaceExactlyOnce(casesSource, String(mutation.find), String(mutation.replace), name)
        : undefined,
  };
}
