import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import {
  Harness,
  newProjectHash,
  zReviewListPayload,
  zSessionDetailPayload,
  zSessionSummary,
  type ProjectHash,
  type ReviewListPayload,
  type SessionDetailPayload,
  type SessionSummary,
} from '@peasant-labs/schema';
import {
  parseStrictYAML,
  requireExactRequiredFields,
  requireRecord,
} from '@/test/strictYaml';

export type StrikeMountedWebFixture = {
  projectHash: ProjectHash;
  projectName: string;
  sessionDetail: SessionDetailPayload;
  mapSession: SessionSummary;
  mapCommit: {
    hash: string;
    subject: string;
    timeMs: number;
  };
  reviewList: ReviewListPayload;
  expected: {
    assistantContent: string;
    mapConversationTitle: string;
    reviewSessionTitle: string;
  };
};

function requiredString(record: Record<string, unknown>, field: string, path: string): string {
  const value = record[field];
  if (typeof value !== 'string' || value.length === 0) {
    throw new Error(`${path}.${field} must be a non-empty string`);
  }
  return value;
}

function requiredInteger(record: Record<string, unknown>, field: string, path: string): number {
  const value = record[field];
  if (typeof value !== 'number' || !Number.isSafeInteger(value)) {
    throw new Error(`${path}.${field} must be a safe integer`);
  }
  return value;
}

export function loadStrikeMountedWebFixture(): StrikeMountedWebFixture {
  const source = readFileSync(
    resolve(process.cwd(), '../internal/mock/testdata/strike_mounted_web.yaml'),
    'utf8',
  );
  const root = requireRecord(
    parseStrictYAML(source, 'mounted Strike web fixture'),
    'mounted Strike web fixture',
  );
  requireExactRequiredFields(
    root,
    ['project', 'sessionDetail', 'mapSession', 'mapCommit', 'reviewList', 'expected'],
    'mounted Strike web fixture',
  );

  const project = requireRecord(root.project, 'mounted Strike web fixture.project');
  requireExactRequiredFields(project, ['hash', 'name'], 'mounted Strike web fixture.project');
  const projectHash = newProjectHash(requiredString(project, 'hash', 'mounted Strike web fixture.project'));
  const projectName = requiredString(project, 'name', 'mounted Strike web fixture.project');

  const sessionDetail = zSessionDetailPayload.strict().parse(root.sessionDetail);
  const mapSession = zSessionSummary.strict().parse(root.mapSession);
  const reviewList = zReviewListPayload.strict().parse(root.reviewList);

  const mapCommitValue = requireRecord(root.mapCommit, 'mounted Strike web fixture.mapCommit');
  requireExactRequiredFields(
    mapCommitValue,
    ['hash', 'subject', 'timeMs'],
    'mounted Strike web fixture.mapCommit',
  );
  const mapCommit = {
    hash: requiredString(mapCommitValue, 'hash', 'mounted Strike web fixture.mapCommit'),
    subject: requiredString(mapCommitValue, 'subject', 'mounted Strike web fixture.mapCommit'),
    timeMs: requiredInteger(mapCommitValue, 'timeMs', 'mounted Strike web fixture.mapCommit'),
  };

  const expectedValue = requireRecord(root.expected, 'mounted Strike web fixture.expected');
  requireExactRequiredFields(
    expectedValue,
    ['assistantContent', 'mapConversationTitle', 'reviewSessionTitle'],
    'mounted Strike web fixture.expected',
  );
  const expected = {
    assistantContent: requiredString(expectedValue, 'assistantContent', 'mounted Strike web fixture.expected'),
    mapConversationTitle: requiredString(expectedValue, 'mapConversationTitle', 'mounted Strike web fixture.expected'),
    reviewSessionTitle: requiredString(expectedValue, 'reviewSessionTitle', 'mounted Strike web fixture.expected'),
  };

  const reviewSession = reviewList.sessions.find(
    (session) => session.sessionId === sessionDetail.id,
  );
  if (
    sessionDetail.harness !== Harness.Strike ||
    mapSession.harness !== Harness.Strike ||
    reviewSession?.harness !== Harness.Strike
  ) {
    throw new Error('mounted Strike web fixture must preserve the canonical Strike harness across transcript, Map, and Review payloads');
  }
  if (
    mapSession.id !== sessionDetail.id ||
    mapSession.project !== projectName ||
    mapSession.projectHash !== projectHash ||
    reviewList.projectHash !== projectHash ||
    sessionDetail.project !== projectName
  ) {
    throw new Error('mounted Strike web fixture project and session identities must agree across every production surface');
  }
  if (
    !sessionDetail.turns?.some((turn) => turn.content === expected.assistantContent) ||
    mapSession.preview !== expected.mapConversationTitle ||
    reviewSession?.title !== expected.reviewSessionTitle
  ) {
    throw new Error('mounted Strike web fixture observable expectations must be backed by the canonical payload rows');
  }

  return {
    projectHash,
    projectName,
    sessionDetail,
    mapSession,
    mapCommit,
    reviewList,
    expected,
  };
}

export const strikeMountedWebFixture = loadStrikeMountedWebFixture();
