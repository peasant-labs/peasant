/** Types for publish-popup-states.mjs, the fixture loader shared with the visual harness. */

export interface PublishStatesCollective {
  id: string;
  name: string;
  members: number;
  acceptance: 'open' | 'verified_only' | 'curated';
  role?: 'owner' | 'member' | 'contributor';
  suggestion?: { reason: 'linked_repository' | 'linked_github_org'; match: string };
}

export interface PublishStatesMatch {
  category: 'CREDENTIAL' | 'PII' | 'PATH' | 'INTERNAL';
  rule: string;
  display: string;
  original: string;
  replacement: string;
  entryIndex: number;
  count?: number;
}

export interface PublishStatesExpect {
  bar: { state: string; text: string; action: 'publish' | 'update' | 'manage' } | { alert: string; retry: boolean };
  popup: null | {
    heading: string;
    texts: string[];
    link?: { text: string; href: string };
    primary: null | { label: string; enabled: boolean };
  };
  request: null | { add: string[]; remove: string[] };
}

export interface PublishStatesCase {
  name: string;
  publication: {
    state: 'unpublished' | 'published';
    outsideSelection?: boolean;
    autoPublish?: boolean;
    newTurns?: number;
    audience?: string[];
    audienceRead?: 'ok' | 'unreachable' | 'missing' | 'failed';
  };
  signIn: 'signed-in' | 'signed-out' | 'waits' | 'login-failed' | 'already-authenticated';
  village: string[] | 'unreachable';
  scan: 'matches' | 'failure' | 'pending' | 'matches-then-failure';
  push: string;
  steps: string[];
  expect: PublishStatesExpect;
}

export interface PublishStatesFixture {
  requiredNames: string[];
  mobileCases: string[];
  wizardLinks: string[];
  collectives: PublishStatesCollective[];
  matches: PublishStatesMatch[];
  pullRequest: { owner: string; name: string; number: number };
  cases: PublishStatesCase[];
}

export interface PublishWorldRequest {
  method: string;
  url: string;
  body?: unknown;
}

export type PublishWorldAnswer =
  | { status: number; json: unknown }
  | { status: number; text: string }
  | { pending: true };

export interface PublishWorld {
  pushRequests: unknown[];
  scanRequests: number;
  respond: (request: PublishWorldRequest) => PublishWorldAnswer | null;
}

export const FIXTURE_VILLAGE: string;
export const FIXTURE_TRANSCRIPT_ID: string;
export const FIXTURE_TRANSCRIPT_URL: string;

export function loadPublishStates(source: string): PublishStatesFixture;
export function collectiveId(index: number): string;
export function publishedAtFor(turns: readonly { timestamp: string }[], newTurns: number): string;
export function createPublishWorld(
  fixture: PublishStatesFixture,
  entry: PublishStatesCase,
  context: { sessionId: string; turns: readonly { timestamp: string }[] },
): PublishWorld;
export function expectedPushBody(fixture: PublishStatesFixture, entry: PublishStatesCase, sessionId: string): unknown;
