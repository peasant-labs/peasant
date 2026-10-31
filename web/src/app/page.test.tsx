import { describe, it, expect, vi, afterEach, beforeEach } from 'vitest';
import { DOCS } from '@/lib/docs';
import { render, screen, cleanup, fireEvent, waitFor, act } from '@testing-library/react';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import HomePage from './page';
vi.mock('@/contexts/PublishContext', () => ({ usePublishState: () => ({ settled: stableSettled }) }));
const stableSettled = new Map();
vi.mock('@/lib/api/home', () => ({ fetchHomeSessions: () => new Promise(() => {}) }));
import { newProjectHash, type ProjectSummary } from '@peasant-labs/schema';
import type { DecodedProjectSummariesPayload } from '@/lib/api/map';
import type { SessionsPayload } from '@/types/messages';
import {
  ALPHA_HASH,
  BETA_HASH,
  makeSession,
} from '@/app/review/[[...segments]]/test-fixtures';
import { DiscoveryRequestError } from '@/lib/api/errors';
import {
  parseStrictYAML,
  requireExactRequiredFields,
  requireRecord,
  requireUniqueNames,
} from '@/test/strictYaml';
import { projectViewerStateFixture } from '@/components/picker/projectViewerStateFixtures';
import { localReviewClarityFixture, makeClarityProjectSummaries } from '@/test/fixtures/localReviewClarity';
import { browseBy, browseTrigger } from '@/test/homeMenus';

// Home's picker rows are the only route into a project; no embedded graph
// mounts here any more, so no app router is required. Mock it anyway so a
// transitive Link/router read can never hit "invariant expected app router".
vi.mock('next/navigation', () => ({ useRouter: () => ({ push: vi.fn() }) }));

// The home reads ambient liveness from the sessions WS channel. The stats
// strip's topics (dashboard, trends, quality) stay unloaded here; their values
// have their own mounted test (page.root-list.test.tsx).
let channelData: SessionsPayload | undefined;
let channelConnected = true;
let channelError: Error | null = null;
let channelErrorCode: 'selection_visibility' | undefined;
vi.mock('@/contexts/WebSocketContext', () => ({
  useChannel: (subs: unknown) => {
    const first = Array.isArray(subs) ? subs[0] : subs;
    const topic = typeof first === 'string' ? first : (first as { topic: string }).topic;
    return {
      data: topic === 'sessions' ? channelData : undefined,
      connected: channelConnected,
      error: topic === 'sessions' ? channelError : null,
      errorCode: topic === 'sessions' ? channelErrorCode : undefined,
    };
  },
}));

// The session list's reads stay pending: these tests assert the picker,
// selection and retained-flow policy; the list has its own mounted tests.
const listApi = vi.hoisted(() => ({
  fetchSyncSessions: vi.fn(),
  fetchPublications: vi.fn(),
}));
vi.mock('@/lib/api/publications', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/lib/api/publications')>();
  return { ...actual, ...listApi };
});

/** Switches Home to one of its retained views from the `browse by` menu. */
async function openView(name: string): Promise<void> {
  await browseBy(name);
}
const PROJECTS_VIEW = 'project';
const GROUPED_VIEW = 'helper threads';
const FLAT_VIEW = 'every session';

// REST stubs — the home fetches per-project summaries (picker rows) only.
const api = vi.hoisted(() => ({
  fetchProjectSummaries: vi.fn<() => Promise<DecodedProjectSummariesPayload>>(),
  // The render-now seed: tests exercise the cold path (no cached payload).
  cachedProjectSummaries: () => null,
}));
vi.mock('@/lib/api/map', () => api);

// The grouped local list is a REST read driven by the live sessions channel.
// These tests assert the picker/selection policy, so the grouped route stubs an
// explicit empty payload; the grouped decode, grouping and member paging have
// their own real-route tests.
const groupedApi = vi.hoisted(() => ({
  fetchGroupedLocalSessions: vi.fn(),
  fetchGroupedLocalSearch: vi.fn(),
}));
vi.mock('@/lib/api/grouped', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/lib/api/grouped')>();
  return { ...actual, ...groupedApi };
});

const GROUPED_HOME_PAYLOAD = {
  items: [
    {
      kind: 'transcript',
      transcript: { session: makeSession({ id: 'sg-owner' }) },
      helperGroups: [
        { groupId: 'hg_home', purpose: 'helper_review', helperThreadCount: 2, memberScope: 'scope-home' },
      ],
    },
    {
      kind: 'transcript',
      transcript: { session: makeSession({ id: 'sg-ordinary' }) },
      helperGroups: [],
    },
  ],
  page: 1,
  limit: 20,
  totalItems: 2,
  ordinarySessionTotal: 2,
  helperThreadTotal: 2,
};

/** A pending promise — keeps the summaries fetch "loading" for a test. */
function pending<T>(): Promise<T> {
  return new Promise<T>(() => {});
}

function makeSummary(over: Partial<ProjectSummary>): ProjectSummary {
  return {
    projectHash: ALPHA_HASH,
    project: 'alpha-project',
    sessions: 12,
    recordedFiles: 34,
    totalFiles: 37,
    lastWorkMs: Date.now() - 2 * 3_600_000, // 2h ago
    openChanges: 2,
    ...over,
  };
}

/** Wraps a project row list into the full wire shape, defaulting selection
 * to inactive/nothing-hidden — most tests aren't exercising the
 * selection-state banner and don't want to repeat that boilerplate. */
function makeSummaries(
  projects: ProjectSummary[],
  selection: DecodedProjectSummariesPayload['selection'] = { active: false, hiddenProjects: 0, hiddenSessions: 0 },
): DecodedProjectSummariesPayload {
  return { projects, selection };
}

function replaceExactlyOnce(source: string, find: string, replace: string, label: string): string {
  const count = source.split(find).length - 1;
  if (count !== 1) throw new Error(`${label} mutation anchor must occur exactly once, received ${count}`);
  return source.replace(find, replace);
}

const requiredSelectionScopeCaseNames = [
  'restrictive saved selection keeps the mode-all library',
  'empty saved selection keeps the mode-all library',
  'excluded saved selection keeps the mode-all library',
  'stale saved selection keeps the mode-all library',
] as const;

type SelectionScopeCase = {
  name: (typeof requiredSelectionScopeCaseNames)[number];
  projects: Array<{ project: string; projectHash: string }>;
  selection: DecodedProjectSummariesPayload['selection'];
  expectedProjects: string[];
};

function loadSelectionScopeFixture(): { cases: SelectionScopeCase[] } {
  const source = readFileSync(resolve(process.cwd(), 'src/app/testdata/home_selection_scope.yaml'), 'utf8');
  const root = requireRecord(parseStrictYAML(source, 'home selection scope fixture'), 'home selection scope fixture');
  requireExactRequiredFields(root, ['expectedCaseCount', 'cases'], 'home selection scope fixture');
  if (root.expectedCaseCount !== requiredSelectionScopeCaseNames.length) {
    throw new Error(`home selection scope fixture expectedCaseCount must equal independently defined ${requiredSelectionScopeCaseNames.length}`);
  }
  if (!Array.isArray(root.cases) || root.cases.length !== requiredSelectionScopeCaseNames.length) {
    throw new Error(`home selection scope fixture must contain ${requiredSelectionScopeCaseNames.length} cases`);
  }
  const cases = root.cases.map((value, index) => requireRecord(value, `home selection scope fixture.cases[${index}]`));
  requireUniqueNames(cases, 'home selection scope fixture.cases');
  for (const [index, testCase] of cases.entries()) {
    const label = `home selection scope fixture.cases[${index}]`;
    requireExactRequiredFields(testCase, ['name', 'projects', 'selection', 'expectedProjects'], label);
    if (!requiredSelectionScopeCaseNames.includes(testCase.name as SelectionScopeCase['name'])) {
      throw new Error(`${label}.name has unknown semantic case ${String(testCase.name)}`);
    }
    if (!Array.isArray(testCase.projects) || testCase.projects.length === 0) {
      throw new Error(`${label}.projects must name at least one recorded project`);
    }
    for (const [projectIndex, project] of testCase.projects.entries()) {
      const projectLabel = `${label}.projects[${projectIndex}]`;
      requireExactRequiredFields(requireRecord(project, projectLabel), ['project', 'projectHash'], projectLabel);
      const row = project as Record<string, unknown>;
      if (typeof row.project !== 'string' || row.project.length === 0) throw new Error(`${projectLabel}.project must be a non-empty string`);
      if (typeof row.projectHash !== 'string' || !/^[0-9a-f]{64}$/.test(row.projectHash)) throw new Error(`${projectLabel}.projectHash must be a 64-character lowercase hex hash`);
    }
    const selection = requireRecord(testCase.selection, `${label}.selection`);
    requireExactRequiredFields(selection, ['active', 'hiddenProjects', 'hiddenSessions'], `${label}.selection`);
    if (typeof selection.active !== 'boolean') throw new Error(`${label}.selection.active must be a boolean`);
    for (const field of ['hiddenProjects', 'hiddenSessions'] as const) {
      if (!Number.isSafeInteger(selection[field]) || (selection[field] as number) < 0) {
        throw new Error(`${label}.selection.${field} must be a non-negative integer`);
      }
    }
    if (!Array.isArray(testCase.expectedProjects) || testCase.expectedProjects.length !== testCase.projects.length) {
      throw new Error(`${label}.expectedProjects must name every recorded project`);
    }
  }
  for (const name of requiredSelectionScopeCaseNames) {
    if (!cases.some((testCase) => testCase.name === name)) {
      throw new Error(`home selection scope fixture is missing required semantic case ${name}`);
    }
  }
  return root as unknown as { cases: SelectionScopeCase[] };
}

const requiredFlatSessionCaseNames = [
  'cross-project-filter-and-top-level-paging',
  'harness-filter-narrows-the-same-set',
] as const;

type FlatSessionCase = {
  name: string;
  sessionCount: number;
  projects: string[];
  harnesses: string[];
  filterQuery: string;
  expectedFilteredCount: number;
  expectedTotalPages: number;
  expectedFilteredPages: number;
};

const flatSessionsManifestSource = readFileSync(
  resolve(process.cwd(), 'src/app/testdata/home_flat_sessions.manifest.yaml'),
  'utf8',
);
const flatSessionsCasesSource = readFileSync(
  resolve(process.cwd(), 'src/app/testdata/home_flat_sessions.yaml'),
  'utf8',
);

function loadFlatSessionFixture(
  manifestSource = flatSessionsManifestSource,
  casesSource = flatSessionsCasesSource,
): FlatSessionCase[] {
  const manifest = requireRecord(
    parseStrictYAML(manifestSource, 'home flat sessions manifest'),
    'home flat sessions manifest',
  );
  requireExactRequiredFields(
    manifest,
    ['expectedCount', 'requiredNames', 'expectedLoaderMutationCount', 'loaderMutations'],
    'home flat sessions manifest',
  );
  const requiredNames = manifest.requiredNames as unknown[];
  if (
    !Number.isSafeInteger(manifest.expectedCount)
    || !Array.isArray(requiredNames)
    || requiredNames.length !== manifest.expectedCount
    || requiredNames.length !== requiredFlatSessionCaseNames.length
  ) {
    throw new Error('home flat sessions manifest must name every required case exactly once');
  }
  if (
    !Number.isSafeInteger(manifest.expectedLoaderMutationCount)
    || !Array.isArray(manifest.loaderMutations)
    || manifest.loaderMutations.length !== manifest.expectedLoaderMutationCount
  ) {
    throw new Error('home flat sessions manifest must carry one loader mutation per expected mutation');
  }
  const root = requireRecord(parseStrictYAML(casesSource, 'home flat sessions cases'), 'home flat sessions cases');
  requireExactRequiredFields(root, ['cases'], 'home flat sessions cases');
  if (!Array.isArray(root.cases)) throw new Error('home flat sessions cases.cases must be an array');
  const rows = root.cases.map((value, index) => requireRecord(value, `home flat sessions cases.cases[${index}]`));
  requireUniqueNames(rows, 'home flat sessions cases.cases');
  rows.forEach((row, index) => {
    const label = `home flat sessions cases.cases[${index}]`;
    requireExactRequiredFields(
      row,
      ['name', 'sessionCount', 'projects', 'harnesses', 'filterQuery', 'expectedFilteredCount', 'expectedTotalPages', 'expectedFilteredPages'],
      label,
    );
    if (!Number.isSafeInteger(row.sessionCount) || (row.sessionCount as number) < 1) {
      throw new Error(`${label}.sessionCount must be a positive integer`);
    }
    if (!Array.isArray(row.projects) || row.projects.length === 0) {
      throw new Error(`${label}.projects must name at least one project`);
    }
    if (!Array.isArray(row.harnesses) || row.harnesses.length === 0) {
      throw new Error(`${label}.harnesses must name at least one harness`);
    }
    if (typeof row.filterQuery !== 'string' || row.filterQuery === '') {
      throw new Error(`${label}.filterQuery must be a non-empty string`);
    }
    for (const field of ['expectedFilteredCount', 'expectedTotalPages', 'expectedFilteredPages'] as const) {
      if (!Number.isSafeInteger(row[field]) || (row[field] as number) < 1) {
        throw new Error(`${label}.${field} must be a positive integer`);
      }
    }
  });
  const names = rows.map((row) => row.name);
  if (
    rows.length !== manifest.expectedCount
    || requiredFlatSessionCaseNames.some((name) => !names.includes(name))
  ) {
    throw new Error('home flat sessions manifest must name exactly the required cases; a case is missing or renamed');
  }
  for (const name of requiredFlatSessionCaseNames) {
    if (!names.includes(name)) throw new Error(`home flat sessions fixture is missing required case ${name}`);
  }
  return rows as unknown as FlatSessionCase[];
}

const flatSessionFixture = loadFlatSessionFixture();

function deferred<T>() {
  let resolvePromise!: (value: T) => void;
  let rejectPromise!: (reason: unknown) => void;
  const promise = new Promise<T>((resolve, reject) => {
    resolvePromise = resolve;
    rejectPromise = reject;
  });
  return { promise, resolve: resolvePromise, reject: rejectPromise };
}

const selectionScopeFixture = loadSelectionScopeFixture();

describe('HomePage — the changes-first picker', () => {
  beforeEach(() => {
    api.fetchProjectSummaries.mockReturnValue(pending());
    // Keep the grouped route pending: these tests assert the picker/selection
    // policy, and a resolved grouped payload would commit an unrelated state
    // update after the synchronous assertions.
    groupedApi.fetchGroupedLocalSessions.mockReturnValue(pending());
    groupedApi.fetchGroupedLocalSearch.mockReturnValue(pending());
    listApi.fetchSyncSessions.mockReturnValue(pending());
    listApi.fetchPublications.mockReturnValue(pending());
  });

  afterEach(() => {
    cleanup();
    channelData = undefined;
    channelConnected = true;
    channelError = null;
    channelErrorCode = undefined;
    vi.clearAllMocks();
  });

  it('mounts the fixture-backed changes picker with review clarity and filtering', async () => {
    const testCase = localReviewClarityFixture.pickerCases.find((row) => row.surface === 'home')!;
    channelData = { sessions: [makeSession({ id: 'clarity-home', project: testCase.targetProject })] };
    api.fetchProjectSummaries.mockResolvedValue(makeSummaries(makeClarityProjectSummaries(testCase)));
    render(<HomePage />);
    await openView(PROJECTS_VIEW);

    const search = await screen.findByRole('searchbox', { name: localReviewClarityFixture.copy.searchAccessibleName });
    expect(search).toHaveClass('input', 'is-input');
    expect(search).toHaveAttribute('placeholder', localReviewClarityFixture.copy.searchPlaceholder);
    expect(search.closest('.input-ico')?.querySelector('svg')).toHaveAttribute('aria-hidden', 'true');
    expect(screen.getByRole('button', { name: localReviewClarityFixture.copy.coverageHelpName })).toHaveTextContent(localReviewClarityFixture.copy.coverageVisibleLabel);
    expect(screen.getByRole('link', { name: testCase.expectedLinkName })).toHaveAttribute('href', testCase.expectedHref);

    fireEvent.change(search, { target: { value: 'no matching project' } });
    expect(screen.getByText('no projects match “', { exact: false }).closest('p')).toHaveTextContent('no projects match “no matching project”.');
    fireEvent.change(search, { target: { value: testCase.searchQuery } });
    expect(screen.getByRole('link', { name: testCase.expectedLinkName })).toHaveAttribute('href', testCase.expectedHref);
  });

  it('teaches the lifecycle when no sessions exist', async () => {
    const fixture = projectViewerStateFixture('genuine no data');
    channelData = { sessions: [] };
    api.fetchProjectSummaries.mockResolvedValue(fixture.summary);
    render(<HomePage />);
    // The empty state names what is missing and links to the docs that teach recording.
    await openView(PROJECTS_VIEW);
    expect(await screen.findByText('no ai work recorded yet')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: /^how to record sessions/ })).toHaveAttribute('href', DOCS.record);
    expect(screen.queryByText('peasant ingest')).not.toBeInTheDocument();
    // No ledger line without sessions.
    expect(screen.queryByText(/on this machine/)).not.toBeInTheDocument();
  });

  for (const testCase of selectionScopeFixture.cases) {
    it(`lists the ingested library: ${testCase.name}`, async () => {
      channelData = { sessions: [] };
      api.fetchProjectSummaries.mockResolvedValue(makeSummaries(
        testCase.projects.map(project => makeSummary({
          project: project.project,
          projectHash: newProjectHash(project.projectHash),
        })),
        testCase.selection,
      ));
      render(<HomePage />);
      await openView(PROJECTS_VIEW);

      for (const project of testCase.expectedProjects) {
        expect(await screen.findByRole('link', { name: `Open the sessions of ${project}` })).toBeInTheDocument();
      }
      // The saved selection is ingestion metadata: it produces no hidden
      // notice, no all-hidden recovery state, and no kickstart instruction.
      expect(screen.queryByText(/hidden by a saved selection/)).not.toBeInTheDocument();
      expect(screen.queryByText(/your saved selection hides all projects/)).not.toBeInTheDocument();
      expect(screen.queryByRole('status', { name: 'project selection recovery' })).not.toBeInTheDocument();
      expect(screen.queryByText(/peasant kickstart/)).not.toBeInTheDocument();
      expect(screen.queryByText('peasant ingest')).not.toBeInTheDocument();
    });
  }

  it('keeps project summaries lazy and omits the unavailable global ledger', async () => {
    channelData = {
      sessions: [
        makeSession({ id: 's1', project: 'alpha-project' }),
        makeSession({ id: 's2', project: 'alpha-project' }),
        makeSession({ id: 's3', project: 'beta-project', projectHash: BETA_HASH }),
      ],
    };
    api.fetchProjectSummaries.mockResolvedValue(makeSummaries([
      // beta last-worked before alpha — alpha must sort first.
      makeSummary({
        project: 'beta-project',
        projectHash: BETA_HASH,
        sessions: 4,
        recordedFiles: 0,
        totalFiles: 0,
        lastWorkMs: Date.now() - 3 * 86_400_000,
        openChanges: 0,
      }),
      makeSummary({ project: 'alpha-project', sessions: 12 }),
    ]));
    render(<HomePage />);

    expect(api.fetchProjectSummaries).not.toHaveBeenCalled();
    expect(screen.queryByText(/sessions on this machine/)).not.toBeInTheDocument();
    await openView(PROJECTS_VIEW);

    // Rows link into /sessions/{projectHash} — that project's session list.
    const alpha = await screen.findByRole('link', {
      name: 'Open the sessions of alpha-project',
    });
    expect(alpha).toHaveAttribute('href', `/sessions/${ALPHA_HASH}`);
    const beta = screen.getByRole('link', { name: 'Open the sessions of beta-project' });
    expect(beta).toHaveAttribute('href', `/sessions/${BETA_HASH}`);

    // Most recent work first.
    const links = screen.getAllByRole('link', { name: /Open the sessions of/ });
    expect(links[0]).toBe(alpha);

    // Per-row stats: AI-built files · last work · in-progress count.
    expect(alpha.textContent).toContain('34 of 37');
    expect(alpha.textContent).toContain('2h ago');
    // Zero total files → coverage unknown, not "0 of 0".
    expect(beta.textContent).toContain('—');
    expect(beta.textContent).toContain('3d ago');

    // No change graph embedded on the home anymore.
    expect(screen.queryByLabelText(/Change history for/)).not.toBeInTheDocument();
  });

  it('E1: shows aggregate summary cards above the multi-project picker', async () => {
    channelData = {
      sessions: [
        makeSession({ id: 's1', project: 'alpha-project' }),
        makeSession({ id: 's2', project: 'beta-project', projectHash: BETA_HASH }),
      ],
    };
    api.fetchProjectSummaries.mockResolvedValue(makeSummaries([
      makeSummary({ project: 'alpha-project', recordedFiles: 34, totalFiles: 37, openChanges: 2 }),
      makeSummary({
        project: 'beta-project',
        projectHash: BETA_HASH,
        recordedFiles: 6,
        totalFiles: 63,
        openChanges: 1,
      }),
    ]));
    render(<HomePage />);
    await openView(PROJECTS_VIEW);

    // StatGrid labels are lowercase chrome; values are data (pre-formatted).
    // 2 projects, coverage (34+6)/(37+63)=40%, open 2+1=3.
    // "projects" labels BOTH the stat tile and the breadcrumb, so match on
    // presence rather than uniqueness (same as "unmerged branches" below).
    expect((await screen.findAllByText('projects')).length).toBeGreaterThanOrEqual(1);
    expect(screen.getByText('files built with ai')).toBeInTheDocument();
    expect(screen.getByText('40%')).toBeInTheDocument();
    expect(screen.getByText('40 of 100 files')).toBeInTheDocument();
    // "unmerged branches" labels BOTH the stat tile and the picker column.
    expect(screen.getAllByText('unmerged branches').length).toBeGreaterThanOrEqual(1);
  });

  it('mounts the grouped local session list from the grouped REST route', async () => {
    channelData = { sessions: [makeSession({ id: 'sg-owner', project: 'alpha-project' })] };
    api.fetchProjectSummaries.mockResolvedValue(makeSummaries([makeSummary({ project: 'alpha-project' })]));
    groupedApi.fetchGroupedLocalSessions.mockResolvedValue(GROUPED_HOME_PAYLOAD);
    render(<HomePage />);
    await openView(GROUPED_VIEW);

    expect(await screen.findByText('all sessions')).toBeInTheDocument();
    expect(await screen.findByText('2 helper threads')).toBeInTheDocument();
    expect(screen.getByText('sg-owner')).toBeInTheDocument();
    expect(screen.getByText('2 sessions · 2 helper threads')).toBeInTheDocument();
    expect(groupedApi.fetchGroupedLocalSessions).toHaveBeenCalled();
  });

  it('rejects every retained flat-session loader mutation', () => {
    const manifest = requireRecord(
      parseStrictYAML(flatSessionsManifestSource, 'home flat sessions manifest'),
      'home flat sessions manifest',
    );
    for (const mutationValue of manifest.loaderMutations as unknown[]) {
      const mutation = requireRecord(mutationValue, 'flat session loader mutation');
      const target = mutation.target === 'manifest' ? flatSessionsManifestSource : flatSessionsCasesSource;
      const mutated = replaceExactlyOnce(target, String(mutation.find), String(mutation.replace), String(mutation.name));
      expect(
        () =>
          loadFlatSessionFixture(
            mutation.target === 'manifest' ? mutated : flatSessionsManifestSource,
            mutation.target === 'cases' ? mutated : flatSessionsCasesSource,
          ),
        String(mutation.name),
      ).toThrow(new RegExp(String(mutation.expectedError)));
    }
  });

  for (const testCase of flatSessionFixture) {
    it(`retains the cross-project filter and top-level pager: ${testCase.name}`, async () => {
      const sessions = Array.from({ length: testCase.sessionCount }, (_, index) =>
        makeSession({
          id: `sess-${String(index).padStart(4, '0')}`,
          project: testCase.projects[index % testCase.projects.length],
          projectHash: index % testCase.projects.length === 0 ? ALPHA_HASH : BETA_HASH,
          harness: testCase.harnesses[index % testCase.harnesses.length] as 'codex',
          startTime: new Date(Date.UTC(2026, 0, 1) - index * 3_600_000).toISOString(),
        }),
      );
      channelData = { sessions };
      api.fetchProjectSummaries.mockResolvedValue(
        makeSummaries(
          testCase.projects.map((project, index) =>
            makeSummary({ project, projectHash: index === 0 ? ALPHA_HASH : BETA_HASH }),
          ),
        ),
      );
      groupedApi.fetchGroupedLocalSessions.mockResolvedValue(GROUPED_HOME_PAYLOAD);

      render(<HomePage />);
      // The grouped list stays reachable with its own helper counts ...
      await openView(GROUPED_VIEW);
      expect(await screen.findByText('2 helper threads')).toBeInTheDocument();
      expect(groupedApi.fetchGroupedLocalSessions).toHaveBeenCalledTimes(1);

      // ... and the flat filter + pager stay reachable from the same menu.
      await openView(FLAT_VIEW);
      expect(screen.getByText(`${testCase.sessionCount} sessions`)).toBeInTheDocument();
      expect(screen.getByText(`page 1 of ${testCase.expectedTotalPages}`)).toBeInTheDocument();

      // Filtering by the previous supported field narrows to the server-visible
      // set without touching the grouped helper membership.
      fireEvent.change(screen.getByRole('searchbox', { name: 'search sessions' }), {
        target: { value: testCase.filterQuery },
      });
      expect(
        screen.getByText(`${testCase.expectedFilteredCount} sessions of ${testCase.sessionCount}`),
      ).toBeInTheDocument();
      expect(screen.getByText(`page 1 of ${testCase.expectedFilteredPages}`)).toBeInTheDocument();
      expect(groupedApi.fetchGroupedLocalSessions).toHaveBeenCalledTimes(1);

      // The top-level pager navigates the filtered result.
      fireEvent.click(screen.getByRole('button', { name: 'next' }));
      expect(
        screen.getByText(`page ${testCase.expectedFilteredPages} of ${testCase.expectedFilteredPages}`),
      ).toBeInTheDocument();
    });
  }

  it('falls back to sessions-channel grouping with stats unavailable while the fetch loads', async () => {
    api.fetchProjectSummaries.mockReturnValue(pending());
    channelData = {
      sessions: [
        makeSession({ id: 's1', project: 'alpha-project', startTime: '2026-06-03T09:00:00Z' }),
        makeSession({ id: 's2', project: 'alpha-project', startTime: '2026-06-02T09:00:00Z' }),
        makeSession({ id: 's3', project: 'beta-project', projectHash: BETA_HASH }),
      ],
    };
    render(<HomePage />);
    await openView(PROJECTS_VIEW);

    const alpha = screen.getByRole('link', { name: 'Open the sessions of alpha-project' });
    expect(alpha).toHaveAttribute('href', `/sessions/${ALPHA_HASH}`);
    // Coverage + unmerged-branch counts are summary-only: while the fetch is in
    // flight they SHIMMER in place rather than showing "—" then popping to a
    // value. Last-work comes from the sessions channel, so it's real text.
    expect(alpha.querySelectorAll('[data-slot="skeleton"]').length).toBeGreaterThan(0);
    expect(alpha.textContent).toMatch(/ago/);
  });

  it('mounts pending statistic placeholders without invalid nesting or hydration diagnostics', async () => {
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});
    channelData = {
      sessions: [
        makeSession({ id: 's1', project: 'alpha-project' }),
        makeSession({ id: 's2', project: 'beta-project', projectHash: BETA_HASH }),
      ],
    };
    render(<HomePage />);
    await openView(PROJECTS_VIEW);
    expect(screen.getAllByText('unmerged branches').length).toBeGreaterThan(0);
    const diagnostics = consoleError.mock.calls.flat().map(String).join('\n');
    expect(diagnostics).not.toMatch(/cannot be a descendant|hydration/i);
    consoleError.mockRestore();
  });

  it('falls back the same way when the summary fetch fails', async () => {
    api.fetchProjectSummaries.mockRejectedValue(new Error('boom'));
    channelData = {
      sessions: [
        makeSession({ id: 's1', project: 'alpha-project' }),
        makeSession({ id: 's2', project: 'beta-project', projectHash: BETA_HASH }),
      ],
    };
    render(<HomePage />);
    await openView(PROJECTS_VIEW);

    expect(
      await screen.findByRole('link', { name: 'Open the sessions of alpha-project' }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole('link', { name: 'Open the sessions of beta-project' }),
    ).toBeInTheDocument();
  });

  it('keeps an ingested project browsable when the summary read reports a legacy selection error', async () => {
    channelData = { sessions: [makeSession({ id: 'stored', project: 'stored-project' })] };
    api.fetchProjectSummaries.mockRejectedValue(new DiscoveryRequestError(
      '/api/v1/projects/summary',
      500,
      'project discovery failed while reading the summary',
      'selection_visibility',
    ));
    render(<HomePage />);
    await openView(PROJECTS_VIEW);

    // The failure is reported, with a retry, as the ordinary error it is. The
    // stored project from the sessions channel stays browsable, and the page
    // does not teach a kickstart recovery for a browsing restriction that no
    // longer exists.
    expect(await screen.findByText(/project discovery failed while reading the summary/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /retry project discovery/i })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Open the sessions of stored-project' })).toBeInTheDocument();
    expect(screen.queryByText(/peasant kickstart/)).not.toBeInTheDocument();
    expect(screen.queryByText(/hidden by a saved selection/)).not.toBeInTheDocument();
    expect(browseTrigger()).toHaveTextContent(`browse by: ${PROJECTS_VIEW}`);
  });

  it('retries a genuine summary failure and replaces the fallback rows', async () => {
    channelData = { sessions: [makeSession({ id: 'stored', project: 'stored-project' })] };
    const retry = deferred<DecodedProjectSummariesPayload>();
    api.fetchProjectSummaries
      .mockRejectedValueOnce(new Error('database unavailable'))
      .mockReturnValueOnce(retry.promise);
    render(<HomePage />);
    await openView(PROJECTS_VIEW);

    expect(await screen.findByText(/database unavailable/)).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Open the sessions of stored-project' })).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: /retry project discovery/i }));
    expect(api.fetchProjectSummaries).toHaveBeenCalledTimes(2);

    retry.resolve(makeSummaries([makeSummary({ project: 'alpha-project' })]));
    expect(await screen.findByRole('link', { name: 'Open the sessions of alpha-project' })).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByText(/database unavailable/)).not.toBeInTheDocument());
  });

  it('shows a channel failure without kickstart guidance', async () => {
    channelError = new Error('live session updates failed');
    channelErrorCode = 'selection_visibility';
    channelData = undefined;
    render(<HomePage />);
    await openView(PROJECTS_VIEW);

    expect(await screen.findByText(/live session updates failed/)).toBeInTheDocument();
    expect(screen.queryByText(/peasant kickstart/)).not.toBeInTheDocument();
  });

  it('renders the ordinary picker for a single-project install — never an embedded change graph', async () => {
    channelData = { sessions: [makeSession({ id: 's1', project: 'alpha-project' })] };
    api.fetchProjectSummaries.mockResolvedValue(makeSummaries([makeSummary({ project: 'alpha-project' })]));
    render(<HomePage />);
    await openView(PROJECTS_VIEW);

    const row = await screen.findByRole('link', { name: 'Open the sessions of alpha-project' });
    expect(row).toHaveAttribute('href', `/sessions/${ALPHA_HASH}`);
    // The change graph is not embedded anywhere on the home surface.
    expect(screen.queryByLabelText(/Change history for/)).not.toBeInTheDocument();
    expect(screen.queryByRole('link', { name: /Open the line of work/ })).not.toBeInTheDocument();
    expect(document.querySelector('[data-tour="changes-list"]')).toBeNull();
    // A single-project install still gets the aggregate KPI grid.
    expect(screen.getByText('files built with ai')).toBeInTheDocument();
  });

  it('renders a single-project row from the sessions channel when summaries fail', async () => {
    api.fetchProjectSummaries.mockRejectedValue(new Error('boom'));
    channelData = { sessions: [makeSession({ id: 's1', project: 'alpha-project' })] };
    render(<HomePage />);
    await openView(PROJECTS_VIEW);

    const row = await screen.findByRole('link', { name: 'Open the sessions of alpha-project' });
    expect(row).toHaveAttribute('href', `/sessions/${ALPHA_HASH}`);
    expect(screen.queryByLabelText(/Change history for/)).not.toBeInTheDocument();
  });

  it('shows the disconnected state when the local app is unreachable', async () => {
    // Unreachable = no WS data AND the summaries REST failed; a healthy-but-empty
    // install (REST ok) shows the teach state, not a disconnect message.
    channelConnected = false;
    channelData = undefined;
    api.fetchProjectSummaries.mockRejectedValue(new Error('down'));
    render(<HomePage />);
    // DataState disconnected panel copy (connection ≠ content principle).
    await openView(PROJECTS_VIEW);
    expect(await screen.findByText(/lost connection to the local program/i)).toBeInTheDocument();
  });
});
