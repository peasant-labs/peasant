import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import {
  assertTimelineNavigationAction,
  type TimelineNavigationAction,
} from '@peasant-labs/fairtrade/graph';
import {
  parseStrictYAML,
  requireExactFields,
  requireExactRequiredFields,
  requireRecord,
  requireUniqueNames,
} from '@/test/strictYaml';
import type { ReviewListPayload } from '@peasant-labs/schema';
import type { ReturnLocation } from '@/lib/navigation/projectRoutes';
import type { TimelineNavigationCommand } from './timelineNavigation';

export type TimelineNavigationExpectation =
  | TimelineNavigationCommand
  | { kind: 'error'; message: string };

export type TimelineNavigationFixtureCase = {
  name: string;
  action: TimelineNavigationAction;
  context: {
    defaultBranch: string | null;
    returnLocation: ReturnLocation | null;
    pagination: {
      cursorAvailable: boolean;
      handlerAvailable: boolean;
    };
  };
  expected: TimelineNavigationExpectation;
  controlName: string | null;
  expectedRouterCalls: string[][] | null;
  mountedReview: ReviewListPayload | null;
};

export type TimelineNavigationMutation = {
  name: string;
  caseName: string;
  operation: 'append-router-call' | 'replace-router-href';
  value: string;
  diagnostic: string;
};

export type TimelineNavigationManifest = {
  expectedCaseCount: number;
  expectedFamilyCount: number;
  expectedMutationCount: number;
  cases: Array<{
    name: string;
    actionType: TimelineNavigationAction['type'];
    sourceKind: 'commit' | 'unlinked' | 'outside-window' | null;
    expectedKind: TimelineNavigationExpectation['kind'];
    mounted: boolean;
  }>;
  mutations: TimelineNavigationMutation[];
};

const fixturePath = resolve(
  process.cwd(),
  'src/app/review/[[...segments]]/testdata/timeline_navigation.yaml',
);
const manifestPath = resolve(
  process.cwd(),
  'src/app/review/[[...segments]]/testdata/timeline_navigation_manifest.yaml',
);

export const timelineNavigationSource = readFileSync(fixturePath, 'utf8');
export const timelineNavigationManifestSource = readFileSync(manifestPath, 'utf8');

export function loadTimelineNavigationManifest(source: string): TimelineNavigationManifest {
  const root = requireRecord(
    parseStrictYAML(source, 'timeline navigation manifest'),
    'timeline navigation manifest',
  );
  requireExactRequiredFields(
    root,
    [
      'expectedCaseCount',
      'expectedFamilyCount',
      'expectedMutationCount',
      'cases',
      'mutations',
    ],
    'timeline navigation manifest',
  );
  if (!Number.isInteger(root.expectedCaseCount) || Number(root.expectedCaseCount) < 1) {
    throw new Error('timeline navigation manifest expectedCaseCount must be a positive integer');
  }
  if (!Number.isInteger(root.expectedFamilyCount) || Number(root.expectedFamilyCount) < 1) {
    throw new Error('timeline navigation manifest expectedFamilyCount must be a positive integer');
  }
  if (!Number.isInteger(root.expectedMutationCount) || Number(root.expectedMutationCount) < 1) {
    throw new Error('timeline navigation manifest expectedMutationCount must be a positive integer');
  }
  if (!Array.isArray(root.cases) || root.cases.length !== root.expectedCaseCount) {
    throw new Error('timeline navigation manifest case count must match expectedCaseCount');
  }
  if (!Array.isArray(root.mutations)) {
    throw new Error('timeline navigation manifest mutations must be an array');
  }
  const cases = root.cases.map((value, index) => {
    const testCase = requireRecord(value, `timeline navigation manifest.cases[${index}]`);
    requireExactRequiredFields(
      testCase,
      ['name', 'actionType', 'sourceKind', 'expectedKind', 'mounted'],
      `timeline navigation manifest.cases[${index}]`,
    );
    requireNonEmptyString(testCase.name, `timeline navigation manifest.cases[${index}].name`);
    requireNonEmptyString(
      testCase.actionType,
      `timeline navigation manifest.cases[${index}].actionType`,
    );
    if (!['open-change', 'open-session', 'open-map', 'show-older'].includes(testCase.actionType)) {
      throw new Error(`timeline navigation manifest.cases[${index}].actionType is unsupported`);
    }
    if (testCase.sourceKind !== null) {
      requireNonEmptyString(
        testCase.sourceKind,
        `timeline navigation manifest.cases[${index}].sourceKind`,
      );
      if (!['commit', 'unlinked', 'outside-window'].includes(testCase.sourceKind)) {
        throw new Error(`timeline navigation manifest.cases[${index}].sourceKind is unsupported`);
      }
    }
    if (!['navigate', 'show-older', 'stay', 'error'].includes(String(testCase.expectedKind))) {
      throw new Error(`timeline navigation manifest.cases[${index}].expectedKind is unsupported`);
    }
    if (typeof testCase.mounted !== 'boolean') {
      throw new Error(`timeline navigation manifest.cases[${index}].mounted must be boolean`);
    }
    return testCase;
  });
  requireUniqueNames(cases, 'timeline navigation manifest.cases');
  const families = new Set(cases.map((testCase) => `${testCase.actionType}:${testCase.sourceKind ?? ''}`));
  if (families.size !== root.expectedFamilyCount) {
    throw new Error('timeline navigation manifest semantic family count must match expectedFamilyCount');
  }
  const mutations = root.mutations.map((value, index) => {
    const mutation = requireRecord(value, `timeline navigation manifest.mutations[${index}]`);
    requireExactRequiredFields(
      mutation,
      ['name', 'caseName', 'operation', 'value', 'diagnostic'],
      `timeline navigation manifest.mutations[${index}]`,
    );
    requireNonEmptyString(mutation.name, `timeline navigation manifest.mutations[${index}].name`);
    requireNonEmptyString(
      mutation.caseName,
      `timeline navigation manifest.mutations[${index}].caseName`,
    );
    requireNonEmptyString(
      mutation.operation,
      `timeline navigation manifest.mutations[${index}].operation`,
    );
    requireNonEmptyString(mutation.value, `timeline navigation manifest.mutations[${index}].value`);
    requireNonEmptyString(
      mutation.diagnostic,
      `timeline navigation manifest.mutations[${index}].diagnostic`,
    );
    if (!['append-router-call', 'replace-router-href'].includes(mutation.operation)) {
      throw new Error(`timeline navigation manifest.mutations[${index}].operation is unsupported`);
    }
    const target = cases.find((testCase) => testCase.name === mutation.caseName);
    if (!target || !target.mounted || target.expectedKind !== 'navigate') {
      throw new Error(
        `timeline navigation manifest.mutations[${index}].caseName must target a mounted navigation case`,
      );
    }
    return mutation as TimelineNavigationMutation;
  });
  requireUniqueNames(mutations, 'timeline navigation manifest.mutations');
  if (mutations.length !== root.expectedMutationCount) {
    throw new Error('timeline navigation manifest mutation count must match expectedMutationCount');
  }
  return {
    expectedCaseCount: root.expectedCaseCount as number,
    expectedFamilyCount: root.expectedFamilyCount as number,
    expectedMutationCount: root.expectedMutationCount as number,
    cases: cases as TimelineNavigationManifest['cases'],
    mutations,
  };
}

export function loadTimelineNavigationFixture(
  source: string,
  manifest: TimelineNavigationManifest,
): { expectedCaseCount: number; cases: TimelineNavigationFixtureCase[] } {
  const root = requireRecord(
    parseStrictYAML(source, 'timeline navigation fixture'),
    'timeline navigation fixture',
  );
  requireExactRequiredFields(root, ['expectedCaseCount', 'cases'], 'timeline navigation fixture');
  if (root.expectedCaseCount !== manifest.expectedCaseCount) {
    throw new Error('timeline navigation fixture expectedCaseCount must match its independent manifest case count');
  }
  if (!Array.isArray(root.cases) || root.cases.length !== root.expectedCaseCount) {
    throw new Error('timeline navigation fixture case count must match expectedCaseCount');
  }

  const cases = root.cases.map((value, index) => {
    const testCase = requireRecord(value, `timeline navigation fixture.cases[${index}]`);
    requireExactRequiredFields(
      testCase,
      ['name', 'action', 'context', 'expected', 'controlName', 'expectedRouterCalls', 'mountedReview'],
      `timeline navigation fixture.cases[${index}]`,
    );
    requireNonEmptyString(testCase.name, `timeline navigation fixture.cases[${index}].name`);
    assertTimelineNavigationAction(testCase.action);
    validateContext(testCase.context, index);
    validateExpected(testCase.expected, index);
    if (testCase.controlName !== null) {
      requireNonEmptyString(
        testCase.controlName,
        `timeline navigation fixture.cases[${index}].controlName`,
      );
    }
    validateExpectedRouterCalls(testCase.expectedRouterCalls, testCase.expected, index);
    validateMountedReview(testCase.mountedReview, index);
    return testCase;
  });
  requireUniqueNames(cases, 'timeline navigation fixture.cases');

  for (const manifested of manifest.cases) {
    const testCase = cases.find((candidate) => candidate.name === manifested.name);
    if (!testCase) throw new Error(`timeline navigation fixture is missing behavior case ${manifested.name}`);
    const action = testCase.action as TimelineNavigationAction;
    if (action.type !== manifested.actionType) {
      throw new Error(`timeline navigation fixture ${manifested.name} action type differs from its manifest`);
    }
    const sourceKind = action.type === 'open-session' ? action.source.kind : null;
    if (sourceKind !== manifested.sourceKind) {
      throw new Error(`timeline navigation fixture ${manifested.name} source kind differs from its manifest`);
    }
    if ((testCase.expected as TimelineNavigationExpectation).kind !== manifested.expectedKind) {
      throw new Error(`timeline navigation fixture ${manifested.name} expected kind differs from its manifest`);
    }
    const mounted = testCase.mountedReview !== null
      && testCase.controlName !== null
      && testCase.expectedRouterCalls !== null;
    if (mounted !== manifested.mounted) {
      throw new Error(`timeline navigation fixture ${manifested.name} mounted evidence differs from its manifest`);
    }
  }

  return root as unknown as { expectedCaseCount: number; cases: TimelineNavigationFixtureCase[] };
}

const manifest = loadTimelineNavigationManifest(timelineNavigationManifestSource);
export const timelineNavigationFixture = loadTimelineNavigationFixture(
  timelineNavigationSource,
  manifest,
);

function validateContext(value: unknown, index: number): void {
  const context = requireRecord(value, `timeline navigation fixture.cases[${index}].context`);
  requireExactRequiredFields(
    context,
    ['defaultBranch', 'returnLocation', 'pagination'],
    `timeline navigation fixture.cases[${index}].context`,
  );
  if (context.defaultBranch !== null) {
    requireNonEmptyString(
      context.defaultBranch,
      `timeline navigation fixture.cases[${index}].context.defaultBranch`,
    );
  }
  if (context.returnLocation !== null) {
    const location = requireRecord(
      context.returnLocation,
      `timeline navigation fixture.cases[${index}].context.returnLocation`,
    );
    requireExactRequiredFields(
      location,
      ['version', 'origin', 'href'],
      `timeline navigation fixture.cases[${index}].context.returnLocation`,
    );
    if (location.version !== 1 || !['Map', 'Review'].includes(String(location.origin))) {
      throw new Error(`timeline navigation fixture.cases[${index}].context.returnLocation is invalid`);
    }
    requireNonEmptyString(
      location.href,
      `timeline navigation fixture.cases[${index}].context.returnLocation.href`,
    );
  }
  const pagination = requireRecord(
    context.pagination,
    `timeline navigation fixture.cases[${index}].context.pagination`,
  );
  requireExactRequiredFields(
    pagination,
    ['cursorAvailable', 'handlerAvailable'],
    `timeline navigation fixture.cases[${index}].context.pagination`,
  );
  if (typeof pagination.cursorAvailable !== 'boolean' || typeof pagination.handlerAvailable !== 'boolean') {
    throw new Error(`timeline navigation fixture.cases[${index}].context.pagination flags must be boolean`);
  }
}

function validateExpected(value: unknown, index: number): void {
  const expected = requireRecord(value, `timeline navigation fixture.cases[${index}].expected`);
  if (expected.kind === 'navigate') {
    requireExactRequiredFields(
      expected,
      ['kind', 'href'],
      `timeline navigation fixture.cases[${index}].expected`,
    );
    requireNonEmptyString(expected.href, `timeline navigation fixture.cases[${index}].expected.href`);
    return;
  }
  if (expected.kind === 'show-older' || expected.kind === 'stay') {
    requireExactRequiredFields(
      expected,
      ['kind'],
      `timeline navigation fixture.cases[${index}].expected`,
    );
    return;
  }
  if (expected.kind === 'error') {
    requireExactRequiredFields(
      expected,
      ['kind', 'message'],
      `timeline navigation fixture.cases[${index}].expected`,
    );
    requireNonEmptyString(expected.message, `timeline navigation fixture.cases[${index}].expected.message`);
    return;
  }
  throw new Error(`timeline navigation fixture.cases[${index}].expected.kind is unsupported`);
}

function validateExpectedRouterCalls(value: unknown, expectedValue: unknown, index: number): void {
  const expected = requireRecord(expectedValue, `timeline navigation fixture.cases[${index}].expected`);
  if (value === null) {
    if (expected.kind === 'navigate' || expected.kind === 'stay') {
      throw new Error(`timeline navigation fixture.cases[${index}] routable expectation needs exact router calls`);
    }
    return;
  }
  if (!Array.isArray(value)) {
    throw new Error(`timeline navigation fixture.cases[${index}].expectedRouterCalls must be an array or null`);
  }
  value.forEach((call, callIndex) => {
    if (!Array.isArray(call) || call.length !== 1 || typeof call[0] !== 'string' || call[0].length === 0) {
      throw new Error(`timeline navigation fixture.cases[${index}].expectedRouterCalls[${callIndex}] must be one href argument`);
    }
  });
  const required = expected.kind === 'navigate' ? [[expected.href]] : expected.kind === 'stay' ? [] : null;
  if (required === null || JSON.stringify(value) !== JSON.stringify(required)) {
    throw new Error(`timeline navigation fixture.cases[${index}].expectedRouterCalls must exactly match its route expectation`);
  }
}

function validateMountedReview(value: unknown, index: number): void {
  if (value === null) return;
  const review = requireRecord(value, `timeline navigation fixture.cases[${index}].mountedReview`);
  requireExactFields(
    review,
    ['projectHash', 'repoFound', 'defaultBranch', 'changes', 'recentCommits', 'sessions'],
    `timeline navigation fixture.cases[${index}].mountedReview`,
  );
  for (const field of ['projectHash', 'repoFound', 'changes', 'recentCommits', 'sessions']) {
    if (!(field in review)) {
      throw new Error(`timeline navigation fixture.cases[${index}].mountedReview is missing required field ${field}`);
    }
  }
  requireNonEmptyString(review.projectHash, `timeline navigation fixture.cases[${index}].mountedReview.projectHash`);
  if (!/^[0-9a-f]{64}$/.test(review.projectHash)) {
    throw new Error(`timeline navigation fixture.cases[${index}].mountedReview.projectHash must be canonical`);
  }
  if (typeof review.repoFound !== 'boolean') {
    throw new Error(`timeline navigation fixture.cases[${index}].mountedReview.repoFound must be boolean`);
  }
  if ('defaultBranch' in review) {
    requireNonEmptyString(review.defaultBranch, `timeline navigation fixture.cases[${index}].mountedReview.defaultBranch`);
  }
  if (!Array.isArray(review.changes) || !Array.isArray(review.recentCommits) || !Array.isArray(review.sessions)) {
    throw new Error(`timeline navigation fixture.cases[${index}].mountedReview collections must be arrays`);
  }
  review.changes.forEach((value, rowIndex) => validateChange(value, index, rowIndex));
  review.recentCommits.forEach((value, rowIndex) => validateCommit(value, index, rowIndex));
  review.sessions.forEach((value, rowIndex) => validateSession(value, index, rowIndex));
}

function validateChange(value: unknown, caseIndex: number, rowIndex: number): void {
  const path = `timeline navigation fixture.cases[${caseIndex}].mountedReview.changes[${rowIndex}]`;
  const change = requireRecord(value, path);
  const required = ['branch', 'aheadCount', 'behindCount', 'filesChanged', 'sessionCount', 'taskCount', 'newEdges', 'removedEdges', 'violations', 'merged'];
  requireExactFields(change, [...required, 'lastWorkMs', 'mergedAtMs', 'reverted', 'baseHash', 'tipCommitMs', 'mergeCommitHash'], path);
  for (const field of required) if (!(field in change)) throw new Error(`${path} is missing required field ${field}`);
  requireNonEmptyString(change.branch, `${path}.branch`);
  for (const field of required.slice(1, -1)) {
    if (!Number.isFinite(change[field])) throw new Error(`${path}.${field} must be a finite number`);
  }
  if (typeof change.merged !== 'boolean') throw new Error(`${path}.merged must be boolean`);
  for (const field of ['lastWorkMs', 'mergedAtMs', 'tipCommitMs']) {
    if (field in change && !Number.isFinite(change[field])) throw new Error(`${path}.${field} must be a finite number`);
  }
  if ('reverted' in change && typeof change.reverted !== 'boolean') throw new Error(`${path}.reverted must be boolean`);
  for (const field of ['baseHash', 'mergeCommitHash']) {
    if (field in change) requireNonEmptyString(change[field], `${path}.${field}`);
  }
}

function validateCommit(value: unknown, caseIndex: number, rowIndex: number): void {
  const path = `timeline navigation fixture.cases[${caseIndex}].mountedReview.recentCommits[${rowIndex}]`;
  const commit = requireRecord(value, path);
  requireExactFields(commit, ['hash', 'subject', 'timeMs', 'hasSession', 'sessionIds'], path);
  for (const field of ['hash', 'subject', 'hasSession', 'sessionIds']) if (!(field in commit)) throw new Error(`${path} is missing required field ${field}`);
  requireNonEmptyString(commit.hash, `${path}.hash`);
  requireNonEmptyString(commit.subject, `${path}.subject`);
  if (typeof commit.hasSession !== 'boolean' || !Array.isArray(commit.sessionIds) || commit.sessionIds.some((id) => typeof id !== 'string' || id.length === 0)) {
    throw new Error(`${path} has invalid session relation fields`);
  }
  if (commit.hasSession !== (commit.sessionIds.length > 0)) {
    throw new Error(`${path}.hasSession must mirror sessionIds`);
  }
  if ('timeMs' in commit && !Number.isFinite(commit.timeMs)) throw new Error(`${path}.timeMs must be a finite number`);
}

function validateSession(value: unknown, caseIndex: number, rowIndex: number): void {
  const path = `timeline navigation fixture.cases[${caseIndex}].mountedReview.sessions[${rowIndex}]`;
  const session = requireRecord(value, path);
  requireExactFields(session, ['sessionId', 'title', 'harness', 'startMs', 'hasCommitBinding'], path);
  for (const field of ['sessionId', 'title', 'harness', 'hasCommitBinding']) if (!(field in session)) throw new Error(`${path} is missing required field ${field}`);
  requireNonEmptyString(session.sessionId, `${path}.sessionId`);
  requireNonEmptyString(session.title, `${path}.title`);
  requireNonEmptyString(session.harness, `${path}.harness`);
  if (typeof session.hasCommitBinding !== 'boolean') throw new Error(`${path}.hasCommitBinding must be boolean`);
  if ('startMs' in session && !Number.isFinite(session.startMs)) throw new Error(`${path}.startMs must be a finite number`);
}

function requireNonEmptyString(value: unknown, path: string): asserts value is string {
  if (typeof value !== 'string' || value.length === 0) {
    throw new Error(`${path} must be a non-empty string`);
  }
}
