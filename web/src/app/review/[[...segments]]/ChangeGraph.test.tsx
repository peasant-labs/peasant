import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, cleanup, within, fireEvent } from '@testing-library/react';
import { ChangeGraph } from './ChangeGraph';
import type { ExplainerState } from '@/components/Explainer';
import type { ReviewListPayload } from '@peasant-labs/schema';
import {
  ALPHA_HASH,
  REVIEW_LIST_PAYLOAD,
  makeChange,
  makeCommitRef,
} from './test-fixtures';

// ChangeGraph uses useRouter (for CommitGraph's onSelect → router.push).
const routerPush = vi.fn();
vi.mock('next/navigation', () => ({ useRouter: () => ({ push: routerPush }) }));

afterEach(() => {
  cleanup();
  routerPush.mockClear();
});

/** The explainer state now lives in the page title (ReviewPageClient); the
 *  graph only receives it as a prop. `open` controls whether the on-demand
 *  "What am I looking at?" content box renders. */
function makeExplainer(open = false): ExplainerState {
  return { id: 'changes', open, hydrated: true, show: () => {}, hide: () => {} };
}

function renderGraph(
  payload: ReviewListPayload = REVIEW_LIST_PAYLOAD,
  explainer: ExplainerState = makeExplainer(),
) {
  return render(
    <ChangeGraph projectHash={ALPHA_HASH} projectName="alpha-project" payload={payload} explainer={explainer} />,
  );
}

// -- CommitGraph dots (lane 0 commits + tip rows) -----------------------------------

describe('ChangeGraph — CommitGraph dots', () => {
  it('draws .cg-dot squares: filled (cg-dot-filled) when a session exists, hollow (cg-dot-hollow) otherwise', () => {
    // REVIEW_LIST_PAYLOAD produces 5 CommitGraph rows:
    //   0: tip fix/ingest-retry  (session=true, sessionCount=1)
    //   1: COMMIT_NEWEST         (hasSession=true)
    //   2: tip feat/graph-cache  (session=true, sessionCount=3)
    //   3: COMMIT_MID            (hasSession=false)
    //   4: COMMIT_OLDEST         (hasSession=true)
    const { container } = renderGraph();
    const dots = container.querySelectorAll('.cg-dot');
    expect(dots).toHaveLength(5);
    expect(dots[0].className).toContain('cg-dot-filled'); // fix/ingest-retry tip: has sessions
    expect(dots[1].className).toContain('cg-dot-filled'); // COMMIT_NEWEST: hasSession
    expect(dots[2].className).toContain('cg-dot-filled'); // feat/graph-cache tip: has sessions
    expect(dots[3].className).toContain('cg-dot-hollow'); // COMMIT_MID: no session
    expect(dots[4].className).toContain('cg-dot-filled'); // COMMIT_OLDEST: hasSession
  });

  it('renders truncated subjects with a relative time on the right', () => {
    renderGraph();
    // These commit messages are rendered in .cg-msg spans.
    expect(screen.getByText('feat(web): land the graph gutter')).toBeInTheDocument();
    expect(screen.getByText('chore: bump deps')).toBeInTheDocument();
    // COMMIT_NEWEST.timeMs = Date.now()-1h → formatRelative = "1h ago".
    expect(screen.getByText('1h ago')).toBeInTheDocument();
  });

  it('keeps the dot + freshness vocabulary in the on-demand explainer', () => {
    // Collapsed explainer (the default): the vocabulary is not shown.
    renderGraph();
    expect(screen.queryByText(/worked on in the last 3 days/)).not.toBeInTheDocument();
    expect(screen.queryByText(/paused — no work for 3 to 14 days/)).not.toBeInTheDocument();
    expect(screen.queryByText(/untouched for over 2 weeks/)).not.toBeInTheDocument();

    cleanup();

    // Opened: vocabulary is available on demand inside the explainer box.
    renderGraph(REVIEW_LIST_PAYLOAD, makeExplainer(true));
    const box = screen.getByRole('region', { name: 'Explanation' });
    expect(within(box).getByText(/filled/)).toBeInTheDocument();
    expect(within(box).getByText(/hollow/)).toBeInTheDocument();
    expect(within(box).getByText(/worked on in the last 3 days/)).toBeInTheDocument();
    expect(within(box).getByText(/paused — no work for 3 to 14/)).toBeInTheDocument();
    expect(within(box).getByText(/untouched for over 2/)).toBeInTheDocument();
    expect(within(box).getByText('develop')).toBeInTheDocument();
  });

  it('keeps every gutter stroke orthogonal — no curved SVG primitives in the lanes', () => {
    const { container } = renderGraph();
    // CommitGraph renders .cg-svg elements (one per row) with lines and g.cg-elbow only.
    const svgs = Array.from(container.querySelectorAll('.cg-svg'));
    expect(svgs.length).toBeGreaterThan(0);
    for (const svg of svgs) {
      // No path / circle / ellipse / rect — orthogonal lines and group elbows only.
      expect(svg.querySelectorAll('path, circle, ellipse, rect')).toHaveLength(0);
      for (const line of Array.from(svg.querySelectorAll('line'))) {
        const horizontal = line.getAttribute('y1') === line.getAttribute('y2');
        const vertical = line.getAttribute('x1') === line.getAttribute('x2');
        expect(horizontal || vertical, 'line must be axis-aligned').toBe(true);
      }
    }
  });
});

// -- Open tip cards ----------------------------------------------------------------

describe('ChangeGraph — open tip cards (preserved TipCard section)', () => {
  it('renders the branch, the compact fact line, the violation glyph, and the detail link', () => {
    renderGraph();
    const card = screen.getByRole('link', { name: 'Open the line of work "feat/graph-cache"' });
    expect(card).toHaveAttribute(
      'href',
      `/review/${ALPHA_HASH}?branch=feat%2Fgraph-cache`,
    );
    expect(card).toHaveTextContent(
      '9 new updates · 14 files · 3 conversations · 21 requests · +2 connections',
    );
    expect(card).toHaveTextContent('last worked 2h ago');
    const violation = within(card).getByLabelText(/1 rule break/);
    expect(violation.querySelector('svg')).not.toBeNull(); // Lucide TriangleAlert
  });

  it('omits zero fragments from the fact line', () => {
    renderGraph();
    const quiet = screen.getByRole('link', { name: 'Open the line of work "fix/ingest-retry"' });
    expect(quiet).toHaveTextContent('2 new updates · 3 files · 1 conversation · 6 requests');
    expect(quiet.textContent).not.toContain('connections');
    expect(quiet.textContent).not.toContain('last worked');
  });

  it('says "no changes" when every fact is zero', () => {
    renderGraph({
      ...REVIEW_LIST_PAYLOAD,
      changes: [
        makeChange('idle', {
          aheadCount: 0,
          filesChanged: 0,
          sessionCount: 0,
          taskCount: 0,
        }),
      ],
    });
    const card = screen.getByRole('link', { name: 'Open the line of work "idle"' });
    expect(within(card).getByText('no changes')).toBeInTheDocument();
  });

  it('flags undated tips in the TipCard', () => {
    renderGraph();
    const card = screen.getByRole('link', { name: 'Open the line of work "fix/ingest-retry"' });
    // TipCard renders "· undated" in the monospace branch line when tipCommitMs is absent.
    expect(within(card).getByText(/undated/)).toBeInTheDocument();
    // NOTE: the "started before this view" TailBand from the old hand-rolled gutter
    // is not implemented in CommitGraph — the branch tip still appears in the lane
    // without its base anchor, but no dashed tail is drawn. That is the capability
    // gap (TailBand dropped) documented in the ChangeGraph JSDoc.
  });
});

// -- Merged chips -------------------------------------------------------------------

describe('ChangeGraph — merged chips', () => {
  it('shows merged chip in "Already merged in" section linked to the detail', () => {
    renderGraph();
    const chip = screen.getByRole('link', { name: 'Open the folded-in line of work "feat/project-overview"' });
    expect(chip).toHaveTextContent('folded in · Project overview');
    expect(chip).toHaveTextContent('3d ago'); // mergedAtMs labels the chip
    expect(chip).toHaveAttribute(
      'href',
      `/review/${ALPHA_HASH}?branch=feat%2Fproject-overview`,
    );
    // Chip is in the "Already merged in" section (not inline at a commit row).
    expect(screen.getByText(/Already merged in/)).toBeInTheDocument();
    expect(
      screen.getByText(/Finished lines of work, now part of the main line/),
    ).toBeInTheDocument();
  });

  it('always puts merged chips in "Already merged in" even when the merge commit is in the window', () => {
    // REVIEW_LIST_PAYLOAD has feat/project-overview with mergeCommitHash=COMMIT_MID.hash.
    // Old code put it inline at the commit row. New code always consolidates in the section.
    renderGraph();
    // The CommitGraph commit row for COMMIT_MID has merged=true (the cg-merged chip
    // inside .cg-meta), but that is the CommitGraph's own chip — it says "merged" with
    // a GitMerge icon, not a Link. The branch-name chip is in "Already merged in".
    const section = screen.getByText(/Already merged in/).closest('div') as HTMLElement;
    expect(
      within(section).getByRole('link', { name: 'Open the folded-in line of work "feat/project-overview"' }),
    ).toBeInTheDocument();
  });

  it('still shows the section when the merge commit is unknown (no mergeCommitHash)', () => {
    renderGraph({
      ...REVIEW_LIST_PAYLOAD,
      changes: REVIEW_LIST_PAYLOAD.changes.map((c) =>
        c.merged ? { ...c, mergeCommitHash: undefined } : c,
      ),
    });
    expect(screen.getByText(/Already merged in/)).toBeInTheDocument();
    expect(
      screen.getByText(/Finished lines of work, now part of the main line/),
    ).toBeInTheDocument();
    expect(
      screen.getByRole('link', { name: 'Open the folded-in line of work "feat/project-overview"' }),
    ).toHaveTextContent('folded in · Project overview');
  });
});

// -- Show older ---------------------------------------------------------------------

describe('ChangeGraph — show older', () => {
  const MANY: ReviewListPayload = {
    projectHash: ALPHA_HASH,
    repoFound: true,
    defaultBranch: 'develop',
    changes: [makeChange('feat/long-running')],
    recentCommits: Array.from({ length: 60 }, (_, i) => makeCommitRef(i)),
    sessions: [],
    rewrittenCommits: [],
  };

  it('caps the initial window and expands to the full payload on click', () => {
    // MANY has 1 undated open change + 60 commits; window caps at 50 commits.
    // CommitGraph renders .cg-band per row: 1 tip + 50 commits = 51 rows.
    const { container } = renderGraph(MANY);
    expect(container.querySelectorAll('.cg-band')).toHaveLength(51);

    // CommitGraph's "show older" button uses lowercase text.
    const button = screen.getByRole('button', { name: 'show older' });
    fireEvent.click(button);

    // All 60 commits become visible: 1 tip + 60 commits = 61 rows.
    expect(container.querySelectorAll('.cg-band')).toHaveLength(61);
    expect(screen.queryByRole('button', { name: 'show older' })).not.toBeInTheDocument();
  });

  it('renders no expander when the payload fits the window', () => {
    renderGraph();
    expect(screen.queryByRole('button', { name: 'show older' })).not.toBeInTheDocument();
  });
});

// -- Keyboard order -------------------------------------------------------------------

describe('ChangeGraph — keyboard order', () => {
  it('TipCards appear before CommitGraph, merged chips appear at the bottom', () => {
    // TipCards use <Link> (→ role="link"); CommitGraph rows use <button>; MergedChips use <Link>.
    // The TipCard section renders openChanges in payload.changes order (feat/graph-cache first,
    // fix/ingest-retry second). Merged chips render after the CommitGraph.
    renderGraph();
    const names = screen
      .getAllByRole('link')
      .map((l) => l.getAttribute('aria-label'));
    expect(names).toEqual([
      'Open the line of work "feat/graph-cache"',           // TipCard (payload order)
      'Open the line of work "fix/ingest-retry"',           // TipCard (payload order)
      'Open the folded-in line of work "feat/project-overview"', // merged chip (bottom)
    ]);
  });
});

// -- Empty states (kept verbatim from the original) -----------------------------------

describe('ChangeGraph — empty states', () => {
  it('states plainly when the project is not a git repository, pointing at the Map', () => {
    renderGraph({
      ...REVIEW_LIST_PAYLOAD,
      repoFound: false,
      defaultBranch: undefined,
      changes: [],
      recentCommits: [],
    });
    expect(
      screen.getByText(/doesn.t keep a change history we can read/),
    ).toBeInTheDocument();
    expect(screen.getByText(/recorded AI conversations are still available/)).toBeInTheDocument();
  });

  it('states the no-branches empty state with the default branch name', () => {
    renderGraph({ ...REVIEW_LIST_PAYLOAD, changes: [] });
    expect(screen.getByText(/No separate lines of work right now/)).toBeInTheDocument();
    expect(screen.getByText('develop')).toBeInTheDocument();
    expect(screen.getByText(/Open the Map to see that activity/)).toBeInTheDocument();
  });
});
