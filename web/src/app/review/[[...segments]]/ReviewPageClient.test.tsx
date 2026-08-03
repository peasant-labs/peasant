import { describe, it, expect, vi, afterEach, beforeEach } from 'vitest';
import { render, screen, cleanup, within, fireEvent } from '@testing-library/react';
import { ReviewPageClient } from './ReviewPageClient';
import { ChangeDetail } from './ChangeDetail';
import type { ExplainerState } from '@/components/Explainer';
import type { SessionsPayload } from '@/types/messages';
import type { ReviewListPayload } from '@peasant-labs/schema';
import type { DecodedChangeDetailPayload } from '@/lib/api/map';
import {
  ALPHA_HASH,
  BETA_HASH,
  CHANGE_DETAIL_PAYLOAD,
  REVIEW_LIST_PAYLOAD,
  SESSION_A,
  SESSION_B,
  SESSION_C,
  makeSession,
  makeTask,
} from './test-fixtures';

// -- Mocks --------------------------------------------------------------------

// ChangeDetail uses next/navigation's useRouter for prev/next-change nav.
const routerPush = vi.hoisted(() => vi.fn());
vi.mock('next/navigation', () => ({ useRouter: () => ({ push: routerPush }) }));

// The sessions WS channel feeds project resolution.
let channelData: SessionsPayload | undefined;
let channelConnected = true;
vi.mock('@/contexts/WebSocketContext', () => ({
  useChannel: () => ({ data: channelData, connected: channelConnected, error: null }),
}));

// MapCanvas is mocked because the canvas library is outside this test boundary; the rest
// of @/components/map (edgeKey, …) stays real.
const canvasSpy = vi.hoisted(() => ({ props: null as Record<string, unknown> | null }));
vi.mock('@/components/map', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/components/map')>();
  return {
    ...actual,
    MapCanvas: (props: Record<string, unknown>) => {
      canvasSpy.props = props;
      return <div data-testid="map-canvas" aria-label={String(props.ariaLabel ?? '')} />;
    },
  };
});

function stubFetch(routes: Record<string, unknown>) {
  const mock = vi.fn((input: RequestInfo | URL) => {
    const url = String(input);
    for (const [fragment, payload] of Object.entries(routes)) {
      if (url.includes(fragment)) {
        return Promise.resolve({ ok: true, json: async () => payload } as Response);
      }
    }
    return Promise.resolve({
      ok: false,
      status: 404,
      text: async () => 'not found',
    } as Response);
  });
  vi.stubGlobal('fetch', mock);
  return mock;
}

const ALPHA_SESSIONS: SessionsPayload = {
  sessions: [
    makeSession({ id: 's1', project: 'alpha-project', projectHash: ALPHA_HASH }),
    makeSession({
      id: 's2',
      project: 'alpha-project',
      projectHash: ALPHA_HASH,
      startTime: '2026-06-02T09:00:00Z',
    }),
  ],
};

beforeEach(() => {
  channelData = ALPHA_SESSIONS;
  channelConnected = true;
  canvasSpy.props = null;
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  // The explainer persists its open/closed state per surface in localStorage;
  // clear it so one test opening the box doesn't leak into the next.
  try {
    window.localStorage.clear();
  } catch {
    /* localStorage unavailable — nothing to clear */
  }
});

// -- Project resolution -------------------------------------------------------

describe('ReviewPageClient — project resolution', () => {
  it('points back to Home (the project picker) when more than one project is recorded, without fetching or duplicating a picker', () => {
    channelData = {
      sessions: [
        makeSession({ id: 's1', project: 'alpha-project', projectHash: ALPHA_HASH }),
        makeSession({ id: 's2', project: 'beta-project', projectHash: BETA_HASH }),
      ],
    };
    const fetchMock = stubFetch({});
    render(<ReviewPageClient projectName={null} branch={null} />);

    expect(screen.getByText('Changes are listed per project.')).toBeInTheDocument();
    expect(
      screen.getByRole('link', { name: 'Choose a project from Home' }),
    ).toHaveAttribute('href', '/');
    // No in-page project picker rows: Home owns the picker.
    expect(screen.queryByRole('link', { name: /alpha-project/ })).not.toBeInTheDocument();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('uses the single project directly when only one is recorded', async () => {
    const fetchMock = stubFetch({
      [`/api/v1/review/${ALPHA_HASH}`]: REVIEW_LIST_PAYLOAD,
    });
    render(<ReviewPageClient projectName={null} branch={null} />);

    expect(await screen.findByText('feat/graph-cache')).toBeInTheDocument();
    expect(String(fetchMock.mock.calls[0][0])).toContain(`/api/v1/review/${ALPHA_HASH}`);
  });

  it('shows a plain note pointing Home when no projects are recorded', () => {
    channelData = { sessions: [] };
    stubFetch({});
    render(<ReviewPageClient projectName={null} branch={null} />);
    expect(screen.getByText('No recorded projects yet.')).toBeInTheDocument();
    expect(
      screen.getByRole('link', { name: 'Choose a project from Home' }),
    ).toHaveAttribute('href', '/');
  });

  it('titles the surface "changes" and breadcrumbs it as changes (lowercase chrome; the "Review" label retires)', async () => {
    stubFetch({ [`/api/v1/review/${ALPHA_HASH}`]: REVIEW_LIST_PAYLOAD });
    render(<ReviewPageClient projectName="alpha-project" branch={null} />);
    await screen.findByText('feat/graph-cache'); // flush the list fetch
    expect(screen.getByRole('heading', { level: 1, name: 'changes' })).toBeInTheDocument();
    const nav = screen.getByRole('navigation', { name: 'Breadcrumb' });
    expect(within(nav).getByText('changes')).toBeInTheDocument();
    expect(within(nav).queryByText('Review')).not.toBeInTheDocument();
  });

  it('reports an unresolvable project hash instead of fetching', () => {
    channelData = {
      sessions: [makeSession({ id: 's1', project: 'alpha-project', projectHash: undefined })],
    };
    const fetchMock = stubFetch({});
    render(<ReviewPageClient projectName="alpha-project" branch={null} />);
    expect(screen.getByText(/No project hash is recorded/)).toBeInTheDocument();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('shows the connection strip while disconnected', () => {
    channelConnected = false;
    channelData = undefined;
    stubFetch({});
    render(<ReviewPageClient projectName={null} branch={null} />);
    expect(screen.getByText(/Trying to reach the Peasant app/)).toBeInTheDocument();
  });
});

// -- The change graph ---------------------------------------------------------

describe('ReviewPageClient — change graph', () => {
  it('renders open changes as tip cards with facts, the violation glyph, and detail links', async () => {
    stubFetch({ [`/api/v1/review/${ALPHA_HASH}`]: REVIEW_LIST_PAYLOAD });
    render(<ReviewPageClient projectName="alpha-project" branch={null} />);

    const card = await screen.findByRole('link', { name: 'Open the line of work "feat/graph-cache"' });
    expect(card).toHaveAttribute(
      'href',
      `/review/${ALPHA_HASH}?branch=feat%2Fgraph-cache`,
    );
    expect(card).toHaveTextContent('9 new updates');
    expect(card).toHaveTextContent('14 files');
    expect(card).toHaveTextContent('+2 connections');
    expect(card).toHaveTextContent('last worked 2h ago');
    // Violation count carries the danger glyph (Lucide TriangleAlert).
    expect(within(card).getByLabelText(/1 rule break/)).toBeInTheDocument();

    // The anchorless branch keeps its facts and is flagged as undated in the TipCard.
    // The "started before this view" TailBand from the old hand-rolled SVG gutter is
    // not implemented in ft-ui CommitGraph (capability gap noted in ChangeGraph JSDoc).
    const quiet = screen.getByRole('link', { name: 'Open the line of work "fix/ingest-retry"' });
    expect(quiet).toHaveTextContent('2 new updates · 3 files · 1 conversation · 6 requests');
    expect(within(quiet).getByText(/undated/)).toBeInTheDocument();
  });

  it('sits the explainer "?" next to the page title and reveals the box when opened', async () => {
    stubFetch({ [`/api/v1/review/${ALPHA_HASH}`]: REVIEW_LIST_PAYLOAD });
    render(<ReviewPageClient projectName="alpha-project" branch={null} />);
    await screen.findByText('feat/graph-cache'); // flush the list fetch

    // The toggle is grouped with the <h1> "Changes" title, not buried in the body.
    const heading = screen.getByRole('heading', { level: 1, name: 'changes' });
    const toggle = screen.getByRole('button', { name: 'what am I looking at?' });
    expect(heading.parentElement?.contains(toggle)).toBe(true);

    // Collapsed by default — the content box does not render.
    expect(screen.queryByRole('region', { name: 'Explanation' })).not.toBeInTheDocument();

    // Opening it from the title reveals the box, with the dot vocabulary folded in.
    fireEvent.click(toggle);
    const box = screen.getByRole('region', { name: 'Explanation' });
    expect(within(box).getByText(/filled/)).toBeInTheDocument();
    expect(within(box).getByText(/worked on in the last 3 days/)).toBeInTheDocument();
  });

  it('renders lane 0 from recentCommits and shows merged changes in the "Already merged in" section', async () => {
    stubFetch({ [`/api/v1/review/${ALPHA_HASH}`]: REVIEW_LIST_PAYLOAD });
    render(<ReviewPageClient projectName="alpha-project" branch={null} />);

    await screen.findByText('feat(web): land the graph gutter');
    // Default-branch commits are CommitGraph rows; dot vocabulary lives in the
    // on-demand explainer, not inline.
    expect(screen.getByText('feat(web): land the graph gutter')).toBeInTheDocument();
    expect(
      screen.queryByText(/filled.*square has a saved conversation behind it/),
    ).not.toBeInTheDocument();
    // Merged changes are consolidated in the "Already merged in" section
    // (CommitGraph's merged=true flag shows a "merged" chip in the row but
    // the Link chip with the branch name lives in the bottom section).
    const chip = screen.getByRole('link', { name: 'Open the folded-in line of work "feat/project-overview"' });
    expect(chip).toHaveTextContent('folded in · Project overview');
    expect(screen.getByText(/Already merged in/)).toBeInTheDocument();
  });

  it('omits merged chips when the payload has no merged entries', async () => {
    const payload: ReviewListPayload = {
      ...REVIEW_LIST_PAYLOAD,
      changes: REVIEW_LIST_PAYLOAD.changes.filter((c) => !c.merged),
    };
    stubFetch({ [`/api/v1/review/${ALPHA_HASH}`]: payload });
    render(<ReviewPageClient projectName="alpha-project" branch={null} />);

    await screen.findByText('feat(web): land the graph gutter');
    expect(screen.queryByText(/^folded in/)).not.toBeInTheDocument();
  });

  it('states plainly when the project is not a git repository, pointing back to the Map', async () => {
    const payload: ReviewListPayload = {
      ...REVIEW_LIST_PAYLOAD,
      repoFound: false,
      defaultBranch: undefined,
      changes: [],
    };
    stubFetch({ [`/api/v1/review/${ALPHA_HASH}`]: payload });
    render(<ReviewPageClient projectName="alpha-project" branch={null} />);

    expect(
      await screen.findByText(/doesn.t keep a change history we can read/),
    ).toBeInTheDocument();
    expect(
      screen.getByRole('link', { name: 'Open the map of alpha-project' }),
    ).toHaveAttribute('href', `/map/${ALPHA_HASH}`);
  });

  it('states the no-branches empty state with the default branch name', async () => {
    const payload: ReviewListPayload = { ...REVIEW_LIST_PAYLOAD, changes: [] };
    stubFetch({ [`/api/v1/review/${ALPHA_HASH}`]: payload });
    render(<ReviewPageClient projectName="alpha-project" branch={null} />);

    expect(await screen.findByText(/No separate lines of work right now/)).toBeInTheDocument();
    expect(screen.getByText('develop')).toBeInTheDocument();
    expect(screen.getByText(/Open the Map to see that activity/)).toBeInTheDocument();
  });

  it('shows a fetch-error pill when the list request fails', async () => {
    stubFetch({}); // every route 404s
    render(<ReviewPageClient projectName="alpha-project" branch={null} />);
    expect(await screen.findByText(/Couldn't load changes/)).toBeInTheDocument();
  });
});

// -- Change detail ------------------------------------------------------------

describe('ReviewPageClient — change detail (integration)', () => {
  it('fetches the change and renders caption, files, work, structure, footnotes, and boundary line', async () => {
    const fetchMock = stubFetch({ '/change?branch=': CHANGE_DETAIL_PAYLOAD });
    render(<ReviewPageClient projectName="alpha-project" branch="feat/graph-cache" />);

    // File-first: the changed-file list leads.
    expect(await screen.findByLabelText('Files changed')).toBeInTheDocument();
    expect(String(fetchMock.mock.calls[0][0])).toContain(
      `/api/v1/review/${ALPHA_HASH}/change?branch=feat%2Fgraph-cache`,
    );

    // Caption fragments are interactive evidence anchors.
    expect(
      screen.getByRole('button', { name: '+2 connections (1 rule break)' }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole('button', {
        name: '14 files in internal/ingest, internal/api',
      }),
    ).toBeInTheDocument();

    // The structure graph is demoted to a disclosure — expand it for the slice,
    // which still receives the delta props (NEW node + new edges).
    expect(screen.queryByTestId('map-canvas')).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Expand the structure impact view' }));
    expect(screen.getByTestId('map-canvas')).toBeInTheDocument();
    expect(canvasSpy.props?.nodeDeltas).toEqual({ 'internal/cache': 'new' });
    expect(canvasSpy.props?.violations).toEqual(CHANGE_DETAIL_PAYLOAD.violations);

    // Boundary line with no verdict language.
    expect(
      screen.getByText(
        'Shows what changed and the recorded work behind it — not whether the change is correct or secure.',
      ),
    ).toBeInTheDocument();
  });
});

// -- ChangeDetail unit renders (work rail, footnotes, exits) --------------------

/** The explainer state lives in the page title now; the detail view receives
 *  it as a prop. Collapsed by default — the box never crowds the detail. */
function makeExplainer(open = false): ExplainerState {
  return { id: 'change-detail', open, hydrated: true, show: () => {}, hide: () => {} };
}

function renderDetail(payload: DecodedChangeDetailPayload = CHANGE_DETAIL_PAYLOAD) {
  return render(
    <ChangeDetail
      projectName="alpha-project"
      projectHash={ALPHA_HASH}
      branch="feat/graph-cache"
      payload={payload}
      explainer={makeExplainer()}
    />,
  );
}

describe('ChangeDetail — branch switcher', () => {
  beforeEach(() => routerPush.mockClear());

  it('offers a select over every open line of work and routes to the chosen one', () => {
    render(
      <ChangeDetail
        projectName="alpha-project"
        projectHash={ALPHA_HASH}
        branch="feat/graph-cache"
        payload={CHANGE_DETAIL_PAYLOAD}
        navBranches={['feat/earlier', 'feat/graph-cache', 'feat/later']}
        explainer={makeExplainer()}
      />,
    );
    const select = screen.getByRole('combobox', { name: 'Switch to another line of work' });
    // Every open branch is an option; the current one is selected.
    expect(within(select).getByRole('option', { name: 'feat/earlier' })).toBeInTheDocument();
    expect(within(select).getByRole('option', { name: 'feat/later' })).toBeInTheDocument();
    expect((select as HTMLSelectElement).value).toBe('feat/graph-cache');

    fireEvent.change(select, { target: { value: 'feat/later' } });
    expect(routerPush).toHaveBeenCalledWith('/review/alpha-project?branch=feat%2Flater');
  });

  it('includes the current branch as an option even when it is not in the open-branch list', () => {
    render(
      <ChangeDetail
        projectName="alpha-project"
        projectHash={ALPHA_HASH}
        branch="feat/merged-already"
        payload={CHANGE_DETAIL_PAYLOAD}
        navBranches={['feat/earlier', 'feat/later']}
        explainer={makeExplainer()}
      />,
    );
    const select = screen.getByRole('combobox', { name: 'Switch to another line of work' });
    expect((select as HTMLSelectElement).value).toBe('feat/merged-already');
    expect(
      within(select).getByRole('option', { name: 'feat/merged-already' }),
    ).toBeInTheDocument();
  });

  it('is omitted when there is only one (or no) other line of work to switch to', () => {
    render(
      <ChangeDetail
        projectName="alpha-project"
        projectHash={ALPHA_HASH}
        branch="feat/graph-cache"
        payload={CHANGE_DETAIL_PAYLOAD}
        navBranches={['feat/graph-cache']}
        explainer={makeExplainer()}
      />,
    );
    expect(
      screen.queryByRole('combobox', { name: 'Switch to another line of work' }),
    ).not.toBeInTheDocument();
  });
});

describe('ChangeDetail — what is unusual (4.4)', () => {
  it('renders a neutral rate-elevation line when present', () => {
    render(
      <ChangeDetail
        projectName="alpha-project"
        projectHash={ALPHA_HASH}
        branch="feat/graph-cache"
        payload={{
          ...CHANGE_DETAIL_PAYLOAD,
          unusual: [
            {
              kind: 'retryLoops',
              label: 'more retry loops per conversation than usual',
              perChange: 4.5,
              perProject: 2,
            },
          ],
        }}
        explainer={makeExplainer()}
      />,
    );
    const band = screen.getByLabelText('Change signals');
    expect(band).toHaveTextContent('more retry loops per conversation than usual');
    expect(band).toHaveTextContent('4.5');
    expect(band).toHaveTextContent('typical for this project');
  });
});

describe('ChangeDetail — recurring friction clusters (5.1)', () => {
  // Isolate the friction rows: clear the other signal sources (work-derived
  // retry loops, structural violations, rate-elevations) so the band reflects
  // only `frictions`.
  function renderFrictions(frictions: DecodedChangeDetailPayload['frictions']) {
    return render(
      <ChangeDetail
        projectName="alpha-project"
        projectHash={ALPHA_HASH}
        branch="feat/graph-cache"
        payload={{ ...CHANGE_DETAIL_PAYLOAD, work: [], violations: [], unusual: [], frictions }}
        explainer={makeExplainer()}
      />,
    );
  }

  it('renders a neutral per-file count (plural)', () => {
    renderFrictions([
      { kind: 'retryLoop', label: 'retry loops', file: 'internal/api/server.go', count: 3, sessions: 2 },
    ]);
    const band = screen.getByLabelText('Change signals');
    expect(band).toHaveTextContent('internal/api/server.go');
    expect(band).toHaveTextContent('retry loops');
    expect(band).toHaveTextContent('3 times');
    expect(band).toHaveTextContent('across 2 conversations');
    // Neutral, never a verdict: no "rule break"/danger framing on friction rows.
    expect(band).not.toHaveTextContent('rule break');
  });

  it('uses singular wording for a single occurrence / conversation', () => {
    renderFrictions([
      { kind: 'retryLoop', label: 'retry loops', file: 'internal/x.go', count: 1, sessions: 1 },
    ]);
    const band = screen.getByLabelText('Change signals');
    expect(band).toHaveTextContent('1 time across 1 conversation');
    expect(band).not.toHaveTextContent('1 times');
    expect(band).not.toHaveTextContent('1 conversations');
  });

  it('renders nothing when there is no friction (and no other signal)', () => {
    renderFrictions([]);
    expect(screen.queryByLabelText('Change signals')).not.toBeInTheDocument();
  });
});

describe('ChangeDetail — copy recap (5.5)', () => {
  it('copies a markdown recap of the change to the clipboard', async () => {
    const writeText = vi.fn((_text: string) => Promise.resolve());
    Object.assign(navigator, { clipboard: { writeText } });
    renderDetail();

    const btn = screen.getByRole('button', { name: /Copy a markdown recap/ });
    fireEvent.click(btn);

    expect(writeText).toHaveBeenCalledTimes(1);
    const md = writeText.mock.calls[0][0];
    expect(md.startsWith(`## ${CHANGE_DETAIL_PAYLOAD.branch}\n`)).toBe(true);
    expect(md).toContain('files changed');
    // The button confirms the copy.
    expect(await screen.findByText('Recap copied')).toBeInTheDocument();
  });
});

describe('ChangeDetail — file-first order', () => {
  it('lazily fetches and renders the real diff lines when a file row is expanded', async () => {
    stubFetch({
      '/diff': {
        branch: 'feat/graph-cache',
        file: 'x.go',
        status: 'M',
        binary: false,
        truncated: false,
        hunks: [
          {
            oldStart: 1,
            oldLines: 1,
            newStart: 1,
            newLines: 2,
            lines: [
              { kind: 'context', text: 'package x' },
              { kind: 'add', text: 'var addedLine = true' },
            ],
            // Attribution (mission climax): which conversation wrote this hunk.
            sessionId: 'sess-abcdef12',
            sessionTitle: 'Wire the cache layer',
          },
        ],
      },
    });
    renderDetail();

    // The path is the diff toggle; before clicking, no diff lines are shown.
    const toggles = screen.getAllByRole('button', { name: /Show the changed lines of/ });
    expect(toggles.length).toBeGreaterThan(0);
    expect(screen.queryByText('var addedLine = true')).not.toBeInTheDocument();

    fireEvent.click(toggles[0]);

    // The real changed line renders (the #1 mission gap — not a copyable command).
    expect(await screen.findByText('var addedLine = true')).toBeInTheDocument();
    // And the hunk is attributed to the conversation that wrote it (3.4).
    const attrib = await screen.findByText('Wire the cache layer');
    expect(attrib.closest('a')).toHaveAttribute(
      'href',
      expect.stringContaining('/projects/alpha-project/sess-abcdef12'),
    );
  });

  it('orders caption → changed files → the work → structure impact → footnotes, with no side rail', () => {
    renderDetail();
    const caption = screen.getByLabelText('Change caption');
    const files = screen.getByLabelText('Files changed');
    const work = screen.getByLabelText('The work');
    const structure = document.getElementById('review-structure')!;
    const footnotes = screen.getByLabelText('Totals');

    // The actual git files lead; the abstract structure graph reads last.
    const story = [caption, files, work, structure, footnotes];
    for (let i = 0; i + 1 < story.length; i++) {
      expect(
        story[i].compareDocumentPosition(story[i + 1]) & Node.DOCUMENT_POSITION_FOLLOWING,
        `expected story element ${i + 1} to follow element ${i}`,
      ).toBeTruthy();
    }

    // Every primary section is full-width — no side rail.
    expect(document.querySelector('aside')).toBeNull();

    // Caption drills still land: every evidence anchor id resolves.
    expect(document.getElementById('review-work')).toBe(work);
    expect(document.getElementById('review-files')).toBe(files);
    expect(document.getElementById('review-structure')).toBe(structure);
  });
});

describe('ChangeDetail — the work section', () => {
  it('links task rows into the viewer with scoped query params', () => {
    renderDetail();
    // SESSION_A's 13 tasks collapse to 3 — expand to reach the last row.
    fireEvent.click(screen.getByRole('button', { name: 'Show all 13 requests' }));
    const link = screen.getByRole('link', {
      name: 'Open request "tests are failing" in the transcript',
    });
    const url = new URL(link.getAttribute('href')!, 'http://localhost');
    expect(url.pathname).toBe(`/projects/alpha-project/${SESSION_A}`);
    expect(url.searchParams.get('scope')).toBe('task');
    expect(url.searchParams.get('scopeVal')).toBe('12');
    expect(url.searchParams.get('origin')).toBe('Review');
    expect(url.searchParams.get('originBranch')).toBe('feat/graph-cache');
    // The flagged request shows the plain "took several attempts" chip.
    expect(within(link).getByText('took several attempts')).toBeInTheDocument();
  });

  it('renders bound sessions as "linked" and candidates as "possibly related" (meaning on hover, not prose)', () => {
    renderDetail();
    // The plain-language binding tags render; their definition lives in the
    // tag's hover tooltip (Radix portal), not in a floating sentence.
    const blockA = screen.getByTestId(`work-session-${SESSION_A}`);
    expect(within(blockA).getByText('linked')).toBeInTheDocument();
    expect(screen.getAllByText('possibly related').length).toBeGreaterThanOrEqual(1);
    // The old floating "one-signal" prose is gone (it's in the tooltip now).
    expect(
      screen.queryByText('matched by only one signal — could be unrelated'),
    ).not.toBeInTheDocument();
    // The wire values never render as chip text.
    expect(screen.queryByText('bound')).not.toBeInTheDocument();
    expect(screen.queryByText('candidate')).not.toBeInTheDocument();
  });

  it('heads the work section tersely (the over-explanation is gone)', () => {
    renderDetail();
    const work = screen.getByLabelText('The work');
    expect(within(work).getByText('The conversations behind it')).toBeInTheDocument();
    expect(within(work).getByText(/click a request to read it/)).toBeInTheDocument();
    // The old multi-paragraph preamble is gone.
    expect(
      within(work).queryByText(/The AI conversations that produced this work\./),
    ).not.toBeInTheDocument();
  });

  it('summarises each session with its touched modules and request/file totals', () => {
    renderDetail();
    // SESSION_A edited 2 distinct files, both under internal/ingest.
    const sessionA = screen.getByTestId(`work-session-${SESSION_A}`);
    expect(within(sessionA).getByText('internal/ingest')).toBeInTheDocument();
    expect(within(sessionA).getByText('13 requests · 2 files')).toBeInTheDocument();
    // SESSION_C edited nothing — the totals still state the fact, no module names.
    const sessionC = screen.getByTestId(`work-session-${SESSION_C}`);
    expect(within(sessionC).getByText('2 requests · 0 files')).toBeInTheDocument();
  });

  it('collapses sessions with more than 3 tasks behind a "Show all" expander, per session', () => {
    renderDetail();
    // SESSION_A (13 tasks) and SESSION_B (6 tasks) collapse; SESSION_C (2) does not.
    expect(screen.getByRole('button', { name: 'Show all 13 requests' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Show all 6 requests' })).toBeInTheDocument();
    expect(
      screen.queryByRole('link', { name: 'Open request "tests are failing" in the transcript' }),
    ).not.toBeInTheDocument();

    // Expanding one session leaves the other collapsed (per-session state).
    fireEvent.click(screen.getByRole('button', { name: 'Show all 13 requests' }));
    expect(
      screen.getByRole('link', { name: 'Open request "tests are failing" in the transcript' }),
    ).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Show all 13 requests' })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Show all 6 requests' })).toBeInTheDocument();
  });

  it('renders a task that repeats the session title as a positional request label instead of the prompt', () => {
    const sessionTitle =
      'rework the change detail surface so the slice leads and the work reads plainly';
    renderDetail({
      ...CHANGE_DETAIL_PAYLOAD,
      work: [
        {
          ...CHANGE_DETAIL_PAYLOAD.work[0],
          title: sessionTitle,
          tasks: [
            // Exact repeat — deduped.
            makeTask(SESSION_A, 0, { title: sessionTitle }),
            // Shared prefix ≥40 chars — deduped.
            makeTask(SESSION_A, 1, {
              title: `${sessionTitle} — and keep the footnotes byte-identical`,
            }),
            // A genuinely different prompt — kept verbatim.
            makeTask(SESSION_A, 2, { title: 'a different prompt entirely' }),
          ],
        },
      ],
    });
    expect(screen.getByText('Request #0 in this conversation')).toBeInTheDocument();
    expect(screen.getByText('Request #1 in this conversation')).toBeInTheDocument();
    expect(screen.getByText('a different prompt entirely')).toBeInTheDocument();
    // Deduped rows get a positional accessible name, not the prompt.
    expect(
      screen.getByRole('link', { name: 'Open request #0 in the transcript' }),
    ).toBeInTheDocument();
    // The session title renders once (the header), never re-quoted by task rows.
    expect(screen.getAllByText(sessionTitle)).toHaveLength(1);
  });

  it('keeps short near-matches verbatim — the prefix rule needs 40 shared characters', () => {
    renderDetail({
      ...CHANGE_DETAIL_PAYLOAD,
      work: [
        {
          ...CHANGE_DETAIL_PAYLOAD.work[0],
          title: 'add caching to ingest',
          tasks: [makeTask(SESSION_A, 0, { title: 'add caching to ingest, but faster' })],
        },
      ],
    });
    expect(screen.getByText('add caching to ingest, but faster')).toBeInTheDocument();
    expect(screen.queryByText('Request #0 in this conversation')).not.toBeInTheDocument();
  });

  it('lists unrecorded commits plainly', () => {
    renderDetail();
    expect(screen.getByText('abc1234')).toBeInTheDocument();
    expect(screen.getByText('bump deps')).toBeInTheDocument();
    expect(
      screen.getByText('Updates without a saved conversation'),
    ).toBeInTheDocument();
  });
});

// -- Work ↔ files linking (hover/focus lights the edited file rows) -------------

describe('ChangeDetail — work ↔ files linking', () => {
  /** One conversation, two tasks editing two of the actual changed FILES, so
   *  the conversation union and the per-task narrowing are distinguishable. */
  const TWO_TASK_PAYLOAD: DecodedChangeDetailPayload = {
    ...CHANGE_DETAIL_PAYLOAD,
    work: [
      {
        ...CHANGE_DETAIL_PAYLOAD.work[0],
        tasks: [
          makeTask(SESSION_A, 0, {
            title: 'first task',
            editedFiles: ['internal/ingest/file0.go'],
          }),
          makeTask(SESSION_A, 1, {
            title: 'second task',
            editedFiles: ['internal/api/server.go'],
          }),
        ],
      },
    ],
  };

  /** Full paths (from each row's path-span title) of the lit changed-file rows. */
  const highlightedPaths = () =>
    Array.from(document.querySelectorAll('li[data-highlighted="true"]'))
      .map((li) => li.querySelector('span.truncate')?.getAttribute('title'))
      .filter(Boolean)
      .sort();

  it('lights no file rows until a conversation is hovered', () => {
    renderDetail(TWO_TASK_PAYLOAD);
    expect(highlightedPaths()).toEqual([]);
  });

  it('lights the files a conversation edited while hovered, and clears on leave', () => {
    renderDetail(TWO_TASK_PAYLOAD);
    const block = screen.getByTestId(`work-session-${SESSION_A}`);

    fireEvent.mouseEnter(block);
    expect(highlightedPaths()).toEqual([
      'internal/api/server.go',
      'internal/ingest/file0.go',
    ]);

    fireEvent.mouseLeave(block);
    expect(highlightedPaths()).toEqual([]);
  });

  it('narrows the lit files to one task row on hover and restores the conversation set on leave', () => {
    renderDetail(TWO_TASK_PAYLOAD);
    const block = screen.getByTestId(`work-session-${SESSION_A}`);
    const row = screen.getByRole('link', { name: 'Open request "second task" in the transcript' });

    fireEvent.mouseEnter(row);
    expect(highlightedPaths()).toEqual(['internal/api/server.go']);

    // Leaving the row toward the session block keeps the pointer inside the
    // session (relatedTarget models the real-browser destination).
    fireEvent.mouseLeave(row, { relatedTarget: block });
    expect(highlightedPaths()).toEqual([
      'internal/api/server.go',
      'internal/ingest/file0.go',
    ]);
  });

  it('lights files on keyboard focus of a task row and clears when focus leaves the conversation', () => {
    renderDetail(TWO_TASK_PAYLOAD);
    const row = screen.getByRole('link', { name: 'Open request "second task" in the transcript' });

    fireEvent.focus(row);
    expect(highlightedPaths()).toEqual(['internal/api/server.go']);

    fireEvent.blur(row);
    expect(highlightedPaths()).toEqual([]);
  });
});

describe('ChangeDetail — footnotes and exits', () => {
  it('renders files, line delta, output tokens, and cost', () => {
    renderDetail();
    const footnotes = screen.getByLabelText('Totals');
    expect(within(footnotes).getByText('14 files touched')).toBeInTheDocument();
    expect(within(footnotes).getByText('+612/−128 lines')).toBeInTheDocument();
    expect(within(footnotes).getByText(/412\.0K/)).toBeInTheDocument();
    expect(within(footnotes).getByText(/4\.10/)).toBeInTheDocument();
  });

  it('omits the line delta when both counts are zero, and cost when unknown', () => {
    renderDetail({
      ...CHANGE_DETAIL_PAYLOAD,
      linesAdded: 0,
      linesRemoved: 0,
      costUsd: undefined,
    });
    const footnotes = screen.getByLabelText('Totals');
    expect(within(footnotes).queryByText(/lines/)).not.toBeInTheDocument();
    expect(within(footnotes).queryByText(/spend/)).not.toBeInTheDocument();
    expect(within(footnotes).getByText(/412\.0K/)).toBeInTheDocument();
  });

  it('exits: open in Map, contribute the work conversations, and the literal diff command', () => {
    renderDetail();
    expect(
      screen.getByRole('link', { name: /See this work on the code map/ }),
    ).toHaveAttribute('href', '/map/alpha-project');
    // The evidence set carries bound AND candidate sessions — entry into the
    // wizard is filtered, not preselected, so the Choose step is the safety net.
    const contribute = screen.getByRole('link', {
      name: 'Contribute the 3 conversations behind this change',
    });
    expect(contribute).toHaveAttribute(
      'href',
      `/share?sessions=${SESSION_A},${SESSION_B},${SESSION_C}`,
    );
    expect(contribute).toHaveTextContent('Contribute 3 conversations');
    // The arrow is a Lucide glyph, never a literal character in the label.
    expect(contribute.textContent).not.toContain('→');
    expect(contribute.querySelector('svg')).not.toBeNull();
    // "view diff" is a copyable command, never a dead link.
    expect(screen.getByText('git diff develop...feat/graph-cache')).toBeInTheDocument();
    expect(
      screen.getByRole('button', { name: 'Copy the git diff command' }),
    ).toBeInTheDocument();
  });

  it('keeps the Contribute exit when every session is a candidate', () => {
    renderDetail({
      ...CHANGE_DETAIL_PAYLOAD,
      work: CHANGE_DETAIL_PAYLOAD.work.map((ws) => ({
        ...ws,
        binding: 'candidate' as const,
      })),
    });
    expect(
      screen.getByRole('link', { name: 'Contribute the 3 conversations behind this change' }),
    ).toHaveAttribute('href', `/share?sessions=${SESSION_A},${SESSION_B},${SESSION_C}`);
  });

  it('omits the Contribute exit when there are no sessions at all', () => {
    renderDetail({ ...CHANGE_DETAIL_PAYLOAD, work: [] });
    expect(screen.queryByRole('link', { name: /Contribute .* conversations/ })).not.toBeInTheDocument();
  });
});

describe('ChangeDetail — changed files (file-first)', () => {
  it('leads with the git files grouped by module, peeking the conversation that touched each in place', () => {
    renderDetail();
    // File-first: the list is the lead — always visible, no collapse toggle.
    expect(
      screen.queryByRole('button', { name: /the changed file list/ }),
    ).not.toBeInTheDocument();

    // Scope to the file list: leaf names also appear as treemap tile labels (5.3).
    const files = screen.getByLabelText('Files changed');

    // Module group headers + a file shown by its path relative to its group.
    expect(within(files).getByText('internal/api')).toBeInTheDocument();
    expect(within(files).getByText('server.go')).toBeInTheDocument();

    // Renames show old → new (full paths); status reads as a plain word now.
    expect(
      within(files).getByText('internal/api/handlers.go → internal/api/handler.go'),
    ).toBeInTheDocument();
    expect(within(files).getAllByText('renamed').length).toBeGreaterThanOrEqual(1);

    // The conversations cell is an inline peek that stays on the page instead
    // of navigating away. It is expanded by default, so SESSION_A's
    // conversation (with a request link scoped into the viewer) is shown without
    // a click, and the toggle now reads as "Hide …".
    const fileRow = within(files).getByText('file0.go').closest('li')!;
    expect(
      within(fileRow).getByRole('button', {
        name: /Hide the .* that touched internal\/ingest\/file0\.go/,
      }),
    ).toBeInTheDocument();
    const convLink = within(fileRow).getAllByRole('link')[0];
    const url = new URL(convLink.getAttribute('href')!, 'http://localhost');
    expect(url.pathname).toBe(`/projects/alpha-project/${SESSION_A}`);
    expect(url.searchParams.get('scope')).toBe('task');
  });

  it('shows the conversation peek expanded by default, and the toggle collapses it', () => {
    renderDetail();
    const files = screen.getByLabelText('Files changed');
    const fileRow = within(files).getByText('file0.go').closest('li')!;

    // Expanded by default: the conversation's full-transcript link is on
    // the page from the start, no click needed.
    const read = within(fileRow).getByRole('link', { name: /Read transcript/ });
    const url = new URL(read.getAttribute('href')!, 'http://localhost');
    expect(url.pathname).toBe(`/projects/alpha-project/${SESSION_A}`);
    expect(url.searchParams.get('scope')).toBe('change');

    // The toggle still works — clicking it collapses the peek.
    fireEvent.click(
      within(fileRow).getByRole('button', {
        name: /Hide the .* that touched internal\/ingest\/file0\.go/,
      }),
    );
    expect(within(fileRow).queryByText('Read transcript')).not.toBeInTheDocument();
  });
});
