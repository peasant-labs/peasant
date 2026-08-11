import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { newProjectHash, type ProjectSummary } from '@peasant-labs/schema';
import {
  parseStrictYAML,
  requireExactRequiredFields,
  requireRecord,
  requireUniqueNames,
} from '@/test/strictYaml';

const REQUIRED_PICKER_CASE_NAMES = [
  'mounted home picker explains coverage and search',
  'mounted map picker explains coverage and search',
] as const;

const REQUIRED_OUTCOME_CASE_NAMES = [
  'mounted resolved outcome exposes heuristic help',
  'mounted partial outcome exposes heuristic help',
  'mounted failed outcome exposes heuristic help',
] as const;

const COPY_FIELDS = [
  'searchPlaceholder',
  'searchAccessibleName',
  'coverageVisibleLabel',
  'coverageHelpName',
  'coverageHelpText',
  'outcomeHelpName',
  'outcomeSource',
  'outcomeLimit',
] as const;

const PICKER_FIELDS = [
  'name',
  'surface',
  'destination',
  'projectCount',
  'targetProject',
  'targetProjectHash',
  'recordedFiles',
  'totalFiles',
  'searchQuery',
  'expectedCoverageLabel',
  'expectedLinkName',
  'expectedHref',
] as const;

const OUTCOME_FIELDS = [
  'name',
  'outcome',
  'sessionId',
  'transcriptLabel',
  'mapLabel',
  'taskTitle',
  'entryIndex',
  'definition',
] as const;

export type ClarityPickerCase = {
  name: string;
  surface: 'home' | 'map';
  destination: 'changes' | 'map';
  projectCount: number;
  targetProject: string;
  targetProjectHash: string;
  recordedFiles: number;
  totalFiles: number;
  searchQuery: string;
  expectedCoverageLabel: string;
  expectedLinkName: string;
  expectedHref: string;
};

export type ClarityOutcomeCase = {
  name: string;
  outcome: 'resolved' | 'partial' | 'failed';
  sessionId: string;
  transcriptLabel: string;
  mapLabel: string;
  taskTitle: string;
  entryIndex: number;
  definition: string;
};

export type LocalReviewClarityFixture = {
  copy: Record<(typeof COPY_FIELDS)[number], string>;
  pickerCases: ClarityPickerCase[];
  outcomeCases: ClarityOutcomeCase[];
};

export const localReviewClarityFixtureSource = readFileSync(
  resolve(process.cwd(), 'src/test/testdata/local_review_clarity.yaml'),
  'utf8',
);

function requireRequiredNames(
  rows: Record<string, unknown>[],
  requiredNames: readonly string[],
  path: string,
): void {
  requireUniqueNames(rows, path);
  const names = rows.map((row) => row.name);
  if (
    rows.length !== requiredNames.length
    || requiredNames.some((name) => !names.includes(name))
    || names.some((name) => !requiredNames.includes(String(name)))
  ) {
    throw new Error(`${path} must contain exactly ${requiredNames.length} required cases`);
  }
}

function requireNonEmptyStrings(row: Record<string, unknown>, fields: readonly string[], path: string): void {
  for (const field of fields) {
    if (typeof row[field] !== 'string' || row[field].length === 0) {
      throw new Error(`${path}.${field} must be a non-empty string`);
    }
  }
}

export function loadLocalReviewClarityFixture(
  source = localReviewClarityFixtureSource,
): LocalReviewClarityFixture {
  const root = requireRecord(parseStrictYAML(source, 'local review clarity fixture'), 'local review clarity fixture');
  requireExactRequiredFields(root, ['copy', 'pickerCases', 'outcomeCases'], 'local review clarity fixture');

  const copy = requireRecord(root.copy, 'local review clarity fixture.copy');
  requireExactRequiredFields(copy, COPY_FIELDS, 'local review clarity fixture.copy');
  requireNonEmptyStrings(copy, COPY_FIELDS, 'local review clarity fixture.copy');

  if (!Array.isArray(root.pickerCases)) {
    throw new Error('local review clarity fixture.pickerCases must be an array');
  }
  const pickerCases = root.pickerCases.map((value, index) => {
    const path = `local review clarity fixture.pickerCases[${index}]`;
    const row = requireRecord(value, path);
    requireExactRequiredFields(row, PICKER_FIELDS, path);
    requireNonEmptyStrings(
      row,
      PICKER_FIELDS.filter((field) => !['projectCount', 'recordedFiles', 'totalFiles'].includes(field)),
      path,
    );
    if (!['home', 'map'].includes(String(row.surface)) || !['changes', 'map'].includes(String(row.destination))) {
      throw new Error(`${path} has an invalid surface or destination`);
    }
    if ((row.surface === 'home') !== (row.destination === 'changes')) {
      throw new Error(`${path} must keep the mounted surface and destination paired`);
    }
    if (!Number.isSafeInteger(row.projectCount) || Number(row.projectCount) < 9) {
      throw new Error(`${path}.projectCount must mount the search threshold`);
    }
    if (
      !Number.isSafeInteger(row.recordedFiles)
      || !Number.isSafeInteger(row.totalFiles)
      || Number(row.recordedFiles) < 0
      || Number(row.totalFiles) <= 0
      || Number(row.recordedFiles) > Number(row.totalFiles)
    ) {
      throw new Error(`${path} has invalid coverage counts`);
    }
    if (!/^[0-9a-f]{64}$/.test(String(row.targetProjectHash))) {
      throw new Error(`${path}.targetProjectHash must be a canonical project hash`);
    }
    return row;
  });
  requireRequiredNames(pickerCases, REQUIRED_PICKER_CASE_NAMES, 'local review clarity fixture.pickerCases');

  if (!Array.isArray(root.outcomeCases)) {
    throw new Error('local review clarity fixture.outcomeCases must be an array');
  }
  const outcomeCases = root.outcomeCases.map((value, index) => {
    const path = `local review clarity fixture.outcomeCases[${index}]`;
    const row = requireRecord(value, path);
    requireExactRequiredFields(row, OUTCOME_FIELDS, path);
    requireNonEmptyStrings(row, OUTCOME_FIELDS.filter((field) => field !== 'entryIndex'), path);
    if (!['resolved', 'partial', 'failed'].includes(String(row.outcome))) {
      throw new Error(`${path}.outcome must be a supported ingest-time outcome`);
    }
    if (!Number.isSafeInteger(row.entryIndex) || Number(row.entryIndex) < 0) {
      throw new Error(`${path}.entryIndex must be a nonnegative integer`);
    }
    return row;
  });
  requireRequiredNames(outcomeCases, REQUIRED_OUTCOME_CASE_NAMES, 'local review clarity fixture.outcomeCases');
  if (new Set(outcomeCases.map((row) => row.outcome)).size !== REQUIRED_OUTCOME_CASE_NAMES.length) {
    throw new Error('local review clarity fixture.outcomeCases must cover each outcome exactly once');
  }
  if (new Set(outcomeCases.map((row) => row.entryIndex)).size !== outcomeCases.length) {
    throw new Error('local review clarity fixture.outcomeCases must use unique entry indices');
  }
  if (new Set(outcomeCases.map((row) => row.sessionId)).size !== outcomeCases.length) {
    throw new Error('local review clarity fixture.outcomeCases must use unique session ids');
  }

  return {
    copy: copy as LocalReviewClarityFixture['copy'],
    pickerCases: pickerCases as unknown as ClarityPickerCase[],
    outcomeCases: outcomeCases as unknown as ClarityOutcomeCase[],
  };
}

export function makeClarityProjectSummaries(testCase: ClarityPickerCase): ProjectSummary[] {
  const target: ProjectSummary = {
    projectHash: newProjectHash(testCase.targetProjectHash),
    project: testCase.targetProject,
    sessions: 1,
    recordedFiles: testCase.recordedFiles,
    totalFiles: testCase.totalFiles,
    lastWorkMs: 1_800_000_000_000,
    openChanges: 2,
  };
  const fillers = Array.from({ length: testCase.projectCount - 1 }, (_, index): ProjectSummary => ({
    projectHash: newProjectHash((index + 1).toString(16).padStart(64, '0')),
    project: `/work/fixture-project-${index + 1}`,
    sessions: 1,
    recordedFiles: index % 2,
    totalFiles: 2,
    lastWorkMs: 1_700_000_000_000 - index,
    openChanges: 0,
  }));
  return [target, ...fillers];
}

export const localReviewClarityFixture = loadLocalReviewClarityFixture();
