import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { newProjectHash, type ProjectSummary } from '@peasant-labs/schema';
import type { DecodedProjectSummariesPayload } from '@/lib/api/map';
import {
  parseStrictYAML,
  requireExactRequiredFields,
  requireRecord,
  requireUniqueNames,
} from '@/test/strictYaml';
import { ProjectListState } from './SelectionRecoveryPanel';

const REQUIRED_CASE_NAMES = [
  'all hidden by saved selection',
  'genuine no data',
  'explicit session makes parent visible',
] as const;

export type ProjectViewerStateCaseName = (typeof REQUIRED_CASE_NAMES)[number];

export interface ProjectViewerStateFixture {
  name: ProjectViewerStateCaseName;
  expectedState: ProjectListState;
  expectedParentLabel: string | null;
  forbiddenIdentities: string[];
  summary: DecodedProjectSummariesPayload;
}

function requiredString(value: unknown, path: string): string {
  if (typeof value !== 'string' || value.length === 0) {
    throw new Error(`${path} must be a non-empty string`);
  }
  return value;
}

function nonNegativeInteger(value: unknown, path: string): number {
  if (!Number.isInteger(value) || (value as number) < 0) {
    throw new Error(`${path} must be a non-negative integer`);
  }
  return value as number;
}

function projectSummary(value: unknown, path: string): ProjectSummary {
  const row = requireRecord(value, path);
  requireExactRequiredFields(
    row,
    [
      'projectHash',
      'project',
      'sessions',
      'recordedFiles',
      'totalFiles',
      'lastWorkMs',
      'openChanges',
    ],
    path,
  );
  return {
    projectHash: newProjectHash(requiredString(row.projectHash, `${path}.projectHash`)),
    project: requiredString(row.project, `${path}.project`),
    sessions: nonNegativeInteger(row.sessions, `${path}.sessions`),
    recordedFiles: nonNegativeInteger(row.recordedFiles, `${path}.recordedFiles`),
    totalFiles: nonNegativeInteger(row.totalFiles, `${path}.totalFiles`),
    lastWorkMs: nonNegativeInteger(row.lastWorkMs, `${path}.lastWorkMs`),
    openChanges: nonNegativeInteger(row.openChanges, `${path}.openChanges`),
  };
}

function loadProjectViewerStateFixtures(): ProjectViewerStateFixture[] {
  const source = readFileSync(
    resolve(process.cwd(), 'src/components/picker/testdata/project_viewer_states.yaml'),
    'utf8',
  );
  const root = requireRecord(
    parseStrictYAML(source, 'project viewer state fixture'),
    'project viewer state fixture',
  );
  requireExactRequiredFields(root, ['expectedCaseCount', 'cases'], 'project viewer state fixture');
  if (root.expectedCaseCount !== REQUIRED_CASE_NAMES.length) {
    throw new Error(
      `project viewer state fixture expectedCaseCount must equal independently defined ${REQUIRED_CASE_NAMES.length}`,
    );
  }
  if (!Array.isArray(root.cases) || root.cases.length !== REQUIRED_CASE_NAMES.length) {
    throw new Error(
      `project viewer state fixture must contain ${REQUIRED_CASE_NAMES.length} cases`,
    );
  }

  const cases = root.cases.map((value, index) =>
    requireRecord(value, `project viewer state fixture.cases[${index}]`),
  );
  requireUniqueNames(cases, 'project viewer state fixture.cases');

  const decoded = cases.map((testCase, index): ProjectViewerStateFixture => {
    const path = `project viewer state fixture.cases[${index}]`;
    requireExactRequiredFields(
      testCase,
      ['name', 'expectedState', 'expectedParentLabel', 'forbiddenIdentities', 'summary'],
      path,
    );
    const name = requiredString(testCase.name, `${path}.name`);
    if (!REQUIRED_CASE_NAMES.includes(name as ProjectViewerStateCaseName)) {
      throw new Error(`${path}.name has unknown semantic case ${JSON.stringify(name)}`);
    }
    if (!Object.values(ProjectListState).includes(testCase.expectedState as ProjectListState)) {
      throw new Error(`${path}.expectedState is invalid`);
    }
    if (testCase.expectedParentLabel !== null && typeof testCase.expectedParentLabel !== 'string') {
      throw new Error(`${path}.expectedParentLabel must be a string or null`);
    }
    if (!Array.isArray(testCase.forbiddenIdentities)) {
      throw new Error(`${path}.forbiddenIdentities must be an array`);
    }
    const forbiddenIdentities = testCase.forbiddenIdentities.map((identity, identityIndex) =>
      requiredString(identity, `${path}.forbiddenIdentities[${identityIndex}]`),
    );

    const summary = requireRecord(testCase.summary, `${path}.summary`);
    requireExactRequiredFields(summary, ['projects', 'selection'], `${path}.summary`);
    if (!Array.isArray(summary.projects)) {
      throw new Error(`${path}.summary.projects must be an array`);
    }
    const selection = requireRecord(summary.selection, `${path}.summary.selection`);
    requireExactRequiredFields(
      selection,
      ['active', 'hiddenProjects', 'hiddenSessions'],
      `${path}.summary.selection`,
    );
    if (typeof selection.active !== 'boolean') {
      throw new Error(`${path}.summary.selection.active must be a boolean`);
    }

    return {
      name: name as ProjectViewerStateCaseName,
      expectedState: testCase.expectedState as ProjectListState,
      expectedParentLabel: testCase.expectedParentLabel as string | null,
      forbiddenIdentities,
      summary: {
        projects: summary.projects.map((project, projectIndex) =>
          projectSummary(project, `${path}.summary.projects[${projectIndex}]`),
        ),
        selection: {
          active: selection.active,
          hiddenProjects: nonNegativeInteger(
            selection.hiddenProjects,
            `${path}.summary.selection.hiddenProjects`,
          ),
          hiddenSessions: nonNegativeInteger(
            selection.hiddenSessions,
            `${path}.summary.selection.hiddenSessions`,
          ),
        },
      },
    };
  });

  for (const name of REQUIRED_CASE_NAMES) {
    if (!decoded.some((testCase) => testCase.name === name)) {
      throw new Error(`project viewer state fixture is missing required semantic case ${name}`);
    }
  }
  return decoded;
}

export const PROJECT_VIEWER_STATE_FIXTURES = loadProjectViewerStateFixtures();

export function projectViewerStateFixture(
  name: ProjectViewerStateCaseName,
): ProjectViewerStateFixture {
  const fixture = PROJECT_VIEWER_STATE_FIXTURES.find((testCase) => testCase.name === name);
  if (!fixture) {
    throw new Error(`project viewer state fixture ${JSON.stringify(name)} is required but missing`);
  }
  return fixture;
}
