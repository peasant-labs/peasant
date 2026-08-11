import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import type { ComponentProps } from 'react';
import { render, screen, cleanup, fireEvent, waitFor, act } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import type { CodeMapComposition } from '@peasant-labs/fairtrade/graph';
import type { SessionsPayload } from '@/types/messages';
import { MapPageClient, MapShell } from './MapPageClient';
import { UNASSIGNED_PROJECT } from '../lib/mapData';
import {
  GRAPH,
  GRAPH_NO_PARSE,
  NODE_DETAIL,
  OTHER_PROJECT,
  OTHER_PROJECT_HASH,
  PROJECT,
  PROJECT_HASH,
  REVIEW,
  SESSIONS,
  SUMMARIES,
  TASKS,
  makeTask,
  makeSession,
} from '../lib/test-fixtures';
import { graphAdapterContractFixture } from '@/test/fixtures/graphAdapterContract';
import { localReviewClarityFixture } from '@/test/fixtures/localReviewClarity';

// ---------------------------------------------------------------------------
// Mocks: the sessions WS channel + the shared composition boundary. This file
// is a page-wiring test only; the packaged composition is covered separately.
// ---------------------------------------------------------------------------

const ws = vi.hoisted(() => ({
  data: undefined as SessionsPayload | undefined,
  connected: true,
  error: null as string | null,
  errorCode: undefined as 'selection_visibility' | undefined,
}));

vi.mock('@/contexts/WebSocketContext', () => ({
  useChannel: () => ({ data: ws.data, connected: ws.connected, error: ws.error, errorCode: ws.errorCode }),
}));

type CodeMapProps = ComponentProps<typeof CodeMapComposition>;

const codeMap = vi.hoisted(() => ({ props: [] as unknown[] }));

vi.mock('@peasant-labs/fairtrade/graph', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@peasant-labs/fairtrade/graph')>();
  function CodeMapCompositionStub(props: CodeMapProps) {
    codeMap.props.push(props);
    const state = props.state ?? actual.createCodeMapState(props.defaultState);
    const publish = (next: typeof state) => props.onStateChange?.(next);
    return (
      <div>
        <button
          type="button"
          onClick={() => publish(actual.reduceCodeMapState(state, {
            type: 'set-presentation',
            presentation: 'navigator',
          }))}
        >
          back to browse
        </button>
        <button
          type="button"
          onClick={() => publish(actual.reduceCodeMapState(state, {
            type: 'set-presentation',
            presentation: 'canvas',
          }))}
        >
          spatial map
        </button>
        {state.presentation === 'canvas' && props.payload ? (
          <div
            data-testid="map-canvas"
            data-structure-edges={props.payload.structureEdges.length}
            data-activity-edges={0}
            data-violations={props.payload.violations.length}
            data-selected={state.selectedId ?? ''}
          >
            <button
              type="button"
              data-testid="canvas-select-node"
              onClick={() => publish(actual.reduceCodeMapState(state, {
                type: 'select',
                id: 'internal/ingest',
              }))}
            >
              select internal/ingest
            </button>
          </div>
        ) : props.canvasSlot}
        {props.rail}
      </div>
    );
  }
  return { ...actual, CodeMapComposition: CodeMapCompositionStub };
});

// ---------------------------------------------------------------------------
// Fetch stub — routes the REST calls onto fixture payloads.
// ---------------------------------------------------------------------------

type StubBody = unknown | 'pending' | 'error';

function respond(body: StubBody): Promise<Response> {
  if (body === 'pending') return new Promise<Response>(() => {});
  if (body === 'error') {
    return Promise.resolve({
      ok: false,
      status: 500,
      text: async () => 'boom',
    } as Response);
  }
  return Promise.resolve({ ok: true, json: async () => body } as Response);
}

function stubFetch(
  overrides: Partial<{
    summary: StubBody;
    graph: StubBody;
    review: StubBody;
    node: StubBody;
    tasks: StubBody;
    resolution: StubBody;
  }> = {},
) {
  const fetchMock = vi.fn((input: RequestInfo | URL) => {
    const url = String(input);
    if (url.includes('/projects/resolve?')) {
      if (overrides.resolution !== undefined) return respond(overrides.resolution);
      const project = new URL(url, 'http://localhost').searchParams.get('name');
      if (project === PROJECT) return respond({ project: PROJECT, projectHash: PROJECT_HASH });
      if (project === OTHER_PROJECT) {
        return respond({ project: OTHER_PROJECT, projectHash: OTHER_PROJECT_HASH });
      }
      return Promise.resolve({
        ok: false,
        status: 404,
        text: async () => 'project not found',
      } as Response);
    }
    if (url.includes('/projects/summary')) return respond(overrides.summary ?? SUMMARIES);
    if (url.includes('/node?')) return respond(overrides.node ?? NODE_DETAIL);
    if (url.includes('/tasks')) return respond(overrides.tasks ?? TASKS);
    if (url.includes('/api/v1/review/')) return respond(overrides.review ?? REVIEW);
    return respond(overrides.graph ?? GRAPH);
  });
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

/** Map/Review data calls (graph, node, tasks, review), excluding the project
 *  summary hash lookup that fires on every mount. */
function dataCalls(fetchMock: ReturnType<typeof vi.fn>): string[] {
  return fetchMock.mock.calls
    .map((c) => String(c[0]))
    .filter((u) => u.includes('/api/v1/map/') || u.includes('/api/v1/review/'));
}

function lastCodeMapProps(): CodeMapProps {
  expect(codeMap.props.length).toBeGreaterThan(0);
  return codeMap.props[codeMap.props.length - 1] as CodeMapProps;
}

describe('MapShell — one map without lenses', () => {
  beforeEach(() => {
    ws.data = { sessions: SESSIONS };
    ws.connected = true;
    ws.error = null;
  ws.errorCode = undefined;
    stubFetch();
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
    codeMap.props.length = 0;
    ws.data = undefined;
    ws.connected = true;
    ws.error = null;
    window.history.replaceState({}, '', '/');
  });

  // -- States -----------------------------------------------------------------

  it('does not re-resolve a canonical project inside the mounted shell', async () => {
    ws.data = { sessions: [] };
    stubFetch({ resolution: 'error' });
    render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} />);
    expect(await screen.findByTestId('map-canvas')).toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('renders an explicitly resolved hidden project without a visible session prerequisite', async () => {
    ws.data = { sessions: [] };
    render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} />);
    expect(await screen.findByTestId('map-canvas')).toBeInTheDocument();
    expect(screen.queryByText('No AI work recorded yet')).not.toBeInTheDocument();
  });

  it('keeps the mounted map available when live session updates fail', async () => {
    ws.error = 'selection conflict';
  ws.errorCode = 'selection_visibility';
    render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} />);
    expect(await screen.findByTestId('map-canvas')).toBeInTheDocument();
    expect(screen.getByText(/Live conversation updates are unavailable/)).toBeInTheDocument();
  });

  it('shows the shimmer (never a blank screen) while the graph loads', async () => {
    stubFetch({ graph: 'pending', review: 'pending' });
    render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} />);
    // findBy flushes the summary fetch inside act(); graph stays pending so
    // the shimmer persists.
    expect(await screen.findByText(/Building the structure map/)).toBeInTheDocument();
    expect(screen.queryByTestId('map-canvas')).not.toBeInTheDocument();
  });

  it('maps from the summary hash even when sessions lack the project hash', async () => {
    // The session carries no hash. The summary endpoint carries it, so the map
    // still loads without widening that session into the project.
    ws.data = {
      sessions: [makeSession({ id: 's1', projectHash: undefined })],
    };
    render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} />);
    expect(await screen.findByTestId('map-canvas')).toBeInTheDocument();
    expect(screen.queryByText(/needs a newer version of the Peasant app/)).not.toBeInTheDocument();
    // A hash-less liveness row is not widened into the canonical project.
    expect(screen.queryByRole('link', { name: 'Open the conversation s1' })).not.toBeInTheDocument();
  });

  it('keeps canonical graph data independent when session rows lack a hash', async () => {
    ws.data = {
      sessions: [makeSession({ id: 's1', projectHash: undefined })],
    };
    const fetchMock = stubFetch({ resolution: 'error' });
    render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} />);
    expect(await screen.findByTestId('map-canvas')).toBeInTheDocument();
    expect(screen.queryByText(/needs a newer version/)).not.toBeInTheDocument();
    expect(dataCalls(fetchMock).length).toBeGreaterThan(0);
  });

  it('does not widen project-less session rows into a canonical map', async () => {
    ws.data = {
      sessions: [makeSession({ id: 's-un', project: undefined, projectHash: undefined })],
    };
    stubFetch();
    render(<MapShell projectHash={PROJECT_HASH} projectName={UNASSIGNED_PROJECT} />);
    expect(await screen.findByTestId('map-canvas')).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: 'Open the conversation s-un' })).not.toBeInTheDocument();
  });

  it('cold disconnected: keeps the REST graph and explains missing liveness', async () => {
    ws.connected = false;
    ws.data = undefined;
    render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} />);
    // findBy flushes the summary fetch inside act() before asserting.
    expect(await screen.findByText(/connection lost; showing the last loaded data/i)).toBeInTheDocument();
    expect(await screen.findByTestId('map-canvas')).toBeInTheDocument();
  });

  it('keeps the canvas and adds a stale-data line while disconnected', async () => {
    ws.connected = false;
    render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} />);
    expect(await screen.findByTestId('map-canvas')).toBeInTheDocument();
    expect(screen.getByText(/connection lost; showing the last loaded data/)).toBeInTheDocument();
  });

  it('notes the no-parse fallback and still renders the recorded-activity nodes', async () => {
    stubFetch({ graph: GRAPH_NO_PARSE });
    render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} />);
    const node = await screen.findByTestId('map-canvas');
    expect(
      screen.getByText(/Structure parsing is not yet available for this project’s languages/),
    ).toBeInTheDocument();
    // One view, no lens fallback: nothing to draw as edges, nodes still render.
    expect(node).toHaveAttribute('data-structure-edges', '0');
    expect(node).toHaveAttribute('data-activity-edges', '0');
  });

  it('shows an error line when the graph call fails', async () => {
    stubFetch({ graph: 'error', review: 'error' });
    render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} />);
    expect(await screen.findByText('Could not load the map graph.')).toBeInTheDocument();
  });

  // -- One map view -------------------------------------------------------------

  it('one view: structure edges + violations on the canvas, never activity edges, no host toolbar knobs', async () => {
    render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} />);
    const node = await screen.findByTestId('map-canvas');
    expect(node).toHaveAttribute('data-structure-edges', '1');
    expect(node).toHaveAttribute('data-violations', '1');
    // The fixture graph HAS activity edges — they must not reach the canvas.
    expect(node).toHaveAttribute('data-activity-edges', '0');

    // The host route does not render its own map controls. The shared CodeMap
    // owns detail grain/search; these assertions prevent duplicate host knobs.
    expect(screen.queryByRole('button', { name: /lens$/i })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /Size nodes by/ })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Toggle effort overlay' })).not.toBeInTheDocument();
    expect(screen.queryByRole('combobox', { name: 'Search nodes' })).not.toBeInTheDocument();

    // Node width and coverage are derived inside CodeMap from the topology
    // payload. Activity coupling stays out of the canvas payload entirely.
    const payload = lastCodeMapProps().payload;
    expect(payload).toBeDefined();
    expect(payload!.nodes[0]).toHaveProperty('loc');
    expect(payload).not.toHaveProperty('activityEdges');
  });

  it('mounted MapPageClient forwards every non-default map field to the Fairtrade composition', async () => {
    const graph = {
      ...GRAPH,
      nodes: GRAPH.nodes.map((node) => (
        node.id === 'internal/ingest'
          ? { ...node, ...graphAdapterContractFixture.mapNode }
          : node
      )),
    };
    stubFetch({ graph });
    render(<MapPageClient projectName={PROJECT} />);

    await screen.findByRole('heading', { name: PROJECT });
    await waitFor(() => {
      const node = lastCodeMapProps().payload?.nodes.find(({ id }) => id === 'internal/ingest');
      expect(node).toEqual(expect.objectContaining(graphAdapterContractFixture.mapNode));
    });
  });

  // -- Grain control: open the map to connected hierarchy -----------------------

  it('opens the map to Folders grain so the hierarchy and its connections show', async () => {
    render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} />);
    await screen.findByTestId('map-canvas');

    // The canvas is driven by a CONTROLLED zoom — defaulting to package
    // ("Folders") grain, not the disconnected top-level Overview.
    expect(lastCodeMapProps().state?.grain).toBe('package');

    expect(screen.queryByRole('button', { name: 'Folders' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Overview' })).not.toBeInTheDocument();
  });

  it('CodeMap zoom callbacks switch the controlled canvas grain', async () => {
    render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} />);
    await screen.findByTestId('map-canvas');

    await act(async () => {
      const props = lastCodeMapProps();
      props.onStateChange?.({ ...props.state!, grain: 'project', expandedIds: [] });
    });
    expect(lastCodeMapProps().state?.grain).toBe('project');

    await act(async () => {
      const props = lastCodeMapProps();
      props.onStateChange?.({ ...props.state!, grain: 'file', expandedIds: ['internal'] });
    });
    expect(lastCodeMapProps().state?.grain).toBe('file');
    expect(lastCodeMapProps().state?.expandedIds).toEqual(['internal']);
  });

  // -- Rail: project panel ------------------------------------------------------

  it('unselected rail: coverage sentence, Recent AI conversations stories, full session list — no KPI tiles', async () => {
    render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} />);
    await screen.findByTestId('map-canvas');

    // Coverage summed from ROOT nodes: 8 of 12 — one plain sentence.
    const coverage = await screen.findAllByText(/files have a saved/);
    expect(coverage[0].textContent).toContain('8');
    expect(coverage[0].textContent).toContain('12');

    // Recent AI conversations: the project's recorded tasks, title + relative time, each
    // linking into the task-scoped viewer with origin=Map (no node context).
    expect(screen.getAllByText('Recent AI conversations').length).toBeGreaterThan(0);
    expect((await screen.findAllByText('fix ingest retry loop'))[0]).toBeInTheDocument();
    expect(screen.getAllByText('tweak web styles')[0]).toBeInTheDocument();
    const taskLink = screen.getAllByRole('link', {
      name: 'Open task at turn 12 of session sess-new',
    })[0];
    expect(taskLink).toHaveAttribute(
      'href',
      `/projects/${PROJECT_HASH}/sess-new?turn=12&origin=Map`,
    );

    // The clarifying sub-line under the recent-work eyebrow.
    expect(
      screen.getAllByText(/latest recorded conversations; hover one to light up what it shaped\./)[0],
    ).toBeInTheDocument();

    // Each row carries a "touched" line — the first-two-segment modules of
    // its editedFiles — so the rail says WHAT a session shaped and WHERE.
    expect(taskLink.textContent).toContain('internal/ingest');
    const webTaskLink = screen.getAllByRole('link', {
      name: 'Open task at turn 4 of session sess-old',
    })[0];
    expect(webTaskLink.textContent).toContain('web/src');

    // Label chips are GONE from Recent AI conversations (the node panel keeps them) —
    // TASK_INGEST carries the 'bug' label, which must not render here.
    expect(screen.queryByText('bug')).not.toBeInTheDocument();

    // The v2 KPI tiles are gone — their data lives on the home picker rows.
    expect(screen.queryByText('Last activity')).not.toBeInTheDocument();
    expect(screen.queryByText('Recorded')).not.toBeInTheDocument();
    expect(screen.queryByText("Totals")).not.toBeInTheDocument();

    // Reverse-chron session list — EVERY session, zero-touch included.
    const links = screen.getAllByRole('link', { name: /Open the conversation/ });
    expect(links[0]).toHaveAccessibleName('Open the conversation sess-new');
    expect(links[0]).toHaveAttribute('href', `/projects/${PROJECT_HASH}/sess-new?origin=Map`);
    expect(links[1]).toHaveAccessibleName('Open the conversation sess-old');
    // The other project's session does not appear.
    expect(screen.queryByRole('link', { name: 'Open the conversation sess-beta' })).not.toBeInTheDocument();
  });

  it('Recent AI conversations states a failed tasks call plainly', async () => {
    stubFetch({ tasks: 'error' });
    render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} />);
    await screen.findByTestId('map-canvas');
    expect((await screen.findAllByText('Could not load recent work.'))[0]).toBeInTheDocument();
  });

  it('mounted Map outcome rows expose the complete ingest-time heuristic help', async () => {
    ws.data = {
      sessions: localReviewClarityFixture.outcomeCases.map((testCase) => makeSession({
        id: testCase.sessionId,
        preview: testCase.taskTitle,
      })),
    };
    stubFetch({
      tasks: {
        projectHash: PROJECT_HASH,
        tasks: localReviewClarityFixture.outcomeCases.map((testCase) => makeTask({
          sessionId: testCase.sessionId,
          entryIndex: testCase.entryIndex,
          title: testCase.taskTitle,
          outcome: testCase.outcome,
          retryLoop: false,
          labels: [],
        })),
      },
    });
    render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} />);
    await screen.findByTestId('map-canvas');

    for (const testCase of localReviewClarityFixture.outcomeCases) {
      const row = (await screen.findAllByRole('link', {
        name: `Open task at turn ${testCase.entryIndex} of session ${testCase.sessionId}`,
      }))[0];
      expect(row).toHaveTextContent(testCase.mapLabel);
    }

    const help = (await screen.findAllByRole('button', {
      name: localReviewClarityFixture.copy.outcomeHelpName,
    }))[0];
    fireEvent.focus(help);
    const tooltip = await screen.findByRole('tooltip');
    expect(tooltip).toHaveTextContent(localReviewClarityFixture.copy.outcomeSource);
    for (const testCase of localReviewClarityFixture.outcomeCases) {
      expect(tooltip).toHaveTextContent(testCase.definition);
    }
    expect(tooltip).toHaveTextContent(localReviewClarityFixture.copy.outcomeLimit);
    expect(help).toHaveAttribute('aria-describedby', tooltip.id);
  });

  // -- Rail to canvas highlight relay -------------------------------------------

  it('hovering a Recent AI conversations row highlights the nodes it edited; leaving clears', async () => {
    const user = userEvent.setup();
    render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} />);
    await screen.findByTestId('map-canvas');

    const row = (
      await screen.findAllByRole('link', { name: 'Open task at turn 12 of session sess-new' })
    )[0];

    // Hover lifts the task's editedFiles into the canvas highlight set (the
    // canvas maps file-grain ids to visible ancestors via liftIdsToVisible).
    await user.hover(row);
    expect(Array.from(lastCodeMapProps().highlightedIds ?? [])).toEqual([
      'internal/ingest/pipeline.go',
    ]);

    // Leaving clears it — nothing stays lit.
    await user.unhover(row);
    expect(lastCodeMapProps().highlightedIds).toBeUndefined();
  });

  it('keyboard parity: focusing a Recent AI conversations row highlights, blurring clears', async () => {
    render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} />);
    await screen.findByTestId('map-canvas');

    const row = (
      await screen.findAllByRole('link', { name: 'Open task at turn 4 of session sess-old' })
    )[0];

    fireEvent.focus(row);
    expect(Array.from(lastCodeMapProps().highlightedIds ?? [])).toEqual(['web/src/app/page.tsx']);

    fireEvent.blur(row);
    expect(lastCodeMapProps().highlightedIds).toBeUndefined();
  });

  it('hovering a Shaped-by row in the node panel relays the same highlight', async () => {
    const user = userEvent.setup();
    render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} />);
    await screen.findByTestId('map-canvas');

    await user.click(screen.getByTestId('canvas-select-node')); // internal/ingest
    const row = (
      await screen.findAllByRole('link', { name: 'Open task at turn 12 of session sess-new' })
    )[0];

    await user.hover(row);
    expect(Array.from(lastCodeMapProps().highlightedIds ?? [])).toEqual([
      'internal/ingest/pipeline.go',
    ]);
    await user.unhover(row);
    expect(lastCodeMapProps().highlightedIds).toBeUndefined();
  });

  // -- Rail: node panel ---------------------------------------------------------

  it('selecting a node loads its panel: shaped-by, commits, footnotes, contribute link', async () => {
    const user = userEvent.setup();
    render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} />);
    await screen.findByTestId('map-canvas');

    await user.click(screen.getByTestId('canvas-select-node'));

    // Identity + traceability from the node-detail payload.
    expect((await screen.findAllByText('internal/ingest'))[0]).toBeInTheDocument();
    const coverage = screen.getAllByText(/files have a saved/);
    expect(coverage[0].textContent).toContain('6');
    expect(coverage[0].textContent).toContain('7');

    // Task rows link to the task-scoped viewer with the exact query params.
    const taskLink = screen.getAllByRole('link', {
      name: 'Open task at turn 12 of session sess-new',
    })[0];
    expect(taskLink).toHaveAttribute(
      'href',
      `/projects/${PROJECT_HASH}/sess-new?turn=12&origin=Map&originNode=internal%2Fingest`,
    );
    expect(screen.getAllByText('fix ingest retry loop')[0]).toBeInTheDocument();
    // Label chips render as plain chips in the node panel; the project panel's
    // recent-conversation rows omit them.
    expect(screen.getAllByText('bug')[0]).toBeInTheDocument();
    // Shaped-by rows carry the same "touched" modules line as Recent AI conversations.
    expect(taskLink.textContent).toContain('internal/ingest');

    // Interleaved commits — the unrecorded one is marked plainly.
    expect(screen.getAllByText('manual hotfix')[0]).toBeInTheDocument();
    expect(screen.getAllByText('no AI conversation captured')[0]).toBeInTheDocument();

    // Footnotes: retry loops /"files re-edited"/ files / cost-when-known.
    expect(screen.getAllByText("times the AI retried")[0]).toBeInTheDocument();
    expect(screen.getAllByText("files re-edited")[0]).toBeInTheDocument();
    expect(screen.getAllByText('$4.10')[0]).toBeInTheDocument();

    // Contribute these sessions → /share?sessions=<distinct ids>.
    const contribute = screen.getAllByRole('link', {
      name: /Contribute the 2 sessions behind this node/,
    })[0];
    expect(contribute).toHaveAttribute('href', '/share?sessions=sess-new,sess-old');

    // Clearing the selection returns to the project panel.
    await user.click(screen.getAllByRole('button', { name: 'Clear node selection' })[0]);
    expect(screen.getAllByText("AI-built files").length).toBeGreaterThan(0);
    expect(screen.queryByText("times the AI retried")).not.toBeInTheDocument();
  });

  it('node panel: "Usually changed alongside" rows from the graph payload; clicking one selects that node', async () => {
    const user = userEvent.setup();
    render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} />);
    await screen.findByTestId('map-canvas');

    await user.click(screen.getByTestId('canvas-select-node')); // internal/ingest

    // The co-edit edge internal/ingest↔cmd (5 shared tasks) reads as a row.
    expect(screen.getAllByText("Usually changed alongside").length).toBeGreaterThan(0);
    const row = (await screen.findAllByRole('button', { name: 'Select the code area cmd' }))[0];
    expect(row.textContent).toContain('cmd');
    expect(row.textContent).toContain('5 shared tasks');

    // Clicking the row selects that node on the canvas…
    await user.click(row);
    expect(screen.getByTestId('map-canvas')).toHaveAttribute('data-selected', 'cmd');
    // …whose own coupling rows derive from the same payload (both edges touch cmd).
    expect(
      (await screen.findAllByRole('button', { name: 'Select the code area internal/ingest' }))[0],
    ).toBeInTheDocument();
    expect(screen.getAllByRole('button', { name: 'Select the code area internal' })[0]).toBeInTheDocument();
  });

  it('omits "Usually changed alongside" when no co-edit edge touches the node', async () => {
    stubFetch({
      graph: { ...GRAPH, activityEdges: [{ from: 'cmd', to: 'internal', taskCount: 3 }] },
    });
    const user = userEvent.setup();
    render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} />);
    await screen.findByTestId('map-canvas');

    await user.click(screen.getByTestId('canvas-select-node')); // internal/ingest
    await screen.findAllByText("times the AI retried");
    expect(screen.queryByText("Usually changed alongside")).not.toBeInTheDocument();
  });

  it('labels each rail panel with a mini-header naming what it is', async () => {
    const user = userEvent.setup();
    render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} />);
    await screen.findByTestId('map-canvas');

    // Unselected: the project panel header carries a "Project" eyebrow.
    expect(screen.getAllByText('Project').length).toBeGreaterThan(0);

    // Selected: the node panel header carries a "Code area" eyebrow + the
    // node's kind + lines in the quiet meta slot (NODE_DETAIL: a 'package').
    await user.click(screen.getByTestId('canvas-select-node')); // internal/ingest
    expect((await screen.findAllByText('Code area'))[0]).toBeInTheDocument();
    expect(
      screen.getAllByText(
        (_content, el) => el?.textContent === 'package · 1,200 lines',
      ).length,
    ).toBeGreaterThan(0);
  });

  it('node panel: "What this area connects to" lists depends-on / used-by; clicking selects that area (5.6)', async () => {
    const user = userEvent.setup();
    render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} />);
    await screen.findByTestId('map-canvas');

    await user.click(screen.getByTestId('canvas-select-node')); // internal/ingest → NODE_DETAIL

    expect(screen.getAllByText('What this area connects to').length).toBeGreaterThan(0);
    expect(screen.getAllByText('Depends on').length).toBeGreaterThan(0);
    expect(screen.getAllByText('Used by').length).toBeGreaterThan(0);

    // A depends-on row (NODE_DETAIL.dependsOn) selects that area on the canvas.
    const dep = (
      await screen.findAllByRole('button', { name: 'Select the code area internal/store' })
    )[0];
    await user.click(dep);
    expect(screen.getByTestId('map-canvas')).toHaveAttribute('data-selected', 'internal/store');
  });

  it('omits "What this area connects to" when the node has no parsed connections (5.6)', async () => {
    stubFetch({ node: { ...NODE_DETAIL, dependsOn: [], usedBy: [] } });
    const user = userEvent.setup();
    render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} />);
    await screen.findByTestId('map-canvas');

    await user.click(screen.getByTestId('canvas-select-node'));
    await screen.findAllByText('times the AI retried'); // detail loaded
    expect(screen.queryByText('What this area connects to')).not.toBeInTheDocument();
  });

  it('states the dark-matter case plainly (no AI conversation captureds)', async () => {
    stubFetch({
      node: { ...NODE_DETAIL, sessionCount: 0, shapedBy: [], recentCommits: [] },
    });
    const user = userEvent.setup();
    render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} />);
    await screen.findByTestId('map-canvas');

    await user.click(screen.getByTestId('canvas-select-node'));
    expect(
      (await screen.findAllByText('No recorded conversations touch this code.'))[0],
    ).toBeInTheDocument();
  });

  // -- Deep link (/map/{project}?node=) -------------------------------------------

  it('preselects the ?node= deep link and opens its panel', async () => {
    const user = userEvent.setup();
    window.history.replaceState({}, '', `/map/${PROJECT_HASH}?node=internal%2Fingest`);
    render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} />);

    await user.click(await screen.findByRole('button', { name: 'spatial map' }));
    const node = await screen.findByTestId('map-canvas');
    await waitFor(() => expect(node).toHaveAttribute('data-selected', 'internal/ingest'));
    expect((await screen.findAllByText("times the AI retried"))[0]).toBeInTheDocument();
  });

  // -- Time strip --------------------------------------------------------------

  it('renders open-branch chips (merged excluded) and navigates to Review on click', async () => {
    window.history.replaceState(
      {},
      '',
      `/map/${PROJECT_HASH}?node=internal%2Fingest&mode=navigator&grain=file&expand=internal&filter=ingest&focus=internal%2Fingest&scale=1.4&panX=12&panY=-8`,
    );
    const assignMock = vi.fn();
    const originalLocation = window.location;
    Object.defineProperty(window, 'location', {
      configurable: true,
      writable: true,
      value: {
        ...originalLocation,
        pathname: originalLocation.pathname,
        search: originalLocation.search,
        assign: assignMock,
      },
    });
    try {
      const user = userEvent.setup();
      render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} />);

      const chip = await screen.findByRole('button', { name: 'Review branch feat/graph-cache' });
      expect(
        screen.queryByRole('button', { name: 'Review branch feat/already-merged' }),
      ).not.toBeInTheDocument();
      // The default branch renders as a quiet label.
      expect(screen.getByText('develop')).toBeInTheDocument();

      await user.click(chip);
      expect(assignMock).toHaveBeenCalledWith(
        `/review/${PROJECT_HASH}?branch=feat%2Fgraph-cache&returnTo=v1.%2Fmap%2F${PROJECT_HASH}%3Fnode%3Dinternal%252Fingest%26mode%3Dnavigator%26grain%3Dfile%26expand%3Dinternal%26filter%3Dingest%26focus%3Dinternal%252Fingest%26scale%3D1.4%26panX%3D12%26panY%3D-8`,
      );
    } finally {
      Object.defineProperty(window, 'location', {
        configurable: true,
        writable: true,
        value: originalLocation,
      });
    }
  });

  it('shows the ledger line only with showLedger (the /map/{project} header strip)', async () => {
    window.history.replaceState({}, '', `/map/${PROJECT_HASH}?grain=file&scale=1.4&panX=12&panY=0`);
    render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} showLedger />);
    expect(
      await screen.findByText(/AI conversations?, on your machine\. Nothing has left it\./),
    ).toBeInTheDocument();
    const canonicalMapLocation = `${window.location.pathname}${window.location.search}`;
    expect((await screen.findAllByRole('link', { name: 'Open the conversation sess-new' }))[0])
      .toHaveAttribute(
        'href',
    `/projects/${PROJECT_HASH}/sess-new?origin=Map&returnTo=${encodeURIComponent(`v1.${canonicalMapLocation}`)}`,
      );
  });

  it('preserves the complete mounted Map location for whole-session, project-task, and node-task viewer exits', async () => {
  const mountedURL = `/map/${PROJECT_HASH}?mode=navigator&grain=file&expand=internal&filter=ingest&focus=internal%2Fingest&scale=1.4&panX=12&panY=-8`;
  window.history.replaceState({}, '', mountedURL);
  const user = userEvent.setup();
  render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} showLedger />);
  const canonicalMapLocation = `${window.location.pathname}${window.location.search}`;
  const returnTo = encodeURIComponent(canonicalMapLocation);
  expect((await screen.findAllByRole('link', { name: 'Open the conversation sess-new' }))[0]).toHaveAttribute(
    'href',
    `/projects/${PROJECT_HASH}/sess-new?origin=Map&returnTo=${encodeURIComponent(`v1.${canonicalMapLocation}`)}`,
  );
  expect((await screen.findAllByRole('link', { name: 'Open task at turn 12 of session sess-new' }))[0]).toHaveAttribute(
    'href',
    `/projects/${PROJECT_HASH}/sess-new?turn=12&origin=Map&returnTo=${encodeURIComponent(`v1.${canonicalMapLocation}`)}`,
  );
  await user.click(screen.getByRole('button', { name: 'spatial map' }));
  await user.click(screen.getByTestId('canvas-select-node'));
  const selectedMapLocation = encodeURIComponent(`${window.location.pathname}${window.location.search}`);
  expect((await screen.findAllByRole('link', { name: 'Open task at turn 12 of session sess-new' }))[0]).toHaveAttribute(
    'href',
    `/projects/${PROJECT_HASH}/sess-new?turn=12&origin=Map&originNode=internal%2Fingest&returnTo=${encodeURIComponent(`v1.${decodeURIComponent(selectedMapLocation)}`)}`,
  );
  });

  // -- Keyed shell (project switches stay in the same route segment) ------------------

  it('MapPageClient keys the shell by project: switching resets selection and refetches tasks', async () => {
    const user = userEvent.setup();
    ws.data = {
      sessions: [
        makeSession({ id: 'sess-a' }),
        makeSession({
          id: 'sess-b',
          project: OTHER_PROJECT,
          projectHash: OTHER_PROJECT_HASH,
        }),
      ],
    };
    const fetchMock = stubFetch();
    const tasksCalls = () =>
      fetchMock.mock.calls.map((c) => String(c[0])).filter((u) => u.includes('/tasks'));

    const { rerender } = render(<MapPageClient projectName={PROJECT} />);
    await user.click(await screen.findByRole('button', { name: 'spatial map' }));
    await screen.findByTestId('map-canvas');

    // Tasks fetch fires eagerly for project A (the Recent AI conversations block), and a
    // node gets selected.
    await waitFor(() => expect(tasksCalls()).toHaveLength(1));
    expect(tasksCalls()[0]).toContain(PROJECT_HASH);
    await user.click(screen.getByTestId('canvas-select-node'));
    expect(screen.getByTestId('map-canvas')).toHaveAttribute('data-selected', 'internal/ingest');

    // Same route segment, new project — the key remounts the shell:
    // selection cleared, tasks refetched for project B.
    rerender(<MapPageClient projectName={OTHER_PROJECT} />);
    await user.click(await screen.findByRole('button', { name: 'spatial map' }));
    expect(await screen.findByTestId('map-canvas')).toHaveAttribute('data-selected', '');
    await waitFor(() => expect(tasksCalls()).toHaveLength(2));
    expect(tasksCalls()[1]).toContain(OTHER_PROJECT_HASH);
  });
});
