import { describe, it, expect, vi, afterEach, beforeEach } from 'vitest';
import { render, screen, cleanup, fireEvent, waitFor, within } from '@testing-library/react';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import HomePage from './page';
import { newProjectHash, type ProjectSummary } from '@peasant-labs/schema';
import type { DecodedProjectSummariesPayload } from '@/lib/api/map';
import type { SessionsPayload } from '@/types/messages';
import {
  ALPHA_HASH,
  BETA_HASH,
  REVIEW_LIST_PAYLOAD,
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

// ChangeGraph (embedded in the home's single-project change list) now calls
// useRouter for CommitGraph tip-row navigation; mock it here so tests never
// hit the "invariant expected app router to be mounted" error.
vi.mock('next/navigation', () => ({ useRouter: () => ({ push: vi.fn() }) }));

// The Changes home reads ambient liveness from the sessions WS channel.
let channelData: SessionsPayload | undefined;
let channelConnected = true;
let channelError: Error | null = null;
let channelErrorCode: 'selection_visibility' | undefined;
vi.mock('@/contexts/WebSocketContext', () => ({
  useChannel: () => ({
    data: channelData,
    connected: channelConnected,
    error: channelError,
    errorCode: channelErrorCode,
  }),
}));

// REST stubs — the home fetches per-project summaries (picker rows) and, on
// single-project installs, the project's review changes for the embedded
// ChangeList (the ChangeList itself renders for real — not mocked).
const api = vi.hoisted(() => ({
  fetchProjectSummaries: vi.fn<() => Promise<DecodedProjectSummariesPayload>>(),
  // The render-now seed: tests exercise the cold path (no cached payload).
  cachedProjectSummaries: () => null,
  fetchReviewChanges: vi.fn(),
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

const requiredSelectionRetryCaseNames = [
  'pending retry stays closed until selected replacement succeeds',
  'failed retry stays closed and actionable',
] as const;

type SelectionRetryFixture = {
  cases: Array<{
    name: string;
    hiddenProject: string;
    replacementProjects: Array<{ project: string; projectHash: string }>;
    retryOutcome: 'success' | 'failure';
  }>;
};

function loadSelectionRetryFixture(): SelectionRetryFixture {
  const source = readFileSync(resolve(process.cwd(), 'src/app/testdata/home_selection_retry.yaml'), 'utf8');
  const root = requireRecord(parseStrictYAML(source, 'home selection retry fixture'), 'home selection retry fixture');
  requireExactRequiredFields(root, ['expectedCaseCount', 'cases'], 'home selection retry fixture');
  if (root.expectedCaseCount !== requiredSelectionRetryCaseNames.length) {
    throw new Error(`home selection retry fixture expectedCaseCount must equal independently defined ${requiredSelectionRetryCaseNames.length}`);
  }
  if (!Array.isArray(root.cases) || root.cases.length !== requiredSelectionRetryCaseNames.length) {
    throw new Error(`home selection retry fixture must contain ${requiredSelectionRetryCaseNames.length} cases`);
  }
  const cases = root.cases.map((value, index) => requireRecord(value, `home selection retry fixture.cases[${index}]`));
  requireUniqueNames(cases, 'home selection retry fixture.cases');
  for (const [index, testCase] of cases.entries()) {
    requireExactRequiredFields(testCase, ['name', 'hiddenProject', 'replacementProjects', 'retryOutcome'], `home selection retry fixture.cases[${index}]`);
    if (!requiredSelectionRetryCaseNames.includes(testCase.name as (typeof requiredSelectionRetryCaseNames)[number])) {
      throw new Error(`home selection retry fixture has unknown semantic case ${String(testCase.name)}`);
    }
    if (!Array.isArray(testCase.replacementProjects)) {
      throw new Error(`home selection retry fixture.cases[${index}].replacementProjects must be an array`);
    }
    testCase.replacementProjects.forEach((project, projectIndex) => {
      requireExactRequiredFields(
        requireRecord(project, `home selection retry fixture.cases[${index}].replacementProjects[${projectIndex}]`),
        ['project', 'projectHash'],
        `home selection retry fixture.cases[${index}].replacementProjects[${projectIndex}]`,
      );
    });
    if (testCase.retryOutcome !== 'success' && testCase.retryOutcome !== 'failure') {
      throw new Error(`home selection retry fixture.cases[${index}].retryOutcome is invalid`);
    }
  }
  for (const name of requiredSelectionRetryCaseNames) {
    if (!cases.some((testCase) => testCase.name === name)) {
      throw new Error(`home selection retry fixture is missing required semantic case ${name}`);
    }
  }
  return root as unknown as SelectionRetryFixture;
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

const selectionRetryFixture = loadSelectionRetryFixture();

describe('HomePage — the changes-first picker', () => {
  beforeEach(() => {
    api.fetchProjectSummaries.mockReturnValue(pending());
    api.fetchReviewChanges.mockReturnValue(pending());
    // Keep the grouped route pending: these tests assert the picker/selection
    // policy, and a resolved grouped payload would commit an unrelated state
    // update after the synchronous assertions.
    groupedApi.fetchGroupedLocalSessions.mockReturnValue(pending());
    groupedApi.fetchGroupedLocalSearch.mockReturnValue(pending());
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

    const search = await screen.findByRole('searchbox', { name: localReviewClarityFixture.copy.searchAccessibleName });
    expect(search).toHaveClass('input', 'is-input');
    expect(search).toHaveAttribute('placeholder', localReviewClarityFixture.copy.searchPlaceholder);
    expect(search.closest('.input-ico')?.querySelector('svg')).toHaveAttribute('aria-hidden', 'true');
    expect(screen.getByRole('button', { name: localReviewClarityFixture.copy.coverageHelpName })).toHaveTextContent(localReviewClarityFixture.copy.coverageVisibleLabel);
    expect(screen.getByRole('link', { name: testCase.expectedLinkName })).toHaveAttribute('href', testCase.expectedHref);

    fireEvent.change(search, { target: { value: 'no matching project' } });
    expect(screen.getByText('No projects match “', { exact: false }).closest('p')).toHaveTextContent('No projects match “no matching project”.');
    fireEvent.change(search, { target: { value: testCase.searchQuery } });
    expect(screen.getByRole('link', { name: testCase.expectedLinkName })).toHaveAttribute('href', testCase.expectedHref);
  });

  it('teaches the lifecycle when no sessions exist', async () => {
    const fixture = projectViewerStateFixture('genuine no data');
    channelData = { sessions: [] };
    api.fetchProjectSummaries.mockResolvedValue(fixture.summary);
    render(<HomePage />);
    // TeachingEmptyState renders lowercase chrome title + the copy-able command.
    expect(await screen.findByText('no ai work recorded yet')).toBeInTheDocument();
    expect(screen.getByText('peasant ingest')).toBeInTheDocument();
    // No ledger line without sessions.
    expect(screen.queryByText(/on your machine/)).not.toBeInTheDocument();
  });

  it('shows one recovery panel instead of stale rows or first-use teaching when selection hides all data', async () => {
    const fixture = projectViewerStateFixture('all hidden by saved selection');
    channelData = {
      sessions: [
        makeSession({
          id: fixture.forbiddenIdentities[2],
          project: fixture.forbiddenIdentities[1],
        }),
      ],
    };
    api.fetchProjectSummaries.mockResolvedValue(fixture.summary);
    render(<HomePage />);

    const panel = await screen.findByRole('status', { name: 'project selection recovery' });
    expect(panel).toHaveTextContent('Peasant hides 2 projects and 5 sessions.');
    expect(panel).toHaveTextContent('The data stays ingested and indexed.');
    expect(panel).toHaveTextContent('It is not available for a future push.');
    expect(panel).toHaveTextContent('Peasant did not delete data.');
    expect(screen.queryByText('peasant ingest')).not.toBeInTheDocument();
    expect(screen.queryByText(/hidden by a saved selection/)).not.toBeInTheDocument();
    for (const identity of fixture.forbiddenIdentities) {
      expect(document.body.textContent).not.toContain(identity);
    }
  });

  it('renders an explicit session parent from the shared project summary without recovery guidance', async () => {
    const fixture = projectViewerStateFixture('explicit session makes parent visible');
    channelData = { sessions: [] };
    api.fetchProjectSummaries.mockResolvedValue(fixture.summary);
    render(<HomePage />);

    expect(
      await screen.findByRole('link', {
        name: `Open the sessions of ${fixture.expectedParentLabel}`,
      }),
    ).toBeInTheDocument();
    expect(screen.queryByRole('status', { name: 'project selection recovery' })).not.toBeInTheDocument();
    expect(screen.queryByText('peasant ingest')).not.toBeInTheDocument();
  });

  it('shows the ledger line and the picker rows from the summary endpoint', async () => {
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

    // Ledger line — the values copy survives the redesign.
    expect(
      await screen.findByText(/AI conversations, on your machine\. Nothing has left it\./),
    ).toBeInTheDocument();

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

    // No map embedded on the home anymore.
    expect(screen.queryByLabelText(/Map of/)).not.toBeInTheDocument();
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
      // The grouped list stays mounted with its own helper counts ...
      expect(await screen.findByText('2 helper threads')).toBeInTheDocument();
      expect(groupedApi.fetchGroupedLocalSessions).toHaveBeenCalledTimes(1);

      // ... and the flat filter + pager stay reachable beside it.
      fireEvent.click(
        screen.getByRole('button', { name: /filter and page through every session/i }),
      );
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
      expect(screen.getByText('2 helper threads')).toBeInTheDocument();
      expect(groupedApi.fetchGroupedLocalSessions).toHaveBeenCalledTimes(1);

      // The top-level pager navigates the filtered result.
      fireEvent.click(screen.getByRole('button', { name: 'next' }));
      expect(
        screen.getByText(`page ${testCase.expectedFilteredPages} of ${testCase.expectedFilteredPages}`),
      ).toBeInTheDocument();
    });
  }

  it('falls back to sessions-channel grouping with stats unavailable while the fetch loads', () => {
    api.fetchProjectSummaries.mockReturnValue(pending());
    channelData = {
      sessions: [
        makeSession({ id: 's1', project: 'alpha-project', startTime: '2026-06-03T09:00:00Z' }),
        makeSession({ id: 's2', project: 'alpha-project', startTime: '2026-06-02T09:00:00Z' }),
        makeSession({ id: 's3', project: 'beta-project', projectHash: BETA_HASH }),
      ],
    };
    render(<HomePage />);

    const alpha = screen.getByRole('link', { name: 'Open the sessions of alpha-project' });
    expect(alpha).toHaveAttribute('href', `/sessions/${ALPHA_HASH}`);
    // Coverage + unmerged-branch counts are summary-only: while the fetch is in
    // flight they SHIMMER in place rather than showing "—" then popping to a
    // value. Last-work comes from the sessions channel, so it's real text.
    expect(alpha.querySelectorAll('[data-slot="skeleton"]').length).toBeGreaterThan(0);
    expect(alpha.textContent).toMatch(/ago/);
  });

  it('mounts pending statistic placeholders without invalid nesting or hydration diagnostics', () => {
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});
    channelData = {
      sessions: [
        makeSession({ id: 's1', project: 'alpha-project' }),
        makeSession({ id: 's2', project: 'beta-project', projectHash: BETA_HASH }),
      ],
    };
    render(<HomePage />);
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

    expect(
      await screen.findByRole('link', { name: 'Open the sessions of alpha-project' }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole('link', { name: 'Open the sessions of beta-project' }),
    ).toBeInTheDocument();
  });

  it('does not reveal stale websocket rows after REST reports a saved-selection failure', async () => {
    channelData = { sessions: [makeSession({ id: 'hidden', project: 'hidden-project' })] };
    api.fetchProjectSummaries.mockRejectedValue(new DiscoveryRequestError(
      '/api/v1/projects/summary',
      500,
      'project discovery failed while applying saved selection',
      'selection_visibility',
    ));
    render(<HomePage />);

    expect(await screen.findByText(/peasant kickstart/)).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: 'Open the sessions of hidden-project' })).not.toBeInTheDocument();
    expect(screen.queryByText(/AI conversation, on your machine/)).not.toBeInTheDocument();
  });

  for (const testCase of selectionRetryFixture.cases) {
    it(testCase.name, async () => {
      channelData = { sessions: [makeSession({ id: 'hidden', project: testCase.hiddenProject })] };
      const retry = deferred<DecodedProjectSummariesPayload>();
      api.fetchProjectSummaries
        .mockRejectedValueOnce(new DiscoveryRequestError(
          '/api/v1/projects/summary',
          500,
          'project discovery failed while applying saved selection',
          'selection_visibility',
        ))
        .mockReturnValueOnce(retry.promise);
      render(<HomePage />);

      expect(await screen.findByText(/peasant kickstart/)).toBeInTheDocument();
      fireEvent.click(screen.getByRole('button', { name: /retry project discovery/i }));

      expect(api.fetchProjectSummaries).toHaveBeenCalledTimes(2);
      expect(screen.getByText(/peasant kickstart/)).toBeInTheDocument();
      expect(screen.queryByRole('link', { name: `Open the sessions of ${testCase.hiddenProject}` })).not.toBeInTheDocument();
      expect(screen.queryByText(/AI conversation, on your machine/)).not.toBeInTheDocument();

      if (testCase.retryOutcome === 'success') {
        retry.resolve(makeSummaries(
          testCase.replacementProjects.map((project) =>
            makeSummary({ ...project, projectHash: newProjectHash(project.projectHash) }),
          ),
        ));
        for (const project of testCase.replacementProjects) {
          expect(await screen.findByRole('link', { name: `Open the sessions of ${project.project}` })).toBeInTheDocument();
        }
        expect(screen.queryByRole('link', { name: `Open the sessions of ${testCase.hiddenProject}` })).not.toBeInTheDocument();
      } else {
        retry.reject(new Error('database unavailable'));
        await waitFor(() => expect(screen.getByText(/database unavailable/)).toBeInTheDocument());
        expect(screen.getByRole('button', { name: /retry project discovery/i })).toBeInTheDocument();
        expect(screen.queryByRole('link', { name: `Open the sessions of ${testCase.hiddenProject}` })).not.toBeInTheDocument();
        expect(screen.queryByText(/AI conversation, on your machine/)).not.toBeInTheDocument();
      }
    });
  }

  it('fails closed on a saved-selection policy error and recovers by refetching when it clears', async () => {
    channelData = { sessions: [makeSession({ id: 'hidden', project: 'hidden-project' })] };
    channelError = new Error('selection policy failed');
    channelErrorCode = 'selection_visibility';
    api.fetchProjectSummaries.mockResolvedValue(makeSummaries([makeSummary({ project: 'hidden-project' })]));
    const view = render(<HomePage />);

    expect(await screen.findByText(/peasant kickstart/)).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: 'Open the sessions of hidden-project' })).not.toBeInTheDocument();

    channelError = null;
    channelErrorCode = undefined;
    channelData = { sessions: [makeSession({ id: 'visible', project: 'alpha-project' })] };
    api.fetchProjectSummaries.mockResolvedValue(makeSummaries([makeSummary({ project: 'alpha-project' })]));
    api.fetchReviewChanges.mockResolvedValue(REVIEW_LIST_PAYLOAD);
    view.rerender(<HomePage />);
    expect(await screen.findByRole('link', { name: 'Open the line of work "feat/graph-cache"' })).toBeInTheDocument();
  });

  it('skips the picker on single-project installs and embeds the Changes list', async () => {
    channelData = { sessions: [makeSession({ id: 's1', project: 'alpha-project' })] };
    api.fetchProjectSummaries.mockResolvedValue(makeSummaries([makeSummary({ project: 'alpha-project' })]));
    api.fetchReviewChanges.mockResolvedValue(REVIEW_LIST_PAYLOAD);
    render(<HomePage />);

    // No picker — straight to the project's changes (real ChangeList rows).
    const row = await screen.findByRole('link', { name: 'Open the line of work "feat/graph-cache"' });
    expect(row).toHaveAttribute(
      'href',
      `/review/${ALPHA_HASH}?branch=feat%2Fgraph-cache`,
    );
    expect(screen.queryByRole('link', { name: /Open the sessions of/ })).not.toBeInTheDocument();
    expect(api.fetchReviewChanges).toHaveBeenCalledWith(ALPHA_HASH);

    // The embedded list is the tour's changes-list anchor.
    expect(document.querySelector('[data-tour="changes-list"]')).not.toBeNull();
  });

  // A selected-mode project list without an explanation reads as broken rather
  // than filtered when most projects are hidden. These guard the fix: an active,
  // actually-hiding selection is called out plainly (with a
  // path to review/widen it, and WITHOUT naming the hidden projects), and an
  // active-but-not-hiding-anything selection stays silent.
  it('shows a selection notice when an active selection hides projects and sessions, on the single-project view', async () => {
    channelData = { sessions: [makeSession({ id: 's1', project: 'alpha-project' })] };
    api.fetchProjectSummaries.mockResolvedValue(
      makeSummaries([makeSummary({ project: 'alpha-project' })], {
        active: true,
        hiddenProjects: 2,
        hiddenSessions: 5,
      }),
    );
    api.fetchReviewChanges.mockResolvedValue(REVIEW_LIST_PAYLOAD);
    render(<HomePage />);

    const notice = await screen.findByRole('status');
    // EXACT match, not a substring/blacklist check: a blacklist of forbidden
    // literals (the previous version of this test) can always be defeated by
    // an identity string that just isn't on the list — this caught a real
    // vacuous-test finding where an unrelated project name was injected
    // straight into the rendered banner and every assertion still passed.
    // An exact match on the full rendered text fails on ANY extra content,
    // named or not, so there is nowhere for a leaked identity to hide.
    // Collapsed, the notice is one quiet line carrying the counts only.
    expect(notice.textContent).toBe('2 projects and 5 sessions hidden by a saved selection');

    // Expanding must not introduce an identity either — same exact-match
    // discipline applied to the disclosed body.
    fireEvent.click(within(notice).getByRole('button'));
    expect(notice.textContent).toBe(
      '2 projects and 5 sessions hidden by a saved selection'
      + 'A saved project selection is limiting what’s shown here. '
      + 'The data stays ingested and indexed — it is only hidden from this list.'
      + 'Run peasant kickstart to review or widen the selection.',
    );
  });

  it('shows a selection notice when an active selection hides data, on the multi-project picker view', async () => {
    channelData = {
      sessions: [
        makeSession({ id: 's1', project: 'alpha-project' }),
        makeSession({ id: 's2', project: 'beta-project', projectHash: BETA_HASH }),
      ],
    };
    api.fetchProjectSummaries.mockResolvedValue(
      makeSummaries(
        [makeSummary({ project: 'alpha-project' }), makeSummary({ project: 'beta-project', projectHash: BETA_HASH })],
        { active: true, hiddenProjects: 1, hiddenSessions: 1 },
      ),
    );
    render(<HomePage />);

    const notice = await screen.findByRole('status');
    // Same exact-match discipline as the single-project case above, with the
    // singular ("1 project"/"1 session", "is hidden") grammar branch.
    expect(notice.textContent).toBe('1 project and 1 session hidden by a saved selection');
  });

  it('shows no selection notice when the selection is inactive', async () => {
    channelData = { sessions: [makeSession({ id: 's1', project: 'alpha-project' })] };
    api.fetchProjectSummaries.mockResolvedValue(
      makeSummaries([makeSummary({ project: 'alpha-project' })], {
        active: false,
        hiddenProjects: 0,
        hiddenSessions: 0,
      }),
    );
    api.fetchReviewChanges.mockResolvedValue(REVIEW_LIST_PAYLOAD);
    render(<HomePage />);

    await screen.findByRole('link', { name: 'Open the line of work "feat/graph-cache"' });
    expect(screen.queryByRole('status')).not.toBeInTheDocument();
  });

  it('shows no selection notice when the selection is active but nothing is hidden', async () => {
    channelData = { sessions: [makeSession({ id: 's1', project: 'alpha-project' })] };
    api.fetchProjectSummaries.mockResolvedValue(
      makeSummaries([makeSummary({ project: 'alpha-project' })], {
        active: true,
        hiddenProjects: 0,
        hiddenSessions: 0,
      }),
    );
    api.fetchReviewChanges.mockResolvedValue(REVIEW_LIST_PAYLOAD);
    render(<HomePage />);

    await screen.findByRole('link', { name: 'Open the line of work "feat/graph-cache"' });
    expect(screen.queryByRole('status')).not.toBeInTheDocument();
  });

  it('resolves the single project hash from the sessions channel when summaries fail', async () => {
    api.fetchProjectSummaries.mockRejectedValue(new Error('boom'));
    api.fetchReviewChanges.mockResolvedValue(REVIEW_LIST_PAYLOAD);
    channelData = { sessions: [makeSession({ id: 's1', project: 'alpha-project' })] };
    render(<HomePage />);

    await screen.findByRole('link', { name: 'Open the line of work "feat/graph-cache"' });
    expect(api.fetchReviewChanges).toHaveBeenCalledWith(ALPHA_HASH);
  });

  it('shows the disconnected state when the local app is unreachable', async () => {
    // Unreachable = no WS data AND the summaries REST failed; a healthy-but-empty
    // install (REST ok) shows the teach state, not a disconnect message.
    channelConnected = false;
    channelData = undefined;
    api.fetchProjectSummaries.mockRejectedValue(new Error('down'));
    render(<HomePage />);
    // DataState disconnected panel copy (connection ≠ content principle).
    expect(await screen.findByText(/lost connection to the local program/i)).toBeInTheDocument();
  });
});
