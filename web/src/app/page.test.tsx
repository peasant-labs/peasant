import { describe, it, expect, vi, afterEach, beforeEach } from 'vitest';
import { render, screen, cleanup, fireEvent, waitFor } from '@testing-library/react';
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
  });

  afterEach(() => {
    cleanup();
    channelData = undefined;
    channelConnected = true;
    channelError = null;
    channelErrorCode = undefined;
    vi.clearAllMocks();
  });

  it('teaches the lifecycle when no sessions exist', async () => {
    channelData = { sessions: [] };
    api.fetchProjectSummaries.mockResolvedValue(makeSummaries([]));
    render(<HomePage />);
    // TeachingEmptyState renders lowercase chrome title + the copy-able command.
    expect(await screen.findByText('no ai work recorded yet')).toBeInTheDocument();
    expect(screen.getByText('peasant ingest')).toBeInTheDocument();
    // No ledger line without sessions.
    expect(screen.queryByText(/on your machine/)).not.toBeInTheDocument();
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

    // Rows link into /review/{project} — the Changes list, NOT the map.
    const alpha = await screen.findByRole('link', {
      name: 'Open the changes of alpha-project',
    });
    expect(alpha).toHaveAttribute('href', `/review/${ALPHA_HASH}`);
    const beta = screen.getByRole('link', { name: 'Open the changes of beta-project' });
    expect(beta).toHaveAttribute('href', `/review/${BETA_HASH}`);

    // Most recent work first.
    const links = screen.getAllByRole('link', { name: /Open the changes of/ });
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
    expect(await screen.findByText('projects')).toBeInTheDocument();
    expect(screen.getByText('files built with ai')).toBeInTheDocument();
    expect(screen.getByText('40%')).toBeInTheDocument();
    expect(screen.getByText('40 of 100 files')).toBeInTheDocument();
    // "unmerged branches" labels BOTH the stat tile and the picker column.
    expect(screen.getAllByText('unmerged branches').length).toBeGreaterThanOrEqual(1);
  });

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

    const alpha = screen.getByRole('link', { name: 'Open the changes of alpha-project' });
    expect(alpha).toHaveAttribute('href', `/review/${ALPHA_HASH}`);
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
      await screen.findByRole('link', { name: 'Open the changes of alpha-project' }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole('link', { name: 'Open the changes of beta-project' }),
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
    expect(screen.queryByRole('link', { name: 'Open the changes of hidden-project' })).not.toBeInTheDocument();
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
      expect(screen.queryByRole('link', { name: `Open the changes of ${testCase.hiddenProject}` })).not.toBeInTheDocument();
      expect(screen.queryByText(/AI conversation, on your machine/)).not.toBeInTheDocument();

      if (testCase.retryOutcome === 'success') {
        retry.resolve(makeSummaries(
          testCase.replacementProjects.map((project) =>
            makeSummary({ ...project, projectHash: newProjectHash(project.projectHash) }),
          ),
        ));
        for (const project of testCase.replacementProjects) {
          expect(await screen.findByRole('link', { name: `Open the changes of ${project.project}` })).toBeInTheDocument();
        }
        expect(screen.queryByRole('link', { name: `Open the changes of ${testCase.hiddenProject}` })).not.toBeInTheDocument();
      } else {
        retry.reject(new Error('database unavailable'));
        await waitFor(() => expect(screen.getByText(/database unavailable/)).toBeInTheDocument());
        expect(screen.getByRole('button', { name: /retry project discovery/i })).toBeInTheDocument();
        expect(screen.queryByRole('link', { name: `Open the changes of ${testCase.hiddenProject}` })).not.toBeInTheDocument();
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
    expect(screen.queryByRole('link', { name: 'Open the changes of hidden-project' })).not.toBeInTheDocument();

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
    expect(screen.queryByRole('link', { name: /Open the changes of/ })).not.toBeInTheDocument();
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
    expect(notice.textContent).toBe(
      'A saved project selection is limiting what’s shown here: 2 projects and 5 sessions are hidden from this list.'
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
    expect(notice.textContent).toBe(
      'A saved project selection is limiting what’s shown here: 1 project and 1 session are hidden from this list.'
      + 'Run peasant kickstart to review or widen the selection.',
    );
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
