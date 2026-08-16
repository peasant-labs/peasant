import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, cleanup, fireEvent, within } from '@testing-library/react';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { SessionDetailV2 } from './SessionDetailV2';
import { parseTranscriptRouteQuery, type ProjectHash } from '@/lib/navigation/projectRoutes';
import { parseStrictYAML, requireExactRequiredFields, requireRecord, requireUniqueNames } from '@/test/strictYaml';

const PROJECT_HASH = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' as ProjectHash;

// -- Mocks --------------------------------------------------------------------

// Mockable next/navigation: tests set the search params before rendering.
const routerReplace = vi.hoisted(() => vi.fn());
let currentSearchParams = new URLSearchParams();
const PATHNAME = '/projects/alpha-project/sess-12345678';

vi.mock('next/navigation', () => ({
  useSearchParams: () => currentSearchParams,
  usePathname: () => PATHNAME,
  useRouter: () => ({ replace: routerReplace }),
}));

// Channel data is mutable so a test can supply a minimal session_detail payload.
let channelData: unknown = undefined;
const viewerLifecycle = vi.hoisted(() => ({ mounts: 0, unmounts: 0 }));
vi.mock('@/contexts/WebSocketContext', () => ({
  useChannel: () => ({ data: channelData, connected: true, error: null }),
}));

// REST-backed label loading is out of scope here.
vi.mock('./lib/useEntryLabels', () => ({
  useEntryLabels: () => ({ entryTypes: [], labelsByEntry: new Map(), addLabel: vi.fn() }),
}));

// Theme persistence is unrelated to this suite.
vi.mock('@/hooks/useTheme', () => ({
  useTheme: () => ({ theme: 'dark', setTheme: vi.fn(), toggle: vi.fn() }),
}));

// The shared package is the heavy leaf (composer, graph canvas); the
// change-scope empty state is adapter-owned and never reaches it.
vi.mock('@peasant-labs/transcript-browser', () => ({
  TrajectoryGraph: () => null,
  annotateTranscript: () => [],
  computePersonalMedians: () => undefined,
  // Reached via scopeTurns when a scope is active; identity is fine for tests.
  prefilterTurns: (turns: unknown) => turns,
}));

// The viewer now mounts fairtrade's composite; the adapter must return a
// turns-bearing VM because the host derives toolVMsByTurn from it. Partial
// mock: other components in the tree import real primitives (Button etc.).
// The mock renders both host extension slots used by this integration:
// `streamPrelude` (in the transcript stream) and `headerActions` (in the
// hero action row) — the restored "copy as markdown" home.
vi.mock(import('@peasant-labs/fairtrade/ui'), async (importOriginal) => {
  const actual = await importOriginal();
  const ReactModule = await import('react');
  const TranscriptViewerMock = (props: React.ComponentProps<typeof actual.TranscriptViewer>) => {
    ReactModule.useEffect(() => {
      viewerLifecycle.mounts += 1;
      return () => { viewerLifecycle.unmounts += 1; };
    }, []);
    return (
      <div
        data-testid="package-session-detail"
        data-initial-kind={props.initialPosition?.kind ?? 'none'}
        data-initial-turn={props.initialPosition?.kind === 'turn' ? props.initialPosition.turnIndex : -1}
        data-initial-request-key={props.initialPosition?.requestKey ?? 'none'}
      >
        {/* The trail is host-owned policy (root → project → session), so expose
            it for assertion; the package would render it in its hero. */}
        <div
          data-testid="package-breadcrumb"
          data-crumbs={JSON.stringify(
            (props.breadcrumb ?? []).map((c) => [c.label, c.href ?? null]),
          )}
        />
        <div data-testid="package-header-actions">
          {props.headerActions as React.ReactNode}
        </div>
        <div data-testid="package-transcript-stream">
          {props.streamPrelude as React.ReactNode}
        </div>
      </div>
    );
  };
  return {
    ...actual,
    TranscriptViewer: TranscriptViewerMock,
    adaptTranscript: (() => ({ turns: [] })) as unknown as typeof actual.adaptTranscript,
    computeAnalytics: (() => ({})) as unknown as typeof actual.computeAnalytics,
  };
});

// A minimal session_detail payload — the package render is mocked, so only the
// adapter's use of turns/project/id matters.
const DETAIL = {
  id: 'sess-12345678',
  project: 'alpha-project',
  turns: [
    { index: 0, role: 'user', depth: 0, content: 'q0', toolCalls: [] },
    { index: 1, role: 'assistant', depth: 0, content: 'a0', toolCalls: [] },
  ],
};

const recoveryManifestSource = readFileSync(resolve(process.cwd(), 'src/components/session-detail/v2/testdata/transcript_target_recovery.manifest.yaml'), 'utf8');
const recoveryCasesSource = readFileSync(resolve(process.cwd(), 'src/components/session-detail/v2/testdata/transcript_target_recovery.yaml'), 'utf8');
const recoveryFields = ['name', 'family', 'search', 'turns', 'targetRole', 'targetDepth', 'expectedInitialKind', 'expectedInitialTurn', 'expectedInitialRequestKey', 'expectedFeedback', 'action', 'expectedReplace', 'expectedAfterActionKind', 'expectedAfterActionTurn', 'expectedAfterActionRequestKey'] as const;
type RecoveryCase = {
  name: string; family: string; search: string; turns: number[];
  targetRole: 'user' | 'assistant'; targetDepth: number; expectedInitialKind: string;
  expectedInitialTurn: number; expectedInitialRequestKey: string; expectedFeedback: string; action: string;
  expectedReplace: string; expectedAfterActionKind: string; expectedAfterActionTurn: number;
  expectedAfterActionRequestKey: string;
};
type ProductionMutation = { name: string; find: string; replace: string; expectedTestFile: string; expectedFailedTestNames: string[]; expectedFailurePattern: string };
type LoaderMutation = { name: string; target: 'manifest' | 'cases'; find: string; replace: string; expectedError: string };
type RecoveryFixture = { cases: RecoveryCase[]; mutations: ProductionMutation[]; loaderMutations: LoaderMutation[] };

function replaceExactlyOnce(source: string, find: string, replacement: string, label: string): string {
  const occurrences = source.split(find).length - 1;
  if (occurrences !== 1) throw new Error(`${label} mutation anchor must occur exactly once, found ${occurrences}`);
  return source.replace(find, replacement);
}

function loadRecoveryCases(manifestText = recoveryManifestSource, casesText = recoveryCasesSource): RecoveryFixture {
  const manifest = requireRecord(parseStrictYAML(manifestText, 'transcript target recovery manifest'), 'transcript target recovery manifest');
  requireExactRequiredFields(manifest, ['expectedCaseCount', 'requiredFamilies', 'requiredNames', 'expectedLoaderMutationCount', 'loaderMutations', 'expectedMutationCount', 'mutations'], 'transcript target recovery manifest');
  if (!Number.isInteger(manifest.expectedCaseCount) || !Array.isArray(manifest.requiredFamilies) || !Array.isArray(manifest.requiredNames) || !Number.isInteger(manifest.expectedLoaderMutationCount) || !Array.isArray(manifest.loaderMutations) || !Number.isInteger(manifest.expectedMutationCount) || !Array.isArray(manifest.mutations)) throw new Error('transcript target recovery manifest requires case, loader-mutation, and production-mutation inventories');
  const requiredFamilies = manifest.requiredFamilies as unknown[];
  const requiredNames = manifest.requiredNames as unknown[];
  if ([...requiredFamilies, ...requiredNames].some((value) => typeof value !== 'string' || value.length === 0)) throw new Error('transcript target recovery manifest values must be nonempty strings');
  if (new Set(requiredFamilies).size !== requiredFamilies.length || new Set(requiredNames).size !== requiredNames.length) throw new Error('transcript target recovery manifest values must be unique');
  if (requiredFamilies.length !== manifest.expectedCaseCount || requiredNames.length !== manifest.expectedCaseCount) throw new Error('transcript target recovery manifest count must cover every family and name');
  const loaderMutations = manifest.loaderMutations.map((row, index) => {
    const mutation = requireRecord(row, `transcript target recovery manifest.loaderMutations[${index}]`);
    requireExactRequiredFields(mutation, ['name', 'target', 'find', 'replace', 'expectedError'], `transcript target recovery manifest.loaderMutations[${index}]`);
    if (!['manifest', 'cases'].includes(mutation.target as string) || ['name', 'find', 'replace', 'expectedError'].some((field) => typeof mutation[field] !== 'string' || mutation[field].length === 0)) throw new Error(`transcript target recovery loader mutation ${index} has invalid values`);
    return mutation as LoaderMutation;
  });
  if (loaderMutations.length !== manifest.expectedLoaderMutationCount || new Set(loaderMutations.map((row) => row.name)).size !== loaderMutations.length) throw new Error('transcript target recovery loader mutation inventory count or names are invalid');
  const mutations = manifest.mutations.map((row, index) => {
    const mutation = requireRecord(row, `transcript target recovery manifest.mutations[${index}]`);
    const fields = ['name', 'find', 'replace', 'expectedTestFile', 'expectedFailedTestNames', 'expectedFailurePattern'];
    requireExactRequiredFields(mutation, fields, `transcript target recovery manifest.mutations[${index}]`);
    for (const field of ['name', 'find', 'replace', 'expectedTestFile', 'expectedFailurePattern']) {
      if (typeof mutation[field] !== 'string' || (mutation[field] as string).length === 0) throw new Error(`transcript target recovery manifest mutation ${index} requires nonempty string fields`);
    }
    const failedTestNames = mutation.expectedFailedTestNames;
    if (!Array.isArray(failedTestNames) || failedTestNames.length === 0 || failedTestNames.some((name) => typeof name !== 'string' || name.length === 0) || new Set(failedTestNames).size !== failedTestNames.length) {
      throw new Error(`transcript target recovery manifest mutation ${index} requires a non-empty array of unique nonempty expectedFailedTestNames`);
    }
    return mutation as unknown as ProductionMutation;
  });
  if (mutations.length !== manifest.expectedMutationCount || new Set(mutations.map((row) => row.name)).size !== mutations.length) throw new Error('transcript target recovery mutation inventory count or names are invalid');
  const mutationNames = [...loaderMutations.map((row) => row.name), ...mutations.map((row) => row.name)];
  if (new Set(mutationNames).size !== mutationNames.length) throw new Error('transcript target recovery mutation names must be globally unique');
  const root = requireRecord(parseStrictYAML(casesText, 'transcript target recovery cases'), 'transcript target recovery cases');
  requireExactRequiredFields(root, ['cases'], 'transcript target recovery cases');
  if (!Array.isArray(root.cases)) throw new Error('transcript target recovery cases must be an array');
  const cases = root.cases.map((row, index) => requireRecord(row, `transcript target recovery cases[${index}]`));
  requireUniqueNames(cases, 'transcript target recovery cases');
  cases.forEach((row, index) => requireExactRequiredFields(row, recoveryFields, `transcript target recovery cases[${index}]`));
  cases.forEach((row, index) => {
    for (const field of ['name', 'family', 'search', 'expectedInitialKind', 'expectedInitialRequestKey', 'expectedFeedback', 'action', 'expectedReplace', 'expectedAfterActionKind', 'expectedAfterActionRequestKey']) {
      if (typeof row[field] !== 'string' || row[field].length === 0) throw new Error(`transcript target recovery cases[${index}].${field} must be a nonempty string`);
    }
    if (!Array.isArray(row.turns) || row.turns.some((turn) => !Number.isSafeInteger(turn) || (turn as number) < 0) || new Set(row.turns).size !== row.turns.length) throw new Error(`transcript target recovery cases[${index}] has invalid turn values`);
    if (!['user', 'assistant'].includes(row.targetRole as string) || !Number.isSafeInteger(row.targetDepth) || (row.targetDepth as number) < 0 || !Number.isSafeInteger(row.expectedInitialTurn) || !Number.isSafeInteger(row.expectedAfterActionTurn)) throw new Error(`transcript target recovery cases[${index}] has invalid role, depth, or index values`);
    if (!['none', 'top', 'turn'].includes(row.expectedInitialKind as string) || !['none', 'top', 'turn'].includes(row.expectedAfterActionKind as string)) throw new Error(`transcript target recovery cases[${index}] has invalid position kind`);
    if (!['none', 'reveal linked turn', 'remove stale turn target'].includes(row.action as string) || (row.expectedReplace !== 'none' && !/^\/projects\/alpha-project\/sess-12345678(?:\?.+)?$/.test(row.expectedReplace as string))) throw new Error(`transcript target recovery cases[${index}] has invalid action or router result`);
    if (!/^turn=\d+(?:&scope=task&scopeVal=\d+&origin=Map)?$/.test(row.search as string)) throw new Error(`transcript target recovery cases[${index}] search is outside the allowed route-query domain`);
    const requestedTurn = Number(new URLSearchParams(row.search as string).get('turn'));
    const requestKeys = [row.expectedInitialRequestKey, row.expectedAfterActionRequestKey] as string[];
    if (requestKeys.some((key) => key !== 'none' && !/^missing-turn:\d+$/.test(key))) throw new Error(`transcript target recovery cases[${index}] requestKey is outside the allowed domain`);
    if (![row.expectedFeedback].every((value) => ['none', 'linked turn is no longer available', 'linked turn is hidden by the current view'].includes(value as string))) throw new Error(`transcript target recovery cases[${index}] feedback is outside the allowed domain`);
    if (!['none', '/projects/alpha-project/sess-12345678', `/projects/alpha-project/sess-12345678?turn=${requestedTurn}&origin=Map`].includes(row.expectedReplace as string)) throw new Error(`transcript target recovery cases[${index}] call token is outside the allowed domain`);
    for (const [kindField, turnField] of [['expectedInitialKind', 'expectedInitialTurn'], ['expectedAfterActionKind', 'expectedAfterActionTurn']] as const) {
      if ((row[kindField] === 'turn') !== ((row[turnField] as number) >= 0)) throw new Error(`transcript target recovery cases[${index}] has inconsistent position kind and turn sentinel`);
    }
    if (row.action === 'remove stale turn target' && row.expectedReplace === 'none') throw new Error(`transcript target recovery cases[${index}] stale-target removal must replace the route`);
    const targetExists = (row.turns as number[]).includes(requestedTurn);
    const targetVisible = targetExists && row.expectedInitialKind === 'turn';
    if (!targetExists) {
      if (row.expectedInitialKind !== 'top' || row.expectedInitialRequestKey !== `missing-turn:${requestedTurn}` || row.expectedFeedback !== 'linked turn is no longer available' || row.action !== 'remove stale turn target') throw new Error(`transcript target recovery cases[${index}] violates the absent-target cross-relations`);
    } else if (!targetVisible) {
      if (row.expectedInitialKind !== 'none' || row.expectedInitialRequestKey !== 'none' || row.expectedFeedback !== 'linked turn is hidden by the current view' || row.action !== 'reveal linked turn') throw new Error(`transcript target recovery cases[${index}] violates the hidden-target cross-relations`);
    } else if (row.expectedInitialTurn !== requestedTurn || row.expectedInitialRequestKey !== 'none' || row.expectedFeedback !== 'none' || row.action !== 'none') throw new Error(`transcript target recovery cases[${index}] violates the visible-target cross-relations`);
    const expectedAfterKind = row.action === 'reveal linked turn' ? 'turn' : row.action === 'remove stale turn target' ? 'top' : row.expectedInitialKind;
    const expectedAfterTurn = expectedAfterKind === 'turn' ? requestedTurn : -1;
    if (row.expectedAfterActionKind !== expectedAfterKind || row.expectedAfterActionTurn !== expectedAfterTurn || row.expectedAfterActionRequestKey !== 'none') throw new Error(`transcript target recovery cases[${index}] violates the post-action cross-relations`);
  });
  const names = cases.map((row) => row.name);
  const families = cases.map((row) => row.family);
  if (cases.length !== manifest.expectedCaseCount || requiredNames.some((name) => !names.includes(name)) || names.some((name) => !requiredNames.includes(name))) throw new Error('transcript target recovery names do not match their independent manifest');
  if (requiredFamilies.some((family) => !families.includes(family))) throw new Error('transcript target recovery cases are missing a required family');
  if (new Set([...names, ...mutationNames]).size !== names.length + mutationNames.length) throw new Error('transcript target recovery case and mutation names must be globally unique');
  return { cases: cases as RecoveryCase[], mutations, loaderMutations };
}

const recoveryFixture = loadRecoveryCases();

beforeEach(() => {
  // Change scope now filters to the named task slices (entry indices), so the
  // scopeVal is numeric — supplied by the Review surface, not a branch name.
  currentSearchParams = new URLSearchParams({
    scope: 'change',
    scopeVal: '0',
    origin: 'Review',
    originBranch: 'feat/x',
  });
  channelData = DETAIL;
  viewerLifecycle.mounts = 0;
  viewerLifecycle.unmounts = 0;
  routerReplace.mockClear();
});

afterEach(() => {
  cleanup();
  channelData = undefined;
});

function renderChangeScope() {
  return render(
    <TestSessionDetail sessionId="sess-12345678" />,
  );
}

function TestSessionDetail({ sessionId }: { sessionId: string }) {
  const routeQuery = parseTranscriptRouteQuery(currentSearchParams);
  if (!routeQuery) throw new Error('test transcript query must be valid');
  return <SessionDetailV2 sessionId={sessionId} projectHash={PROJECT_HASH} projectName="alpha-project" routeQuery={routeQuery} />;
}

function assertDataAttribute(element: HTMLElement, attribute: string, expected: string, invariant: string): void {
  const received = element.getAttribute(attribute);
  if (received !== expected) throw new Error(`${invariant} invariant failed: ${attribute} expected ${expected}, received ${received ?? 'missing'}`);
}

function assertRouterCallToken(expected: string, invariant: string): void {
  const calls = routerReplace.mock.calls.map((call) => call[0]);
  const wanted = expected === 'none' ? [] : [expected];
  if (JSON.stringify(calls) !== JSON.stringify(wanted)) throw new Error(`${invariant} invariant failed: expected router call tokens ${JSON.stringify(wanted)}, received ${JSON.stringify(calls)}`);
}

// -- Breadcrumb trail ---------------------------------------------------------

function crumbs(): [string, string | null][] {
  return JSON.parse(screen.getByTestId('package-breadcrumb').getAttribute('data-crumbs') ?? '[]');
}

describe('SessionDetailV2 — breadcrumb trail', () => {
  it('walks projects → this project\'s sessions → this session', () => {
    currentSearchParams = new URLSearchParams();
    renderChangeScope();

    const trail = crumbs();
    expect(trail.map(([label]) => label)).toEqual([
      'projects',
      'alpha-project',
      'sess-123',
    ]);
    // The root returns to the project list; the project crumb returns to THAT
    // project's session list — never to the capability-gated map.
    expect(trail[0][1]).toBe('/');
    expect(trail[1][1]).toBe(`/sessions/${PROJECT_HASH}`);
    expect(trail.some(([, href]) => href?.startsWith('/map'))).toBe(false);
    // The session you are on is the end of the trail, so it is not a link.
    expect(trail[2][1]).toBeNull();
  });
});

// -- Change scope (now filters; no longer a dead-end) --------------------------

describe('SessionDetailV2 — change scope', () => {
  it('renders the filtered session (not a not-available dead-end)', () => {
    const { container } = renderChangeScope();

    // It reaches the real viewer (the package), not an adapter empty state.
    expect(screen.getByTestId('package-session-detail')).toBeInTheDocument();
    expect(container.textContent).not.toMatch(/isn't available yet/i);
    expect(container.textContent).not.toMatch(/wave|coming/i);
  });

  it('shows the change scope chip with a clear "x"', () => {
    renderChangeScope();
    expect(
      screen.getByText(/Showing only this change’s requests/),
    ).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Clear scope' })).toBeInTheDocument();
  });

  it('clearing the scope removes the scope params but keeps the origin way back', () => {
    renderChangeScope();
    fireEvent.click(screen.getByRole('button', { name: 'Clear scope' }));
    expect(routerReplace).toHaveBeenCalledWith(
      `${PATHNAME}?origin=Review&originBranch=feat%2Fx`,
    );
  });

  it('keeps the scope chip while the rest of the removed "showing every step" chrome stays gone', () => {
    renderChangeScope();

    // The scope chip is the one piece of the old prelude that was explicitly
    // kept — present and functional even with the rest of the chrome removed.
    expect(screen.getByRole('button', { name: 'Clear scope' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /prompts & replies only/i })).not.toBeInTheDocument();
    expect(screen.queryByText(/showing every step/i)).not.toBeInTheDocument();
    expect(screen.queryByTestId('steps-waterfall')).not.toBeInTheDocument();
  });
});

describe('SessionDetailV2 — decoded target recovery', () => {
  it('rejects every strict loader mutation from YAML', () => {
    for (const mutation of recoveryFixture.loaderMutations) {
      const mutated = mutation.target === 'manifest'
        ? replaceExactlyOnce(recoveryManifestSource, mutation.find, mutation.replace, mutation.name)
        : replaceExactlyOnce(recoveryCasesSource, mutation.find, mutation.replace, mutation.name);
      expect(() => loadRecoveryCases(mutation.target === 'manifest' ? mutated : recoveryManifestSource, mutation.target === 'cases' ? mutated : recoveryCasesSource), mutation.name).toThrow(new RegExp(mutation.expectedError));
    }
  });

  for (const fixture of recoveryFixture.cases) {
    it(fixture.name, () => {
      currentSearchParams = new URLSearchParams(fixture.search);
      channelData = {
        ...DETAIL,
        turns: fixture.turns.map((index) => ({
          index,
          role: index === 42 ? fixture.targetRole : 'user',
          depth: index === 42 ? fixture.targetDepth : 0,
          content: `turn ${index}`,
          toolCalls: [],
        })),
      };

      const view = render(<TestSessionDetail sessionId="sess-12345678" />);
      const viewer = screen.getByTestId('package-session-detail');
      assertDataAttribute(viewer, 'data-initial-kind', fixture.expectedInitialKind, `${fixture.family} initial position`);
      assertDataAttribute(viewer, 'data-initial-turn', String(fixture.expectedInitialTurn), `${fixture.family} initial position`);
      assertDataAttribute(viewer, 'data-initial-request-key', fixture.expectedInitialRequestKey, `${fixture.family} initial position`);
      if (fixture.expectedFeedback === 'none') expect(screen.queryByText(/linked turn is/i)).not.toBeInTheDocument();
      else expect(screen.getByText(fixture.expectedFeedback)).toBeInTheDocument();

      if (fixture.action !== 'none') fireEvent.click(screen.getByRole('button', { name: fixture.action }));
      assertRouterCallToken(fixture.expectedReplace, `${fixture.family} router call token`);
      if (fixture.expectedReplace !== 'none') {
        currentSearchParams = new URL(fixture.expectedReplace, 'https://peasant.invalid').searchParams;
        view.rerender(<TestSessionDetail sessionId="sess-12345678" />);
      }
      assertDataAttribute(viewer, 'data-initial-kind', fixture.expectedAfterActionKind, `${fixture.family} post-action position`);
      assertDataAttribute(viewer, 'data-initial-turn', String(fixture.expectedAfterActionTurn), `${fixture.family} post-action position`);
      assertDataAttribute(viewer, 'data-initial-request-key', fixture.expectedAfterActionRequestKey, `${fixture.family} post-action position`);
      expect(viewerLifecycle.mounts).toBe(1);
    });
  }
});

// -- Removed prelude + rehomed copy-as-markdown -------------------------------

describe('SessionDetailV2 — the "showing every step" prelude is gone', () => {
  beforeEach(() => {
    currentSearchParams = new URLSearchParams(); // no scope
    channelData = DETAIL;
  });

  it('copies the conversation as Markdown from its new home (headerActions, not the removed prelude)', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.assign(navigator, { clipboard: { writeText } });
    render(<TestSessionDetail sessionId="sess-12345678" />);

    const headerActions = screen.getByTestId('package-header-actions');
    fireEvent.click(within(headerActions).getByRole('button', { name: 'copy as markdown' }));

    expect(writeText).toHaveBeenCalledTimes(1);
    const md = writeText.mock.calls[0][0] as string;
    expect(md).toContain('## You');
    expect(md).toContain('q0'); // the DETAIL fixture's first user turn
  });

  it('does not render the "prompts & replies only" toggle, the "showing every step" sentence, or the steps waterfall anywhere', () => {
    render(<TestSessionDetail sessionId="sess-12345678" />);

    expect(screen.queryByRole('button', { name: /prompts & replies only/i })).not.toBeInTheDocument();
    expect(screen.queryByText(/showing every step/i)).not.toBeInTheDocument();
    expect(screen.queryByTestId('steps-waterfall')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /^steps \(/i })).not.toBeInTheDocument();
  });

  it('does not render the scope chip when there is no active Map/Review scope (it is conditional, not part of the removed chrome)', () => {
    render(<TestSessionDetail sessionId="sess-12345678" />);

    expect(screen.queryByRole('button', { name: 'Clear scope' })).not.toBeInTheDocument();
    expect(screen.queryByText(/^Showing /)).not.toBeInTheDocument();
  });

  it('keeps the bounded transcript-view shell height class intact after the prelude removal', () => {
    const { container } = render(<TestSessionDetail sessionId="sess-12345678" />);
    expect(container.querySelector('[data-tour="transcript-view"]')).toHaveClass(
      'h-[calc(100dvh-var(--app-header-height))]',
    );
  });

  it('keeps the transcript viewer mounted when the host switches to a different session', () => {
    const view = render(
      <TestSessionDetail sessionId="sess-12345678" />,
    );
    expect(viewerLifecycle.mounts).toBe(1);

    channelData = { ...DETAIL, id: 'sess-87654321' };
    view.rerender(
      <TestSessionDetail sessionId="sess-87654321" />,
    );
    expect(viewerLifecycle.mounts).toBe(1);
    expect(viewerLifecycle.unmounts).toBe(0);
  });
});
