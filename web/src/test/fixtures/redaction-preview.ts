import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { parse } from 'yaml';
import type { RedactionCategory } from '@/types/messages';

export const RedactionCategoryFixtureKind = {
  Success: 'success',
  UnknownGroup: 'unknown_group',
  UnknownItem: 'unknown_item',
  Mismatch: 'mismatch',
} as const;

export type RedactionCategoryFixtureKind =
  (typeof RedactionCategoryFixtureKind)[keyof typeof RedactionCategoryFixtureKind];

export interface RedactionCategoryFixture {
  name: string;
  kind: RedactionCategoryFixtureKind;
  groupCategory: string;
  itemCategory: string;
  expectedCategory?: RedactionCategory;
  expectedError?: string;
}

interface RedactionCategoryFixtureFile {
  cases?: RedactionCategoryFixture[];
}

interface RedactionLevelFixtureFile {
  unknownValues?: string[];
}

const REDACTION_CATEGORY_FIXTURE_PATH = resolve(
  process.cwd(),
  'src/test/testdata/redaction_categories.yaml',
);

export const REDACTION_CATEGORY_FIXTURES = loadRedactionCategoryFixtures();

export const REDACTION_PREVIEW_UNKNOWN_ITEM_ERROR = requiredFailureFixture(
  RedactionCategoryFixtureKind.UnknownItem,
).expectedError as string;

export const UNKNOWN_REDACTION_LEVEL_FIXTURES = loadUnknownRedactionLevelFixtures();

function loadUnknownRedactionLevelFixtures(): string[] {
  const source = readFileSync(
    resolve(process.cwd(), 'src/test/testdata/redaction_levels.yaml'),
    'utf8',
  );
  const parsed = parse(source) as RedactionLevelFixtureFile;
  const values = parsed.unknownValues ?? [];
  if (values.length !== 2 || values.some((value) => !value || value.trim() !== value)) {
    throw new Error('redaction level fixture must contain exactly two non-empty, trimmed unknown values');
  }
  return values;
}

function loadRedactionCategoryFixtures(): RedactionCategoryFixture[] {
  const source = readFileSync(REDACTION_CATEGORY_FIXTURE_PATH, 'utf8');
  const parsed = parse(source) as RedactionCategoryFixtureFile;
  const cases = parsed.cases ?? [];
  if (cases.length !== 7) {
    throw new Error(
      `redaction category fixture has ${cases.length} cases, want four canonical values plus unknown-group, unknown-item, and group/item-mismatch failures`,
    );
  }

  const names = new Set<string>();
  const canonical = new Set<RedactionCategory>(['CREDENTIAL', 'PII', 'PATH', 'INTERNAL']);
  const seenCanonical = new Set<RedactionCategory>();
  const failureKinds = new Set<RedactionCategoryFixtureKind>();
  const validKinds = new Set<RedactionCategoryFixtureKind>(
    Object.values(RedactionCategoryFixtureKind),
  );
  for (const [index, fixture] of cases.entries()) {
    if (
      !fixture.name ||
      !validKinds.has(fixture.kind) ||
      !fixture.groupCategory ||
      !fixture.itemCategory ||
      names.has(fixture.name)
    ) {
      throw new Error(
        `redaction category fixture cases[${index}] needs a unique name, valid kind, and non-empty groupCategory and itemCategory`,
      );
    }
    names.add(fixture.name);
    const groupKnown = canonical.has(fixture.groupCategory as RedactionCategory);
    const itemKnown = canonical.has(fixture.itemCategory as RedactionCategory);
    if (fixture.kind === RedactionCategoryFixtureKind.Success) {
      if (!fixture.expectedCategory || fixture.expectedError) {
        throw new Error(
          `redaction category fixture ${JSON.stringify(fixture.name)} success case needs expectedCategory and no expectedError`,
        );
      }
      if (!canonical.has(fixture.expectedCategory)) {
        throw new Error(
          `redaction category fixture ${JSON.stringify(fixture.name)} has invalid expectedCategory ${JSON.stringify(fixture.expectedCategory)}`,
        );
      }
      if (
        fixture.groupCategory !== fixture.expectedCategory ||
        fixture.itemCategory !== fixture.expectedCategory
      ) {
        throw new Error(
          `redaction category fixture ${JSON.stringify(fixture.name)} changes canonical wire bytes`,
        );
      }
      seenCanonical.add(fixture.expectedCategory);
      continue;
    }
    if (fixture.expectedCategory || !fixture.expectedError) {
      throw new Error(
        `redaction category fixture ${JSON.stringify(fixture.name)} failure case needs expectedError and no expectedCategory`,
      );
    }
    if (failureKinds.has(fixture.kind)) {
      throw new Error(
        `redaction category fixture duplicates failure kind ${JSON.stringify(fixture.kind)}`,
      );
    }
    failureKinds.add(fixture.kind);
    const validShape =
      (fixture.kind === RedactionCategoryFixtureKind.UnknownGroup && !groupKnown && itemKnown) ||
      (fixture.kind === RedactionCategoryFixtureKind.UnknownItem && groupKnown && !itemKnown) ||
      (fixture.kind === RedactionCategoryFixtureKind.Mismatch &&
        groupKnown &&
        itemKnown &&
        fixture.groupCategory !== fixture.itemCategory);
    if (!validShape) {
      throw new Error(
        `redaction category fixture ${JSON.stringify(fixture.name)} does not match discriminator ${JSON.stringify(fixture.kind)}`,
      );
    }
  }
  const requiredFailures = [
    RedactionCategoryFixtureKind.UnknownGroup,
    RedactionCategoryFixtureKind.UnknownItem,
    RedactionCategoryFixtureKind.Mismatch,
  ];
  if (
    seenCanonical.size !== canonical.size ||
    requiredFailures.some((kind) => !failureKinds.has(kind))
  ) {
    throw new Error(
      `redaction category fixture must cover all four canonical values and each failure kind exactly once; got ${seenCanonical.size} canonical and failure kinds ${JSON.stringify([...failureKinds].sort())}`,
    );
  }
  return cases;
}

function requiredFailureFixture(kind: RedactionCategoryFixtureKind): RedactionCategoryFixture {
  const fixture = REDACTION_CATEGORY_FIXTURES.find((row) => row.kind === kind);
  if (!fixture) {
    throw new Error(`redaction category fixture has no ${JSON.stringify(kind)} case`);
  }
  return fixture;
}
