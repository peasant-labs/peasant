import { within } from '@testing-library/react';
import { expect } from 'vitest';
import {
  parseStrictYAML,
  requireExactRequiredFields,
  requireRecord,
  requireUniqueNames,
} from '@/test/strictYaml';

export type CommitRowExpectedRow = {
  sessionId: string;
  expectedName: string;
  expectedHarness: string | null;
};

export type CommitRowMutation = {
  name: string;
  caseName: string;
  sessionId: string;
  find: string;
  replace: string;
};

export type CommitRowManifest = {
  expectedCount: number;
  requiredNames: string[];
  classification: { surface: string; scope: string; behavior: string };
  provenance: { source: string; test: string; fixture: string };
  expectedMutationCount: number;
  requiredMutationNames: string[];
  mutations: CommitRowMutation[];
};

export function commitRowAccessibleName(expected: Pick<CommitRowExpectedRow, 'expectedName' | 'expectedHarness'>): string {
  return expected.expectedHarness ? `${expected.expectedHarness} ${expected.expectedName}` : expected.expectedName;
}

export function assertCommitRowLink(link: HTMLElement, expected: CommitRowExpectedRow): void {
  expect(link).toHaveTextContent(expected.expectedName);
  expect(link).toHaveAccessibleName(commitRowAccessibleName(expected));
  expect(link).toHaveStyle({ fontSize: 'var(--fs-label)' });
  if (expected.expectedHarness) {
    const providerName = within(link).getByText(expected.expectedHarness).closest('.pv-name');
    expect(providerName).not.toBeNull();
    expect(providerName).toHaveStyle({ fontSize: 'var(--fs-label)' });
  } else {
    expect(link.querySelector('.pv-name')).toBeNull();
  }
}

export function replaceExactlyOnce(source: string, find: string, replace: string, label: string): string {
  const count = source.split(find).length - 1;
  if (count !== 1) {
    throw new Error(`${label} mutation anchor must occur exactly once, found ${count}`);
  }
  return source.replace(find, replace);
}

export function loadCommitRowManifest(source: string, label: string): CommitRowManifest {
  const root = requireRecord(parseStrictYAML(source, label), label);
  requireExactRequiredFields(
    root,
    ['expectedCount', 'requiredNames', 'classification', 'provenance', 'expectedMutationCount', 'requiredMutationNames', 'mutations'],
    label,
  );
  if (!Number.isSafeInteger(root.expectedCount) || !Array.isArray(root.requiredNames)) {
    throw new Error(`${label} requires an independent case count and required-name inventory`);
  }
  if (root.requiredNames.some((value) => typeof value !== 'string' || value.length === 0)) {
    throw new Error(`${label}.requiredNames must contain only non-empty strings`);
  }

  const classification = requireRecord(root.classification, `${label}.classification`);
  requireExactRequiredFields(classification, ['surface', 'scope', 'behavior'], `${label}.classification`);
  if (Object.values(classification).some((value) => typeof value !== 'string' || value.length === 0)) {
    throw new Error(`${label}.classification fields must be non-empty strings`);
  }

  const provenance = requireRecord(root.provenance, `${label}.provenance`);
  requireExactRequiredFields(provenance, ['source', 'test', 'fixture'], `${label}.provenance`);
  if (Object.values(provenance).some((value) => typeof value !== 'string' || value.length === 0)) {
    throw new Error(`${label}.provenance fields must be non-empty strings`);
  }

  if (!Number.isSafeInteger(root.expectedMutationCount) || !Array.isArray(root.requiredMutationNames) || !Array.isArray(root.mutations)) {
    throw new Error(`${label} requires an independent mutation count and inventory`);
  }
  if (root.requiredMutationNames.some((value) => typeof value !== 'string' || value.length === 0)) {
    throw new Error(`${label}.requiredMutationNames must contain only non-empty strings`);
  }

  const mutations = root.mutations.map((value, index) => {
    const row = requireRecord(value, `${label}.mutations[${index}]`);
    requireExactRequiredFields(row, ['name', 'caseName', 'sessionId', 'find', 'replace'], `${label}.mutations[${index}]`);
    if (Object.values(row).some((value) => typeof value !== 'string' || value.length === 0)) {
      throw new Error(`${label}.mutations[${index}] requires non-empty string fields`);
    }
    return row as CommitRowMutation;
  });
  requireUniqueNames(mutations, `${label}.mutations`);
  const requiredCaseNames = new Set(root.requiredNames as string[]);
  for (const mutation of mutations) {
    if (!requiredCaseNames.has(mutation.caseName)) {
      throw new Error(`${label}.mutations references unknown case ${JSON.stringify(mutation.caseName)}`);
    }
  }
  if (mutations.length !== root.expectedMutationCount) {
    throw new Error(`${label} mutation inventory must contain exactly ${root.expectedMutationCount} rows`);
  }
  const mutationNames = mutations.map((row) => row.name);
  if (JSON.stringify(mutationNames) !== JSON.stringify(root.requiredMutationNames)) {
    throw new Error(`${label} mutation inventory does not match its independent name list`);
  }

  return {
    expectedCount: root.expectedCount as number,
    requiredNames: root.requiredNames as string[],
    classification: classification as CommitRowManifest['classification'],
    provenance: provenance as CommitRowManifest['provenance'],
    expectedMutationCount: root.expectedMutationCount as number,
    requiredMutationNames: root.requiredMutationNames as string[],
    mutations,
  };
}

export async function proveCommitRowMutation<F extends { cases: Array<{ name: string; expectedRows: CommitRowExpectedRow[] }> }>(
  source: string,
  mutation: CommitRowMutation,
  loadFixture: (fixtureSource: string) => F,
  renderLink: (fixture: F, mutation: CommitRowMutation) => HTMLElement | Promise<HTMLElement>,
  selectExpected: (fixture: F, mutation: CommitRowMutation) => CommitRowExpectedRow,
): Promise<void> {
  const mutatedSource = replaceExactlyOnce(source, mutation.find, mutation.replace, mutation.name);
  const fixture = loadFixture(mutatedSource);
  const link = await renderLink(fixture, mutation);
  const expected = selectExpected(fixture, mutation);
  expect(() => assertCommitRowLink(link, expected)).toThrow();
}
