import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { parse, stringify } from 'yaml';
import { describe, expect, it } from 'vitest';
import {
  HELPER_GROUP_REQUIRED_ROLES,
  loadHelperGroupFixture,
  readHelperGroupFixture,
} from './helper-groups-fixture';
import { readLoaderMutationFixture, type LoaderMutation } from './helper-groups-loader';

const FIXTURE_PATH = resolve(process.cwd(), 'src/app/share/testdata/mounted-share-helper-groups.yaml');
const fixtureSource = readFileSync(FIXTURE_PATH, 'utf8');
const mutations = readLoaderMutationFixture();

interface MutableRow { role?: string; [field: string]: unknown }
interface MutableDocument {
  requiredNames: string[];
  paging: Record<string, number>;
  owners: MutableRow[];
  members: MutableRow[];
  contexts: MutableRow[];
  contextMembers: MutableRow[];
}

/** Apply one declared mutation to the checked-in corpus and re-serialize it. */
function applyMutation(source: string, mutation: LoaderMutation): string {
  // The document is mutable test input; the typed loader under test re-validates
  // the serialized result, so a cast here cannot weaken production validation.
  const document = parse(source) as MutableDocument;
  switch (mutation.operation) {
    case 'set-required-names':
      document.requiredNames = [...mutation.value];
      break;
    case 'remove-required-name':
      document.requiredNames = document.requiredNames.filter((name) => name !== mutation.role);
      break;
    case 'rename-corpus-role': {
      for (const row of [...document.owners, ...document.members, ...document.contexts, ...document.contextMembers]) {
        if (row.role === mutation.from) row.role = mutation.to;
      }
      break;
    }
    case 'set-member-field': {
      const row = [...document.members, ...document.contextMembers].find((member) => member.role === mutation.role);
      if (!row) throw new Error(`loader mutation ${mutation.name} names no member role ${mutation.role}`);
      row[mutation.field] = mutation.value;
      break;
    }
    case 'set-context-field': {
      const row = document.contexts.find((context) => context.role === mutation.role);
      if (!row) throw new Error(`loader mutation ${mutation.name} names no context role ${mutation.role}`);
      row[mutation.field] = mutation.value;
      break;
    }
    case 'set-root-field':
      (document as unknown as Record<string, unknown>)[mutation.field] = mutation.value;
      break;
  }
  return stringify(document);
}

describe('mounted helper-group fixture loader', () => {
  it('loads the unmutated corpus against the independent role inventory', () => {
    expect(() => readHelperGroupFixture()).not.toThrow();
    expect(() => loadHelperGroupFixture(fixtureSource, HELPER_GROUP_REQUIRED_ROLES)).not.toThrow();
  });

  it.each(mutations.cases)('rejects the $name mutation', (mutation) => {
    const mutated = applyMutation(fixtureSource, mutation);
    expect(mutated).not.toEqual(fixtureSource);
    expect(() => loadHelperGroupFixture(mutated, HELPER_GROUP_REQUIRED_ROLES)).toThrow(/mounted helper-group fixture is invalid/);
  });
});
