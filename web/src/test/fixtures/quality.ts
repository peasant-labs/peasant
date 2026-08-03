/**
 * Quality-channel test fixtures backed by the published shared fixture corpus.
 *
 * Source of truth:
 *   @peasant-labs/schema/fixtures/quality
 *
 * The package generates this typed entry point from the canonical YAML rows, so
 * consumers use the same fixture data without reaching into an unpublished Go
 * source-tree path.
 */

import { adaptQualitySessions, type QualitySession } from '@/lib/quality/types';
import {
  QualityFixtureName as SchemaQualityFixtureName,
  QualityFixtureSetName as SchemaQualityFixtureSetName,
  loadQualityFixtures,
  toQualitySession,
  type QualityFixtureSet,
  type QualitySessionFixture,
} from '@peasant-labs/schema/fixtures/quality';

export const QualityFixtureName = SchemaQualityFixtureName;
export type QualityFixtureName =
  (typeof QualityFixtureName)[keyof typeof QualityFixtureName];

export const QualityFixtureSetName = SchemaQualityFixtureSetName;
export type QualityFixtureSetName =
  (typeof QualityFixtureSetName)[keyof typeof QualityFixtureSetName];

const QUALITY_FIXTURES = loadQualityFixtures();
const QUALITY_FIXTURE_ROWS = loadQualityFixtureRows(QUALITY_FIXTURES.sessions);
const QUALITY_FIXTURE_SETS = loadQualityFixtureSets(QUALITY_FIXTURES.sets);

export function qualityFixture(name: QualityFixtureName): QualitySession {
  const row = QUALITY_FIXTURE_ROWS.find((fixture) => fixture.name === name);
  if (!row) {
    throw new Error(`unknown quality fixture ${JSON.stringify(name)}`);
  }
  return cloneQualitySession(row);
}

export function qualityFixtures(
  names: readonly QualityFixtureName[],
): QualitySession[] {
  return names.map(qualityFixture);
}

export function qualityFixtureSet(name: QualityFixtureSetName): QualitySession[] {
  const set = QUALITY_FIXTURE_SETS.find((fixtureSet) => fixtureSet.name === name);
  if (!set) {
    throw new Error(`unknown quality fixture set ${JSON.stringify(name)}`);
  }
  return qualityFixtures(set.cases);
}

export function allQualityFixtures(): QualitySession[] {
  return QUALITY_FIXTURE_ROWS.map(cloneQualitySession);
}

function loadQualityFixtureRows(rows: QualitySessionFixture[]): QualitySessionFixture[] {
  if (rows.length === 0) {
    throw new Error('quality fixture corpus has no quality_sessions rows');
  }
  assertFixtureNames(rows);
  return rows;
}

function loadQualityFixtureSets(sets: QualityFixtureSet[]): QualityFixtureSet[] {
  assertFixtureSetNames(sets);
  return sets;
}

function assertFixtureNames(rows: QualitySessionFixture[]) {
  const seen = new Set(rows.map((row) => row.name));
  const known = new Set(Object.values(QualityFixtureName));
  for (const name of Object.values(QualityFixtureName)) {
    if (!seen.has(name)) {
      throw new Error(`quality fixture corpus is missing ${JSON.stringify(name)}`);
    }
  }
  for (const row of rows) {
    if (!known.has(row.name)) {
      throw new Error(`quality fixture corpus has unregistered case ${JSON.stringify(row.name)}`);
    }
  }
}

function assertFixtureSetNames(sets: QualityFixtureSet[]) {
  const rows = new Set(QUALITY_FIXTURE_ROWS.map((row) => row.name));
  const seen = new Set(sets.map((set) => set.name));
  const known = new Set(Object.values(QualityFixtureSetName));
  for (const name of Object.values(QualityFixtureSetName)) {
    if (!seen.has(name)) {
      throw new Error(`quality fixture corpus is missing set ${JSON.stringify(name)}`);
    }
  }
  for (const set of sets) {
    if (!known.has(set.name)) {
      throw new Error(`quality fixture corpus has unregistered set ${JSON.stringify(set.name)}`);
    }
    for (const fixtureName of set.cases) {
      if (!rows.has(fixtureName)) {
        throw new Error(
          `quality fixture set ${JSON.stringify(set.name)} references unknown case ${JSON.stringify(fixtureName)}`,
        );
      }
    }
  }
}

function cloneQualitySession(row: QualitySessionFixture): QualitySession {
  return adaptQualitySessions([toQualitySession(row)])[0]!;
}
