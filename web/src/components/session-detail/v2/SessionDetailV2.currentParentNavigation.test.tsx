import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, waitFor } from '@testing-library/react';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { SessionDetailV2 } from './SessionDetailV2';
import { parseTranscriptRouteQuery, transcriptHref, type ProjectHash } from '@/lib/navigation/projectRoutes';
import { writeTranscriptReadingState } from './lib/transcriptReadingState';
import { parseStrictYAML, requireExactRequiredFields, requireRecord, requireUniqueNames } from '@/test/strictYaml';

const PROJECT_HASH = ('a'.repeat(64)) as ProjectHash;
const CHILD_PATHNAME = `/projects/${PROJECT_HASH}/sess_contextchild`;
const STARTED_BY_LABEL = 'started by';
const CONTEXT_LABEL = 'context inherited from';
const LINK_ACTION = 'open current session';
const RESOLVED = 'resolved';
const KNOWN_UNAVAILABLE = 'known_unavailable';
const RELATIONSHIP_STATUSES = [RESOLVED, KNOWN_UNAVAILABLE];
const LINK_KINDS = ['started_by', 'none'];
const CASE_FIELDS = ['name', 'childSessionId', 'projectName', 'contextFromStatus', 'contextFromLocalId', 'startedByStatus', 'startedByLocalId', 'expectedLinkKind'] as const;
const TRANSITION_FIELDS = ['name', 'childSessionId', 'projectName', 'contextFromStatus', 'contextFromLocalId', 'startedByStatus', 'startedByLocalId', 'storedScrollTop', 'storedActiveTurn', 'storedEarlierOpen', 'expectedEarlierExpanded', 'expectedScrollTop'] as const;

type NavigationCase = {
  name: string;
  childSessionId: string;
  projectName: string;
  contextFromStatus: string;
  contextFromLocalId: string;
  startedByStatus: string;
  startedByLocalId: string;
  expectedLinkKind: string;
};

type NavigationTransition = {
  name: string;
  childSessionId: string;
  projectName: string;
  contextFromStatus: string;
  contextFromLocalId: string;
  startedByStatus: string;
  startedByLocalId: string;
  storedScrollTop: number;
  storedActiveTurn: number;
  storedEarlierOpen: boolean;
  expectedEarlierExpanded: boolean;
  expectedScrollTop: number;
};

const casesSource = readFileSync(resolve(process.cwd(), 'src/components/session-detail/v2/testdata/current_parent_navigation.yaml'), 'utf8');
const manifestSource = readFileSync(resolve(process.cwd(), 'src/components/session-detail/v2/testdata/current_parent_navigation.manifest.yaml'), 'utf8');

function loadFixtures(casesText = casesSource, manifestText = manifestSource): { cases: NavigationCase[]; transitions: NavigationTransition[] } {
  const manifest = requireRecord(parseStrictYAML(manifestText, 'current-parent manifest'), 'current-parent manifest');
  requireExactRequiredFields(manifest, ['expectedCaseCount', 'requiredNames', 'expectedTransitionCount', 'requiredTransitionNames', 'expectedLoaderMutationCount', 'loaderMutations', 'expectedMutationCount', 'mutations'], 'current-parent manifest');
  const requiredNames = manifest.requiredNames as unknown[];
  const requiredTransitionNames = manifest.requiredTransitionNames as unknown[];
  const root = requireRecord(parseStrictYAML(casesText, 'current-parent cases'), 'current-parent cases');
  requireExactRequiredFields(root, ['cases', 'transitions'], 'current-parent cases');
  if (!Array.isArray(root.cases) || !Array.isArray(root.transitions)) throw new Error('current-parent cases and transitions must be arrays');
  const rows = root.cases.map((row, index) => requireRecord(row, `current-parent cases[${index}]`));
  requireUniqueNames(rows, 'current-parent cases');
  rows.forEach((row, index) => {
    requireExactRequiredFields(row, CASE_FIELDS, `current-parent cases[${index}]`);
    if (typeof row.childSessionId !== 'string' || row.childSessionId.length === 0) throw new Error(`current-parent cases[${index}].childSessionId is invalid`);
    if (typeof row.projectName !== 'string' || row.projectName.length === 0) throw new Error(`current-parent cases[${index}].projectName is invalid`);
    if (!RELATIONSHIP_STATUSES.includes(row.contextFromStatus as string) || !RELATIONSHIP_STATUSES.includes(row.startedByStatus as string)) throw new Error(`current-parent cases[${index}] has an unknown relationship status`);
    if (typeof row.contextFromLocalId !== 'string' || row.contextFromLocalId.length === 0) throw new Error(`current-parent cases[${index}].contextFromLocalId is invalid`);
    if (typeof row.startedByLocalId !== 'string' || row.startedByLocalId.length === 0) throw new Error(`current-parent cases[${index}].startedByLocalId is invalid`);
    if (!LINK_KINDS.includes(row.expectedLinkKind as string)) throw new Error(`current-parent cases[${index}].expectedLinkKind is invalid`);
    const expectLink = row.startedByStatus === RESOLVED;
    if (expectLink !== (row.expectedLinkKind === 'started_by')) throw new Error(`current-parent cases[${index}] has inconsistent link expectation for its started_by status`);
  });
  const names = rows.map((row) => row.name);
  const transitions = root.transitions.map((row, index) => requireRecord(row, `current-parent transitions[${index}]`));
  requireUniqueNames(transitions, 'current-parent transitions');
  transitions.forEach((row, index) => {
    requireExactRequiredFields(row, TRANSITION_FIELDS, `current-parent transitions[${index}]`);
    for (const field of ['storedScrollTop', 'storedActiveTurn', 'expectedScrollTop']) {
      if (!Number.isSafeInteger(row[field]) || (row[field] as number) < 0) throw new Error(`current-parent transitions[${index}].${field} must be a safe nonnegative integer`);
    }
    if (typeof row.storedEarlierOpen !== 'boolean' || typeof row.expectedEarlierExpanded !== 'boolean') throw new Error(`current-parent transitions[${index}] disclosure flags must be booleans`);
    if (row.expectedEarlierExpanded !== row.storedEarlierOpen) throw new Error(`current-parent transitions[${index}] must restore the stored disclosure state`);
    if (row.expectedScrollTop !== row.storedScrollTop) throw new Error(`current-parent transitions[${index}] must restore the stored scroll offset`);
  });
  if (rows.length !== manifest.expectedCaseCount || requiredNames.length !== manifest.expectedCaseCount || requiredNames.some((name) => !names.includes(name as string))) throw new Error('current-parent cases do not match their independent manifest');
  const transitionNames = transitions.map((row) => row.name);
  if (transitions.length !== manifest.expectedTransitionCount || requiredTransitionNames.length !== manifest.expectedTransitionCount || requiredTransitionNames.some((name) => !transitionNames.includes(name as string))) throw new Error('current-parent transitions do not match their independent manifest');
  return { cases: rows as unknown as NavigationCase[], transitions: transitions as unknown as NavigationTransition[] };
}

const routerPush = vi.hoisted(() => vi.fn());
const routerReplace = vi.hoisted(() => vi.fn());
let currentSearchParams = new URLSearchParams('origin=Map');
let channelData: unknown;

vi.mock('next/navigation', () => ({
  useSearchParams: () => currentSearchParams,
  usePathname: () => CHILD_PATHNAME,
  useRouter: () => ({ push: routerPush, replace: routerReplace }),
}));
vi.mock('@/contexts/WebSocketContext', () => ({ useChannel: () => ({ data: channelData, connected: true, error: null }) }));
vi.mock('./lib/useEntryLabels', () => ({ useEntryLabels: () => ({ entryTypes: [], labelsByEntry: new Map(), addLabel: vi.fn() }) }));
vi.mock('@/hooks/useTheme', () => ({ useTheme: () => ({ theme: 'dark', setTheme: vi.fn(), toggle: vi.fn() }) }));
vi.mock('@peasant-labs/fairtrade/graph', () => ({ TrajectoryGraph: () => null }));

function childPayload(spec: { childSessionId: string; projectName: string; contextFromStatus: string; contextFromLocalId: string; startedByStatus: string; startedByLocalId: string }) {
  const turns = [0, 1, 2, 3, 4].map((index) => ({
    index,
    role: index % 2 === 0 ? 'user' : 'assistant',
    depth: 0,
    content: `child turn ${index}`,
    timestamp: '2026-08-02T09:00:00Z',
    toolCalls: [],
    sourceEntryRef: `e_contextchild_${index}`,
  }));
  return {
    id: spec.childSessionId,
    project: spec.projectName,
    harness: 'claude-code',
    startTime: '2026-08-02T09:00:00Z',
    endTime: '2026-08-02T09:12:00Z',
    durationMins: 12,
    totalTokens: 1540,
    tokensIn: 1180,
    tokensOut: 360,
    turnCount: 5,
    toolCallCount: 0,
    inputSubmissionCount: 1,
    turns,
    relationships: [
      { kind: 'context_from', targetState: 'target_known', targetLocalId: spec.contextFromLocalId, evidence: 'native_typed' },
      { kind: 'started_by', targetState: 'target_known', targetLocalId: spec.startedByLocalId, evidence: 'native_typed' },
    ],
    earlierHistory: [
      {
        state: 'uncertain_migrated',
        turns: [
          { index: 0, role: 'user', depth: 0, content: 'earlier question', timestamp: '2026-08-01T21:00:00Z', toolCalls: [], sourceEntryRef: 'e_contextchild_earlier_0' },
          { index: 1, role: 'assistant', depth: 0, content: 'earlier answer', timestamp: '2026-08-01T21:04:00Z', toolCalls: [], sourceEntryRef: 'e_contextchild_earlier_1' },
        ],
      },
    ],
    relationshipNavigation: [
      { kind: 'context_from', status: spec.contextFromStatus, localId: spec.contextFromLocalId },
      { kind: 'started_by', status: spec.startedByStatus, ...(spec.startedByStatus === RESOLVED ? { localId: spec.startedByLocalId } : {}) },
    ],
  };
}

function TestDetail({ sessionId, projectName }: { sessionId: string; projectName: string }) {
  const routeQuery = parseTranscriptRouteQuery(currentSearchParams);
  if (!routeQuery) throw new Error('fixture route query must be valid');
  return <SessionDetailV2 sessionId={sessionId} projectHash={PROJECT_HASH} projectName={projectName} routeQuery={routeQuery} />;
}

beforeEach(() => {
  routerPush.mockClear();
  routerReplace.mockClear();
  currentSearchParams = new URLSearchParams('origin=Map');
  window.sessionStorage.clear();
});
afterEach(() => {
  cleanup();
  channelData = undefined;
  vi.restoreAllMocks();
});

describe('mounted current-parent navigation through the real adapter and viewer', () => {
  const fixtureSet = loadFixtures();

  for (const fixture of fixtureSet.cases) {
    it(fixture.name, async () => {
      channelData = childPayload(fixture);
      const view = render(<TestDetail sessionId={fixture.childSessionId} projectName={fixture.projectName} />);
      await waitFor(() => expect(view.container.querySelector('.txn-app')).toBeInTheDocument());

      const contextSection = view.container.querySelector('.txn-context');
      if (!contextSection) throw new Error('the mounted viewer did not render the session context section');
      const contextRow = [...view.container.querySelectorAll('.txn-context-source')].find((node) => node.textContent?.includes(CONTEXT_LABEL));
      if (!contextRow) throw new Error('the mounted viewer did not render the context/source relationship row');
      const starterRow = [...view.container.querySelectorAll('.txn-context-source')].find((node) => node.textContent?.includes(STARTED_BY_LABEL));
      if (!starterRow) throw new Error('the mounted viewer did not render the started_by parent relationship row');

      if (fixture.expectedLinkKind === 'none') {
        expect(starterRow.querySelector('.txn-context-link')).toBeNull();
        expect(routerPush).not.toHaveBeenCalled();
        return;
      }

      const link = starterRow.querySelector('button.txn-context-link');
      if (!link) throw new Error('the mounted viewer did not render an active link for the resolved parent target');
      expect(link.textContent?.trim()).toBe(LINK_ACTION);
      fireEvent.click(link);

      await waitFor(() => {
        const calls = routerPush.mock.calls;
        const expected = transcriptHref(PROJECT_HASH, fixture.startedByLocalId);
        if (calls.length === 0) throw new Error('expected the current-parent link to route to the exact stored target; no route was pushed');
        const pushed = String(calls[calls.length - 1]![0]);
        if (pushed !== expected) throw new Error(`expected the current-parent link to route to the exact stored target ${expected}, received ${pushed}`);
      });
    });
  }

  for (const fixture of fixtureSet.transitions) {
    it(fixture.name, async () => {
      channelData = childPayload(fixture);
      writeTranscriptReadingState(fixture.childSessionId, {
        scrollTop: fixture.storedScrollTop,
        activeTurn: fixture.storedActiveTurn,
        earlierHistoryOpen: { 'earlier-0': fixture.storedEarlierOpen },
      });

      const view = render(<TestDetail sessionId={fixture.childSessionId} projectName={fixture.projectName} />);
      await waitFor(() => expect(view.container.querySelector('.txn-app')).toBeInTheDocument());

      await waitFor(() => {
        const toggle = view.container.querySelector('.txn-earlier-toggle');
        if (!toggle) throw new Error('the mounted viewer did not render the earlier-history disclosure');
        if (toggle.getAttribute('aria-expanded') !== String(fixture.expectedEarlierExpanded)) {
          throw new Error(`the earlier-history disclosure was not restored from the child's stored reading state (aria-expanded=${toggle.getAttribute('aria-expanded')})`);
        }
      });

      await waitFor(() => {
        const scroller = view.container.querySelector('.txn-stream') as HTMLElement | null;
        if (!scroller) throw new Error('the mounted viewer did not render the inner stream');
        if (scroller.scrollTop !== fixture.expectedScrollTop) {
          throw new Error(`the inner stream scroll offset was not restored from the child's stored reading state (scrollTop=${scroller.scrollTop})`);
        }
      });
    });
  }
});
