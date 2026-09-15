import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { SessionDetailV2 } from './SessionDetailV2';
import { parseTranscriptRouteQuery, transcriptHref, type ProjectHash } from '@/lib/navigation/projectRoutes';
import { parseStrictYAML, requireExactFields, requireExactRequiredFields, requireRecord, requireUniqueNames } from '@/test/strictYaml';

/**
 * The production session-detail component mounted over the REAL published
 * Fairtrade package: the shipped `adaptTranscript` adapter and the shipped
 * `TranscriptViewer` composite, not a stand-in. The cases live in
 * `testdata/mounted_context_navigation.yaml`; the manifest in
 * `mounted_context_navigation.manifest.yaml` names every required case and the
 * production mutations each must survive.
 */

const PROJECT_HASH = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' as ProjectHash;
const PATHNAME = '/projects/alpha-project/sess-context-nav';
const routerPush = vi.hoisted(() => vi.fn());
const routerReplace = vi.hoisted(() => vi.fn());

let currentSearchParams = new URLSearchParams();
let channelData: unknown;

vi.mock('next/navigation', () => ({
  useSearchParams: () => currentSearchParams,
  usePathname: () => PATHNAME,
  useRouter: () => ({ push: routerPush, replace: routerReplace }),
}));

vi.mock('@/contexts/WebSocketContext', () => ({
  useChannel: () => ({ data: channelData, connected: true, error: null }),
}));

vi.mock('./lib/useEntryLabels', () => ({
  useEntryLabels: () => ({ entryTypes: [], labelsByEntry: new Map(), addLabel: vi.fn() }),
}));

vi.mock('@/hooks/useTheme', () => ({
  useTheme: () => ({ theme: 'dark', setTheme: vi.fn(), toggle: vi.fn() }),
}));

vi.mock('@peasant-labs/fairtrade/graph', () => ({ TrajectoryGraph: () => null }));

const fixtureDir = 'src/components/session-detail/v2/testdata';
const manifestSource = readFileSync(resolve(process.cwd(), fixtureDir, 'mounted_context_navigation.manifest.yaml'), 'utf8');
const casesSource = readFileSync(resolve(process.cwd(), fixtureDir, 'mounted_context_navigation.yaml'), 'utf8');

type HeaderRow = { label: string; link: boolean; status?: string };
type ContextCase = {
  name: string;
  search: string;
  relationships?: Array<Record<string, unknown>>;
  navigation?: Array<Record<string, unknown>>;
  earlierSections?: Array<{ state: string; turns: Array<{ index: number; content: string }> }>;
  expectedHeader: HeaderRow[];
  expectedPushLocalId: string;
  expectedPushRowIndex: number;
  expectedEarlierCount?: number;
};
type LoaderMutation = { name: string; target: 'manifest' | 'cases'; find: string; replace: string; expectedError: string };
type ProductionMutation = {
  name: string;
  find: string;
  replace: string;
  expectedFailedTestNames: string[];
  expectedFailurePattern: string;
};

const manifestFields = ['expectedCaseCount', 'requiredNames', 'expectedLoaderMutationCount', 'loaderMutations', 'expectedMutationCount', 'mutations'];
const caseFields = ['name', 'search', 'relationships', 'navigation', 'earlierSections', 'expectedHeader', 'expectedPushLocalId', 'expectedPushRowIndex', 'expectedEarlierCount'];
const headerFields = ['label', 'link', 'status'];
const loaderMutationFields = ['name', 'target', 'find', 'replace', 'expectedError'];
const mutationFields = ['name', 'find', 'replace', 'expectedTestFile', 'expectedFailedTestNames', 'expectedFailurePattern'];

const RELATIONSHIP_KINDS = ['context_from', 'started_by'];
const TARGET_STATES = ['target_known', 'target_known_retained', 'unknown', 'conflicting_current_native_evidence', 'explicit_none'];
const NAVIGATION_STATUSES = ['resolved', 'known_unavailable', 'inaccessible', 'unknown', 'conflicting', 'general_link_only'];
const EARLIER_STATES = ['uncertain_migrated', 'uncertain_unresolved'];
const SEARCH_GRAMMAR = /^(?:[?&]?[A-Za-z]+=[^&=]*)?(?:&[A-Za-z]+=[^&=]*)*$/;

function replaceExactlyOnce(source: string, find: string, replacement: string, label: string): string {
  const occurrences = source.split(find).length - 1;
  if (occurrences !== 1) throw new Error(`${label} mutation anchor must occur exactly once, found ${occurrences}`);
  return source.replace(find, replacement);
}

function strictClosedSet(value: unknown, allowed: readonly string[], label: string): string {
  if (typeof value !== 'string' || !allowed.includes(value)) {
    throw new Error(`${label} must be one of ${allowed.join(', ')}, received ${JSON.stringify(value)}`);
  }
  return value;
}

function loadCases(manifestText = manifestSource, casesText = casesSource): {
  cases: ContextCase[];
  loaderMutations: LoaderMutation[];
  mutations: ProductionMutation[];
} {
  const manifest = requireRecord(parseStrictYAML(manifestText, 'context navigation manifest'), 'context navigation manifest');
  requireExactRequiredFields(manifest, manifestFields, 'context navigation manifest');
  if (!Array.isArray(manifest.requiredNames) || manifest.requiredNames.some((name) => typeof name !== 'string' || name === '')) {
    throw new Error('context navigation manifest.requiredNames must be nonempty strings');
  }
  const requiredNames = manifest.requiredNames as string[];
  if (new Set(requiredNames).size !== requiredNames.length) throw new Error('context navigation manifest.requiredNames must be unique');
  const loaderMutations = (manifest.loaderMutations as unknown[]).map((row, index) => {
    const mutation = requireRecord(row, `context navigation manifest.loaderMutations[${index}]`);
    requireExactRequiredFields(mutation, loaderMutationFields, `context navigation manifest.loaderMutations[${index}]`);
    if (mutation.target !== 'manifest' && mutation.target !== 'cases') throw new Error(`context navigation loader mutation ${index} names an unknown target`);
    for (const field of ['find', 'replace', 'expectedError']) {
      if (typeof mutation[field] !== 'string' || (mutation[field] as string).length === 0) throw new Error(`context navigation loader mutation ${index}.${field} must be a nonempty string`);
    }
    return mutation as LoaderMutation;
  });
  if (loaderMutations.length !== manifest.expectedLoaderMutationCount) throw new Error('context navigation loader mutation count drift');
  const mutations = (manifest.mutations as unknown[]).map((row, index) => {
    const mutation = requireRecord(row, `context navigation manifest.mutations[${index}]`);
    requireExactRequiredFields(mutation, mutationFields, `context navigation manifest.mutations[${index}]`);
    if (typeof mutation.find !== 'string' || typeof mutation.replace !== 'string' || typeof mutation.expectedFailurePattern !== 'string' || typeof mutation.expectedTestFile !== 'string' || mutation.expectedTestFile === '') {
      throw new Error(`context navigation production mutation ${index} requires string find/replace/expectedTestFile/expectedFailurePattern`);
    }
    const names = mutation.expectedFailedTestNames;
    if (!Array.isArray(names) || names.length === 0 || names.some((name) => typeof name !== 'string' || name === '')) {
      throw new Error(`context navigation production mutation ${index} requires expectedFailedTestNames`);
    }
    return mutation as unknown as ProductionMutation;
  });
  if (mutations.length !== manifest.expectedMutationCount) throw new Error('context navigation production mutation count drift');
  const mutationNames = [...loaderMutations.map((row) => row.name), ...mutations.map((row) => row.name)];
  if (new Set(mutationNames).size !== mutationNames.length) throw new Error('context navigation mutation names must be unique');

  const root = requireRecord(parseStrictYAML(casesText, 'context navigation cases'), 'context navigation cases');
  requireExactRequiredFields(root, ['cases'], 'context navigation cases');
  if (!Array.isArray(root.cases)) throw new Error('context navigation cases must be an array');
  const rows = root.cases.map((value, index) => requireRecord(value, `context navigation cases[${index}]`));
  requireUniqueNames(rows, 'context navigation cases');
  rows.forEach((row, index) => {
    requireExactFields(row, caseFields, `context navigation cases[${index}]`);
    for (const required of ['name', 'search', 'expectedHeader']) {
      if (!(required in row)) throw new Error(`context navigation cases[${index}] is missing required fields: ${required}`);
    }
    if (row.expectedPushLocalId === undefined) row.expectedPushLocalId = '';
    if (row.expectedPushRowIndex === undefined) row.expectedPushRowIndex = 0;
    if (typeof row.search !== 'string' || !SEARCH_GRAMMAR.test(row.search) || typeof row.expectedPushLocalId !== 'string') {
      throw new Error(`context navigation cases[${index}] has invalid search or push target`);
    }
    for (const key of ['relationships', 'navigation', 'earlierSections'] as const) {
      if (row[key] !== undefined && !Array.isArray(row[key])) throw new Error(`context navigation cases[${index}].${key} must be an array`);
    }
    for (const relationship of (row.relationships ?? []) as Array<Record<string, unknown>>) {
      if (!RELATIONSHIP_KINDS.includes(String(relationship.kind))) throw new Error(`context navigation cases[${index}] names an unknown relationship kind`);
      strictClosedSet(relationship.targetState, TARGET_STATES, `context navigation cases[${index}] relationship targetState`);
    }
    for (const navigation of (row.navigation ?? []) as Array<Record<string, unknown>>) {
      if (!RELATIONSHIP_KINDS.includes(String(navigation.kind))) throw new Error(`context navigation cases[${index}] names an unknown navigation kind`);
      strictClosedSet(navigation.status, NAVIGATION_STATUSES, `context navigation cases[${index}] navigation status`);
    }
    for (const section of (row.earlierSections ?? []) as Array<Record<string, unknown>>) {
      strictClosedSet(section.state, EARLIER_STATES, `context navigation cases[${index}] earlier state`);
      if (!Array.isArray(section.turns) || section.turns.length === 0) throw new Error(`context navigation cases[${index}] earlier section needs turns`);
    }
    if (row.expectedEarlierCount !== undefined && (!Number.isSafeInteger(row.expectedEarlierCount) || (row.expectedEarlierCount as number) < 1)) {
      throw new Error(`context navigation cases[${index}].expectedEarlierCount must be a positive integer`);
    }
    if ((row.earlierSections === undefined) !== (row.expectedEarlierCount === undefined)) {
      throw new Error(`context navigation cases[${index}] must pair earlierSections with expectedEarlierCount`);
    }
    if (!Array.isArray(row.expectedHeader)) throw new Error(`context navigation cases[${index}].expectedHeader must be an array`);
    const header = (row.expectedHeader as unknown[]).map((value, headerIndex) => {
      const entry = requireRecord(value, `context navigation cases[${index}].expectedHeader[${headerIndex}]`);
      const allowed = entry.status === undefined ? ['label', 'link'] : headerFields;
      requireExactRequiredFields(entry, allowed, `context navigation cases[${index}].expectedHeader[${headerIndex}]`);
      if (typeof entry.label !== 'string' || typeof entry.link !== 'boolean') throw new Error(`context navigation cases[${index}].expectedHeader[${headerIndex}] needs a label and a link flag`);
      if (entry.link === false && typeof entry.status !== 'string') throw new Error(`context navigation cases[${index}].expectedHeader[${headerIndex}] must state the status of a non-link row`);
      return entry as unknown as HeaderRow;
    });
    const linkable = header.filter((entry) => entry.link).length;
    if (linkable > 0 && row.expectedPushLocalId === '') throw new Error(`context navigation cases[${index}] links a target but names no push target`);
    if (linkable === 0 && row.expectedPushLocalId !== '') throw new Error(`context navigation cases[${index}] names a push target but renders no link`);
    if (linkable > 0) {
      const pushRow = row.expectedPushRowIndex as number;
      if (!Number.isSafeInteger(pushRow) || pushRow < 0 || pushRow >= header.length || !header[pushRow].link) {
        throw new Error(`context navigation cases[${index}] names a push row that is not a link`);
      }
    }
  });
  const names = rows.map((row) => row.name as string);
  if (rows.length !== manifest.expectedCaseCount || requiredNames.length !== manifest.expectedCaseCount) {
    throw new Error('context navigation cases do not match their independent manifest count');
  }
  if (requiredNames.some((name) => !names.includes(name)) || names.some((name) => !requiredNames.includes(name))) {
    throw new Error('context navigation cases do not match their independent manifest names');
  }
  return { cases: rows as unknown as ContextCase[], loaderMutations, mutations };
}

function TestDetail() {
  const routeQuery = parseTranscriptRouteQuery(currentSearchParams);
  if (!routeQuery) throw new Error('context navigation route query must be valid');
  return <SessionDetailV2 sessionId="sess-context-nav" projectHash={PROJECT_HASH} projectName="alpha-project" routeQuery={routeQuery} />;
}

function channelPayloadFor(fixture: ContextCase): unknown {
  const timestamp = '2026-07-15T08:00:00Z';
  return {
    id: 'sess-context-nav',
    project: 'alpha-project',
    harness: 'codex',
    startTime: timestamp,
    endTime: '2026-07-15T08:01:00Z',
    durationMins: 1,
    totalTokens: 12,
    tokensIn: 5,
    tokensOut: 7,
    turnCount: 1,
    toolCallCount: 0,
    turns: [{ index: 0, role: 'user', depth: 0, content: 'turn 0', timestamp, toolCalls: [] }],
    ...(fixture.relationships ? { relationships: fixture.relationships } : {}),
    ...(fixture.navigation ? { relationshipNavigation: fixture.navigation } : {}),
    ...(fixture.earlierSections
      ? {
          earlierHistory: fixture.earlierSections.map((section) => ({
            state: section.state,
            turns: section.turns.map((turn) => ({
              index: turn.index,
              role: 'user',
              depth: 0,
              content: turn.content,
              timestamp,
              entryType: 'text',
              sourceEntryRef: `e_old_${turn.index}`,
              provenance: {
                origin: 'submitted_input',
                actor: 'unknown',
                delivery: 'inherited_context',
                ownership: 'uncertain',
                evidence: 'retained_last_good',
                inputModality: 'text',
                submissionRef: `s_old_${turn.index}`,
              },
            })),
          })),
        }
      : {}),
  };
}

function headerRows(container: HTMLElement): HTMLElement[] {
  return [...container.querySelectorAll<HTMLElement>('.txn-context-row')];
}

/** Every mounted assertion fails with one searchable invariant prefix. */
function invariant(condition: unknown, detail: string): asserts condition {
  if (!condition) throw new Error(`mounted context navigation invariant failed: ${detail}`);
}

function assertHeader(container: HTMLElement, expected: HeaderRow[]): HTMLElement[] {
  const rows = headerRows(container);
  invariant(rows.length === expected.length, `expected ${expected.length} context header rows, rendered ${rows.length}`);
  expected.forEach((entry, index) => {
    const label = rows[index].querySelector('span')?.textContent;
    invariant(label === entry.label, `row ${index} label ${JSON.stringify(label)} is not ${JSON.stringify(entry.label)}`);
    const link = rows[index].querySelector<HTMLButtonElement>('.txn-context-link');
    const status = rows[index].querySelector<HTMLElement>('.txn-context-status');
    if (entry.link) {
      invariant(link !== null && status === null, `row ${index} must offer a link and no status label`);
      invariant(link?.textContent === 'open current session', `row ${index} link label drifted`);
    } else {
      invariant(link === null, `row ${index} must not offer a link`);
      invariant(status?.textContent === entry.status, `row ${index} status ${JSON.stringify(status?.textContent)} is not ${JSON.stringify(entry.status)}`);
    }
  });
  return rows;
}

function assertEarlierCount(container: HTMLElement, expected: number): HTMLButtonElement[] {
  const toggles = [...container.querySelectorAll<HTMLButtonElement>('.txn-earlier-toggle')];
  invariant(toggles.length === expected, `expected ${expected} retained-history sections, rendered ${toggles.length}`);
  invariant(toggles[0]?.textContent?.includes('earlier history') === true, 'retained-history disclosure label drifted');
  return toggles;
}

beforeEach(() => {
  routerPush.mockClear();
  routerReplace.mockClear();
  if (typeof HTMLElement.prototype.scrollIntoView !== 'function') {
    Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', { configurable: true, value: () => undefined });
  }
});

afterEach(() => {
  cleanup();
  currentSearchParams = new URLSearchParams();
  channelData = undefined;
});

describe('packed Fairtrade context navigation mounted through Peasant', () => {
  const fixtureSet = loadCases();

  it('rejects every strict loader mutation from the fixture manifest', () => {
    for (const mutation of fixtureSet.loaderMutations) {
      const mutated = replaceExactlyOnce(
        mutation.target === 'manifest' ? manifestSource : casesSource,
        mutation.find,
        mutation.replace,
        mutation.name,
      );
      expect(
        () => loadCases(mutation.target === 'manifest' ? mutated : manifestSource, mutation.target === 'cases' ? mutated : casesSource),
        mutation.name,
      ).toThrow(new RegExp(mutation.expectedError));
    }
  });

  for (const fixture of fixtureSet.cases) {
    it(fixture.name, async () => {
      currentSearchParams = new URLSearchParams(fixture.search);
      channelData = channelPayloadFor(fixture);

      const view = render(<TestDetail />);
      await waitFor(() => invariant(view.container.querySelector('.txn-app') !== null, 'the transcript composite never mounted'));

      const rows = assertHeader(view.container, fixture.expectedHeader);

      if (fixture.expectedPushLocalId) {
        const link = rows[fixture.expectedPushRowIndex].querySelector<HTMLButtonElement>('.txn-context-link');
        act(() => {
          if (link) fireEvent.click(link);
        });
        invariant(routerPush.mock.calls.length === 1, `expected one navigation for the authorized target, recorded ${routerPush.mock.calls.length}`);
        invariant(
          routerPush.mock.calls[0][0] === transcriptHref(PROJECT_HASH, fixture.expectedPushLocalId),
          `navigation target ${JSON.stringify(routerPush.mock.calls[0][0])} is not the authorized target`,
        );
      } else {
        invariant(routerPush.mock.calls.length === 0, 'an unauthorized target navigated');
      }

      if (fixture.expectedEarlierCount !== undefined) {
        const content = fixture.earlierSections?.[0]?.turns[0]?.content ?? '';
        const toggles = assertEarlierCount(view.container, fixture.expectedEarlierCount);

        if (fixture.search.includes('earlier=')) {
          // A route that already names the section restores it, so the child
          // reads exactly the state it was left in.
          invariant(toggles[0].getAttribute('aria-expanded') === 'true', 'a disclosed section did not restore from the route');
          invariant(view.container.textContent?.includes(content) === true, 'restored retained history did not render its captured blocks');

          // Back to the child without the disclosure collapses it again, which
          // is what browser Back to a pre-disclosure child URL must show.
          currentSearchParams = new URLSearchParams('');
          view.rerender(<TestDetail />);
          await waitFor(() => invariant(view.container.querySelector('.txn-earlier-toggle')?.getAttribute('aria-expanded') === 'false', 'leaving the disclosure route did not collapse the section'));
          invariant(view.container.textContent?.includes(content) === false, 'collapsed retained history still rendered its blocks');
        } else {
          invariant(toggles[0].getAttribute('aria-expanded') === 'false', 'retained history was disclosed without a reader action');
          invariant(view.container.textContent?.includes(content) === false, 'collapsed retained history rendered its blocks');

          act(() => {
            fireEvent.click(toggles[0]);
          });
          await waitFor(() => invariant(routerReplace.mock.calls.length > 0, 'the retained-history disclosure was not persisted in the route'));
          const disclosed = routerReplace.mock.calls.at(-1)?.[0] as string;
          invariant(disclosed === `${PATHNAME}?earlier=earlier-0`, `persisted disclosure route ${JSON.stringify(disclosed)} is not the section that was opened`);

          // The disclosure lives in the route, so returning to that URL (Back
          // from the parent, a reload, or a copied link) reopens the section.
          currentSearchParams = new URL(disclosed, 'https://peasant.invalid').searchParams;
          view.rerender(<TestDetail />);
          await waitFor(() => invariant(view.container.querySelector('.txn-earlier-toggle')?.getAttribute('aria-expanded') === 'true', 'returning to the disclosed route did not reopen the section'));
          invariant(view.container.textContent?.includes(content) === true, 'reopened retained history did not render its captured blocks');
        }
      }
    });
  }
});
