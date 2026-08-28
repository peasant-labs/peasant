import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { SessionDetailV2 } from './SessionDetailV2';
import { parseTranscriptRouteQuery, type ProjectHash } from '@/lib/navigation/projectRoutes';
import { parseStrictYAML, requireExactRequiredFields, requireRecord, requireUniqueNames } from '@/test/strictYaml';

const PROJECT_HASH = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' as ProjectHash;
const PATHNAME = '/projects/alpha-project/sess-12345678';
const activeTurnCallbacks = vi.hoisted(() => [] as string[]);
const fields = ['name', 'search', 'turns', 'targetRole', 'targetDepth', 'action', 'applyRouterResult', 'expectedScrolls', 'expectedRouter', 'expectedCallbacks', 'expectedHistory'] as const;
const transitionFields = ['name', 'initialSearch', 'rerenderSearch', 'turns', 'expectedScrolls', 'expectedRouter', 'expectedCallbacks', 'expectedHistory'] as const;
const mutationFields = ['name', 'find', 'replace', 'expectedTestFile', 'expectedFailedTestNames', 'expectedFailurePattern'] as const;
const loaderMutationFields = ['name', 'target', 'find', 'replace', 'expectedError'] as const;
type Fixture = {
  name: string; search: string; turns: number[]; targetRole: 'user' | 'assistant';
  targetDepth: number; action: string; applyRouterResult: boolean; expectedScrolls: string[];
  expectedRouter: string[]; expectedCallbacks: string[]; expectedHistory: string[];
};
type TransitionFixture = {
  name: string; initialSearch: string; rerenderSearch: string; turns: number[]; expectedScrolls: string[];
  expectedRouter: string[]; expectedCallbacks: string[]; expectedHistory: string[];
};
type LoaderMutation = { name: string; target: 'manifest' | 'cases'; find: string; replace: string; expectedError: string };

const manifestSource = readFileSync(resolve(process.cwd(), 'src/components/session-detail/v2/testdata/transcript_mounted_position.manifest.yaml'), 'utf8');
const casesSource = readFileSync(resolve(process.cwd(), 'src/components/session-detail/v2/testdata/transcript_mounted_position.yaml'), 'utf8');

function strictStringArray(value: unknown, label: string, grammar: RegExp): string[] {
  if (!Array.isArray(value) || value.some((item) => typeof item !== 'string' || !grammar.test(item))) throw new Error(`${label} has invalid string grammar`);
  return value as string[];
}

function strictTurns(value: unknown, label: string): number[] {
  if (!Array.isArray(value) || value.some((turn) => !Number.isSafeInteger(turn) || (turn as number) < 0) || new Set(value).size !== value.length) throw new Error(`${label} must contain unique safe nonnegative integers`);
  return value as number[];
}

function assertExactMatrix(actual: unknown[], expected: unknown[], invariant: string): void {
  if (JSON.stringify(actual) !== JSON.stringify(expected)) throw new Error(`${invariant} invariant failed: expected ${JSON.stringify(expected)}, received ${JSON.stringify(actual)}`);
}

function replaceExactlyOnce(source: string, find: string, replacement: string, label: string): string {
  const occurrences = source.split(find).length - 1;
  if (occurrences !== 1) throw new Error(`${label} mutation anchor must occur exactly once, found ${occurrences}`);
  return source.replace(find, replacement);
}

function loadFixtures(manifestText = manifestSource, casesText = casesSource): { cases: Fixture[]; transitions: TransitionFixture[]; loaderMutations: LoaderMutation[] } {
  const manifest = requireRecord(parseStrictYAML(manifestText, 'mounted transcript manifest'), 'mounted transcript manifest');
  requireExactRequiredFields(manifest, ['expectedCaseCount', 'requiredNames', 'expectedTransitionCount', 'requiredTransitionNames', 'expectedLoaderMutationCount', 'loaderMutations', 'expectedMutationCount', 'mutations'], 'mounted transcript manifest');
  if (!Number.isSafeInteger(manifest.expectedCaseCount) || !Array.isArray(manifest.requiredNames) || !Number.isSafeInteger(manifest.expectedTransitionCount) || !Array.isArray(manifest.requiredTransitionNames) || !Number.isSafeInteger(manifest.expectedLoaderMutationCount) || !Array.isArray(manifest.loaderMutations) || !Number.isSafeInteger(manifest.expectedMutationCount) || !Array.isArray(manifest.mutations)) throw new Error('mounted transcript manifest requires case, transition, loader-mutation, and production-mutation inventories');
  const requiredNames = manifest.requiredNames as unknown[];
  const requiredTransitionNames = manifest.requiredTransitionNames as unknown[];
  if ([...requiredNames, ...requiredTransitionNames].some((name) => typeof name !== 'string' || name.length === 0) || new Set(requiredNames).size !== requiredNames.length || new Set(requiredTransitionNames).size !== requiredTransitionNames.length) throw new Error('mounted transcript manifest requires unique nonempty names');
  const loaderMutations = manifest.loaderMutations.map((row, index) => {
    const mutation = requireRecord(row, `mounted transcript manifest.loaderMutations[${index}]`);
    requireExactRequiredFields(mutation, loaderMutationFields, `mounted transcript manifest.loaderMutations[${index}]`);
    if (!['manifest', 'cases'].includes(mutation.target as string) || ['name', 'find', 'replace', 'expectedError'].some((field) => typeof mutation[field] !== 'string' || mutation[field].length === 0)) throw new Error(`mounted transcript loader mutation ${index} has invalid values`);
    return mutation as LoaderMutation;
  });
  if (loaderMutations.length !== manifest.expectedLoaderMutationCount || new Set(loaderMutations.map((row) => row.name)).size !== loaderMutations.length) throw new Error('mounted transcript loader mutation inventory count or names are invalid');
  const mutations = manifest.mutations.map((row, index) => {
    const mutation = requireRecord(row, `mounted transcript manifest.mutations[${index}]`);
    requireExactRequiredFields(mutation, mutationFields, `mounted transcript manifest.mutations[${index}]`);
    for (const field of ['name', 'find', 'replace', 'expectedTestFile', 'expectedFailurePattern']) {
      if (typeof mutation[field] !== 'string' || (mutation[field] as string).length === 0) throw new Error(`mounted transcript mutation ${index} requires nonempty string fields`);
    }
    const failedTestNames = mutation.expectedFailedTestNames;
    if (!Array.isArray(failedTestNames) || failedTestNames.length === 0 || failedTestNames.some((name) => typeof name !== 'string' || name.length === 0) || new Set(failedTestNames).size !== failedTestNames.length) {
      throw new Error(`mounted transcript mutation ${index} requires a non-empty array of unique nonempty expectedFailedTestNames`);
    }
    return mutation;
  });
  if (mutations.length !== manifest.expectedMutationCount || new Set(mutations.map((row) => row.name)).size !== mutations.length) throw new Error('mounted transcript mutation inventory count or names are invalid');
  const mutationNames = [...loaderMutations.map((row) => row.name), ...mutations.map((row) => row.name as string)];
  if (new Set(mutationNames).size !== mutationNames.length) throw new Error('mounted transcript mutation names must be globally unique');
  const root = requireRecord(parseStrictYAML(casesText, 'mounted transcript cases'), 'mounted transcript cases');
  requireExactRequiredFields(root, ['cases', 'transitions'], 'mounted transcript cases');
  if (!Array.isArray(root.cases) || !Array.isArray(root.transitions)) throw new Error('mounted transcript cases and transitions must be arrays');
  const rows = root.cases.map((row, index) => requireRecord(row, `mounted transcript cases[${index}]`));
  requireUniqueNames(rows, 'mounted transcript cases');
  rows.forEach((row, index) => {
    requireExactRequiredFields(row, fields, `mounted transcript cases[${index}]`);
    if (typeof row.name !== 'string' || row.name.length === 0 || typeof row.search !== 'string' || !/^turn=\d+(?:&(?:scope|scopeVal|origin)=[^&]+)*$/.test(row.search) || !['user', 'assistant'].includes(row.targetRole as string) || !Number.isSafeInteger(row.targetDepth) || (row.targetDepth as number) < 0 || !['none', 'reveal linked turn'].includes(row.action as string) || typeof row.applyRouterResult !== 'boolean') throw new Error(`mounted transcript cases[${index}] has invalid scalar fields`);
    strictTurns(row.turns, `mounted transcript cases[${index}].turns`);
    strictStringArray(row.expectedScrolls, `mounted transcript cases[${index}].expectedScrolls`, /^(?:top|turn:\d+)$/);
    strictStringArray(row.expectedRouter, `mounted transcript cases[${index}].expectedRouter`, /^\/projects\/alpha-project\/sess-12345678(?:\?.+)?$/);
    strictStringArray(row.expectedCallbacks, `mounted transcript cases[${index}].expectedCallbacks`, /^active:\d+$/);
    strictStringArray(row.expectedHistory, `mounted transcript cases[${index}].expectedHistory`, /^(?:push|replace)$/);
    if ((row.applyRouterResult as boolean) !== ((row.expectedRouter as unknown[]).length === 1) || (row.action === 'none' && row.applyRouterResult)) throw new Error(`mounted transcript cases[${index}] has inconsistent action and router expectations`);
  });
  const names = rows.map((row) => row.name);
  if (rows.length !== manifest.expectedCaseCount || requiredNames.length !== manifest.expectedCaseCount || requiredNames.some((name) => !names.includes(name as string)) || names.some((name) => !requiredNames.includes(name))) throw new Error('mounted transcript cases do not match their independent manifest');
  const transitions = root.transitions.map((row, index) => requireRecord(row, `mounted transcript transitions[${index}]`));
  requireUniqueNames(transitions, 'mounted transcript transitions');
  transitions.forEach((row, index) => {
    requireExactRequiredFields(row, transitionFields, `mounted transcript transitions[${index}]`);
    if (typeof row.name !== 'string' || row.name.length === 0 || typeof row.initialSearch !== 'string' || !/^turn=\d+$/.test(row.initialSearch) || typeof row.rerenderSearch !== 'string' || !/^turn=\d+$/.test(row.rerenderSearch) || row.initialSearch === row.rerenderSearch) throw new Error(`mounted transcript transitions[${index}] has invalid target sequence`);
    strictTurns(row.turns, `mounted transcript transitions[${index}].turns`);
    strictStringArray(row.expectedScrolls, `mounted transcript transitions[${index}].expectedScrolls`, /^(?:top|turn:\d+)$/);
    strictStringArray(row.expectedRouter, `mounted transcript transitions[${index}].expectedRouter`, /^\/projects\/alpha-project\/sess-12345678(?:\?.+)?$/);
    strictStringArray(row.expectedCallbacks, `mounted transcript transitions[${index}].expectedCallbacks`, /^active:\d+$/);
    strictStringArray(row.expectedHistory, `mounted transcript transitions[${index}].expectedHistory`, /^(?:push|replace)$/);
  });
  const transitionNames = transitions.map((row) => row.name);
  if (transitions.length !== manifest.expectedTransitionCount || requiredTransitionNames.length !== manifest.expectedTransitionCount || requiredTransitionNames.some((name) => !transitionNames.includes(name as string)) || transitionNames.some((name) => !requiredTransitionNames.includes(name))) throw new Error('mounted transcript transitions do not match their independent manifest');
  if (new Set([...names, ...transitionNames, ...mutationNames]).size !== names.length + transitionNames.length + mutationNames.length) throw new Error('mounted transcript case and mutation names must be globally unique');
  return { cases: rows as Fixture[], transitions: transitions as TransitionFixture[], loaderMutations };
}

const routerReplace = vi.hoisted(() => vi.fn());
let currentSearchParams = new URLSearchParams();
let channelData: unknown;
vi.mock('next/navigation', () => ({
  useSearchParams: () => currentSearchParams,
  usePathname: () => PATHNAME,
  useRouter: () => ({ replace: routerReplace }),
}));
vi.mock('@/contexts/WebSocketContext', () => ({ useChannel: () => ({ data: channelData, connected: true, error: null }) }));
vi.mock('./lib/useEntryLabels', () => ({ useEntryLabels: () => ({ entryTypes: [], labelsByEntry: new Map(), addLabel: vi.fn() }) }));
vi.mock('@/hooks/useTheme', () => ({ useTheme: () => ({ theme: 'dark', setTheme: vi.fn(), toggle: vi.fn() }) }));
vi.mock('@peasant-labs/fairtrade/ui', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@peasant-labs/fairtrade/ui')>();
  const ReactModule = await import('react');
  return {
    ...actual,
    TranscriptViewer: (props: React.ComponentProps<typeof actual.TranscriptViewer>) => ReactModule.createElement(actual.TranscriptViewer, {
      ...props,
      onActiveTurnChange: (turn: number) => activeTurnCallbacks.push(`active:${turn}`),
    }),
    annotateTranscript: () => [],
    computePersonalMedians: () => undefined,
  };
});
vi.mock('@peasant-labs/fairtrade/graph', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@peasant-labs/fairtrade/graph')>();
  return { ...actual, TrajectoryGraph: () => null };
});

function TestDetail() {
  const routeQuery = parseTranscriptRouteQuery(currentSearchParams);
  if (!routeQuery) throw new Error('fixture route query must be valid');
  return <SessionDetailV2 sessionId="sess-12345678" projectHash={PROJECT_HASH} projectName="alpha-project" routeQuery={routeQuery} />;
}

beforeEach(() => {
  routerReplace.mockClear();
  activeTurnCallbacks.length = 0;
  window.localStorage.clear();
  if (typeof HTMLElement.prototype.scrollIntoView !== 'function') {
    Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', { configurable: true, value: () => undefined });
  }
});
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe('packed Fairtrade viewer mounted through Peasant', () => {
  const fixtureSet = loadFixtures();
  it('rejects every strict loader mutation from YAML', () => {
    for (const mutation of fixtureSet.loaderMutations) {
      const mutated = replaceExactlyOnce(mutation.target === 'manifest' ? manifestSource : casesSource, mutation.find, mutation.replace, mutation.name);
      expect(() => loadFixtures(mutation.target === 'manifest' ? mutated : manifestSource, mutation.target === 'cases' ? mutated : casesSource), mutation.name).toThrow(new RegExp(mutation.expectedError));
    }
  });

  for (const fixture of fixtureSet.cases) {
    it(fixture.name, async () => {
      currentSearchParams = new URLSearchParams(fixture.search);
      channelData = {
        id: 'sess-12345678', project: 'alpha-project', harness: 'codex',
        startTime: '2026-07-15T08:00:00Z', endTime: '2026-07-15T08:01:00Z', durationMins: 1,
        totalTokens: 12, tokensIn: 5, tokensOut: 7, turnCount: fixture.turns.length, toolCallCount: 0,
        turns: fixture.turns.map((index) => ({
          index, role: index === 42 ? fixture.targetRole : 'user', depth: index === 42 ? fixture.targetDepth : 0,
          content: `turn ${index}`, timestamp: '2026-07-15T08:00:00Z', toolCalls: [],
        })),
      };
      const scrolls: string[] = [];
      const history: string[] = [];
      vi.spyOn(HTMLElement.prototype, 'scrollTo').mockImplementation(function (this: HTMLElement, optionsOrX: ScrollToOptions | number) {
        if (this.classList.contains('txn-stream') && typeof optionsOrX === 'object' && optionsOrX !== null && optionsOrX.top === 0) scrolls.push('top');
      } as typeof HTMLElement.prototype.scrollTo);
      vi.spyOn(HTMLElement.prototype, 'scrollIntoView').mockImplementation(function (this: HTMLElement) { scrolls.push(`turn:${this.dataset.turn}`); });
      vi.spyOn(window.history, 'pushState').mockImplementation(() => { history.push('push'); });
      vi.spyOn(window.history, 'replaceState').mockImplementation(() => { history.push('replace'); });

      const view = render(<TestDetail />);
      await waitFor(() => expect(view.container.querySelector('.txn-app')).toBeInTheDocument());
      const viewer = view.container.querySelector('.txn-app');
      if (fixture.action !== 'none') fireEvent.click(await screen.findByRole('button', { name: fixture.action }));
      if (fixture.applyRouterResult) {
        await waitFor(() => expect(routerReplace).toHaveBeenCalledTimes(1));
        currentSearchParams = new URL(routerReplace.mock.calls[0][0] as string, 'https://peasant.invalid').searchParams;
        view.rerender(<TestDetail />);
      }
      await waitFor(() => assertExactMatrix(scrolls, fixture.expectedScrolls, 'mounted positioning scroll'));
      assertExactMatrix(routerReplace.mock.calls.map((call) => call[0]), fixture.expectedRouter, 'mounted router call token');
      assertExactMatrix(activeTurnCallbacks, fixture.expectedCallbacks, 'mounted positioning callback');
      assertExactMatrix(history, fixture.expectedHistory, 'mounted positioning history');
      expect(view.container.querySelector('.txn-app')).toBe(viewer);
    });
  }

  for (const fixture of fixtureSet.transitions) {
    it(fixture.name, async () => {
      currentSearchParams = new URLSearchParams(fixture.initialSearch);
      channelData = {
        id: 'sess-12345678', project: 'alpha-project', harness: 'codex', startTime: '2026-07-15T08:00:00Z',
        endTime: '2026-07-15T08:01:00Z', durationMins: 1, totalTokens: 12, tokensIn: 5, tokensOut: 7,
        turnCount: fixture.turns.length, toolCallCount: 0,
        turns: fixture.turns.map((index) => ({ index, role: 'user', depth: 0, content: `turn ${index}`, timestamp: '2026-07-15T08:00:00Z', toolCalls: [] })),
      };
      const scrolls: string[] = [];
      const history: string[] = [];
      vi.spyOn(HTMLElement.prototype, 'scrollTo').mockImplementation(function (this: HTMLElement, optionsOrX: ScrollToOptions | number) {
        if (this.classList.contains('txn-stream') && typeof optionsOrX === 'object' && optionsOrX !== null && optionsOrX.top === 0) scrolls.push('top');
      } as typeof HTMLElement.prototype.scrollTo);
      vi.spyOn(HTMLElement.prototype, 'scrollIntoView').mockImplementation(function (this: HTMLElement) { scrolls.push(`turn:${this.dataset.turn}`); });
      vi.spyOn(window.history, 'pushState').mockImplementation(() => { history.push('push'); });
      vi.spyOn(window.history, 'replaceState').mockImplementation(() => { history.push('replace'); });
      const view = render(<TestDetail />);
      await waitFor(() => assertExactMatrix(scrolls, ['top'], 'mounted transition initial scroll'));
      const viewer = view.container.querySelector('.txn-app');
      currentSearchParams = new URLSearchParams(fixture.rerenderSearch);
      view.rerender(<TestDetail />);
      await waitFor(() => assertExactMatrix(scrolls, fixture.expectedScrolls, 'mounted transition scroll'));
      assertExactMatrix(routerReplace.mock.calls.map((call) => call[0]), fixture.expectedRouter, 'mounted transition router call token');
      assertExactMatrix(activeTurnCallbacks, fixture.expectedCallbacks, 'mounted transition callback');
      assertExactMatrix(history, fixture.expectedHistory, 'mounted transition history');
      expect(view.container.querySelector('.txn-app')).toBe(viewer);
    });
  }
});
