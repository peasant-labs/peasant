import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, cleanup, fireEvent, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { isCodeMapViewportScale } from '@peasant-labs/fairtrade/graph';
import {
  parseStrictYAML,
  requireExactFields,
  requireRecord,
  requireUniqueNames,
} from '@/test/strictYaml';
import type { Harness } from '@peasant-labs/schema';
import type { SessionsPayload } from '@/types/messages';
import { RouteOrigin, transcriptHref, type ProjectHash } from '@/lib/navigation/projectRoutes';
import { MapPageClient, MapShell } from './MapPageClient';
import {
  assertCommitRowLink,
  loadCommitRowManifest,
  proveCommitRowMutation,
} from './commitRowFixtures';
import {
  GRAPH_WITH_FILE,
  makeSession,
  NODE_DETAIL,
  PROJECT,
  PROJECT_HASH,
  REVIEW,
  SESSIONS,
  SUMMARIES,
  TASKS,
} from '../lib/test-fixtures';

const navigatorRouteSource = readFileSync(
  resolve(process.cwd(), 'src/app/map/[[...segments]]/testdata/navigator_route.yaml'),
  'utf8',
);
const mountedSessionManifestSource = readFileSync(
  resolve(process.cwd(), 'src/app/map/[[...segments]]/testdata/mounted_commit_row_sessions.manifest.yaml'),
  'utf8',
);
const mountedSessionCasesSource = readFileSync(
  resolve(process.cwd(), 'src/app/map/[[...segments]]/testdata/mounted_commit_row_sessions.yaml'),
  'utf8',
);
const mountedSessionManifest = loadCommitRowManifest(
  mountedSessionManifestSource,
  'mounted commit-row session fixtures manifest',
);

type MountedSessionCase = {
  name: string;
  selectedNode: string;
  commit: {
    hash: string;
    subject: string;
    timeMs: number;
    sessionIds: string[];
  };
  liveSessions: Array<{
    id: string;
    harness: Harness;
    preview: string;
    project: string;
    projectHash: ProjectHash;
  }>;
  expectedRows: Array<{
    sessionId: string;
    expectedName: string;
    expectedHarness: string | null;
  }>;
};

function loadMountedSessionCases(source: string = mountedSessionCasesSource): { cases: MountedSessionCase[] } {
  const root = requireRecord(
    parseStrictYAML(source, 'mounted commit-row session fixtures'),
    'mounted commit-row session fixtures',
  );
  requireExactFields(root, ['cases'], 'mounted commit-row session fixtures');
  if (!Array.isArray(root.cases)) {
    throw new Error('mounted commit-row session fixtures.cases must be an array');
  }

  const rows = root.cases.map((value, index) => {
    const row = requireRecord(value, `mounted commit-row session fixtures.cases[${index}]`);
    requireExactFields(
      row,
      ['name', 'selectedNode', 'commit', 'liveSessions', 'expectedRows'],
      `mounted commit-row session fixtures.cases[${index}]`,
    );
    const commit = requireRecord(row.commit, `mounted commit-row session fixtures.cases[${index}].commit`);
    requireExactFields(
      commit,
      ['hash', 'subject', 'timeMs', 'sessionIds'],
      `mounted commit-row session fixtures.cases[${index}].commit`,
    );
    if (!Array.isArray(commit.sessionIds) || !Array.isArray(row.liveSessions) || !Array.isArray(row.expectedRows)) {
      throw new Error(`mounted commit-row session fixtures.cases[${index}] requires array fields`);
    }
    row.liveSessions.forEach((session, sessionIndex) => {
      const sessionRow = requireRecord(
        session,
        `mounted commit-row session fixtures.cases[${index}].liveSessions[${sessionIndex}]`,
      );
      requireExactFields(
        sessionRow,
        ['id', 'harness', 'preview', 'project', 'projectHash'],
        `mounted commit-row session fixtures.cases[${index}].liveSessions[${sessionIndex}]`,
      );
    });
    row.expectedRows.forEach((expected, expectedIndex) => {
      const expectedRow = requireRecord(
        expected,
        `mounted commit-row session fixtures.cases[${index}].expectedRows[${expectedIndex}]`,
      );
      requireExactFields(
        expectedRow,
        ['sessionId', 'expectedName', 'expectedHarness'],
        `mounted commit-row session fixtures.cases[${index}].expectedRows[${expectedIndex}]`,
      );
    });
    return row;
  });
  requireUniqueNames(rows, 'mounted commit-row session fixtures.cases');

  const names = rows.map((row) => row.name);
  if (
    rows.length !== mountedSessionManifest.expectedCount ||
    JSON.stringify(names) !== JSON.stringify(mountedSessionManifest.requiredNames)
  ) {
    throw new Error('mounted commit-row session fixtures do not match their independent manifest');
  }
  return { cases: rows as unknown as MountedSessionCase[] };
}

const mountedSessionFixture = loadMountedSessionCases();
const mountedSessionCases = mountedSessionFixture.cases;

function findMountedCommitRowLink(
  row: HTMLElement,
  testCase: MountedSessionCase,
  sessionId: string,
): HTMLAnchorElement {
  const href = transcriptHref(PROJECT_HASH, sessionId, {
    origin: RouteOrigin.Map,
    originNode: testCase.selectedNode,
  });
  const link = within(row)
    .getAllByRole('link')
    .find((candidate) => candidate.getAttribute('href') === href);
  if (!link) {
    throw new Error(`mounted commit-row session fixtures case ${testCase.name} missing link for ${sessionId}`);
  }
  return link as HTMLAnchorElement;
}

async function renderMountedCommitRow(testCase: MountedSessionCase): Promise<HTMLElement> {
  const user = userEvent.setup();
  ws.data = {
    sessions: testCase.liveSessions.map((session) => makeSession(session)),
  };
  stubFetch({
    node: {
      ...NODE_DETAIL,
      path: testCase.selectedNode,
      recentCommits: [
        {
          ...testCase.commit,
          hasSession: testCase.commit.sessionIds.length > 0,
        },
      ],
    },
  });
  render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} />);
  await screen.findByRole('application', { name: `Code map of ${PROJECT}` });

  const selectedLeaf = testCase.selectedNode.split('/').filter(Boolean).at(-1) ?? testCase.selectedNode;
  await user.click(screen.getByRole('button', { name: new RegExp(`^${selectedLeaf}: folder`, 'i') }));

  const commitRow = (await screen.findAllByText(testCase.commit.subject))[0]?.closest('div');
  if (!commitRow) {
    throw new Error(`mounted commit-row session fixtures case ${testCase.name} failed to render the commit row`);
  }
  return commitRow as HTMLElement;
}
const navigatorRouteValue = requireRecord(
  parseStrictYAML(navigatorRouteSource, 'navigator route fixture'),
  'navigator route fixture',
);
requireExactFields(
  navigatorRouteValue,
  [
    'deep_link',
    'navigator_interaction',
    'canvas_interaction',
    'stale_selection',
    'invalid_viewport',
    'history',
  ],
  'navigator route fixture',
);
const navigatorRouteDeepLink = requireRecord(
  navigatorRouteValue.deep_link,
  'navigator route fixture.deep_link',
);
requireExactFields(
  navigatorRouteDeepLink,
  ['url', 'selectedNode', 'grain', 'filter', 'focus', 'viewport'],
  'navigator route fixture.deep_link',
);
const navigatorRouteViewport = requireRecord(
  navigatorRouteDeepLink.viewport,
  'navigator route fixture.deep_link.viewport',
);
requireExactFields(
  navigatorRouteViewport,
  ['scale', 'panX', 'panY'],
  'navigator route fixture.deep_link.viewport',
);
const navigatorInteraction = requireRecord(
  navigatorRouteValue.navigator_interaction,
  'navigator route fixture.navigator_interaction',
);
requireExactFields(
  navigatorInteraction,
  ['url', 'selectedNode', 'filter'],
  'navigator route fixture.navigator_interaction',
);
const canvasInteraction = requireRecord(
  navigatorRouteValue.canvas_interaction,
  'navigator route fixture.canvas_interaction',
);
requireExactFields(
  canvasInteraction,
  ['url', 'disclosureLabel'],
  'navigator route fixture.canvas_interaction',
);
const staleSelection = requireRecord(
  navigatorRouteValue.stale_selection,
  'navigator route fixture.stale_selection',
);
requireExactFields(staleSelection, ['url', 'selectedNode'], 'navigator route fixture.stale_selection');
const invalidViewport = requireRecord(
  navigatorRouteValue.invalid_viewport,
  'navigator route fixture.invalid_viewport',
);
requireExactFields(invalidViewport, ['url'], 'navigator route fixture.invalid_viewport');
const history = requireRecord(navigatorRouteValue.history, 'navigator route fixture.history');
requireExactFields(
  history,
  [
    'canonicalUrl',
    'noncanonicalUrl',
    'canonicalTarget',
    'malformedInitialUrl',
    'malformedPopstateUrl',
    'malformedTarget',
    'oracleMutations',
  ],
  'navigator route fixture.history',
);
for (const field of [
  'canonicalUrl',
  'noncanonicalUrl',
  'canonicalTarget',
  'malformedInitialUrl',
  'malformedPopstateUrl',
  'malformedTarget',
] as const) {
  if (typeof history[field] !== 'string' || history[field].length === 0) {
    throw new Error(`navigator route fixture.history.${field} must be a non-empty string`);
  }
}
if (!Array.isArray(history.oracleMutations)) {
  throw new Error('navigator route fixture.history.oracleMutations must be an array');
}
const historyOracleMutations = history.oracleMutations.map((value, index) => {
  const row = requireRecord(value, `navigator route fixture.history.oracleMutations[${index}]`);
  requireExactFields(
    row,
    ['name', 'writes'],
    `navigator route fixture.history.oracleMutations[${index}]`,
  );
  if (typeof row.name !== 'string' || row.name.length === 0
    || !Array.isArray(row.writes)
    || row.writes.some((write) => typeof write !== 'string' || write.length === 0)) {
    throw new Error('navigator route fixture history mutations require a name and string writes');
  }
  return row as { name: string; writes: string[] };
});
requireUniqueNames(historyOracleMutations, 'navigator route fixture.history.oracleMutations');
const navigatorRouteFixture = navigatorRouteValue as unknown as {
  deep_link: {
    url: string;
    selectedNode: string;
    grain: string;
    filter: string;
    focus: string;
    viewport: { scale: number; panX: number; panY: number };
  };
  navigator_interaction: { url: string; selectedNode: string; filter: string };
  canvas_interaction: { url: string; disclosureLabel: string };
  stale_selection: { url: string; selectedNode: string };
  invalid_viewport: { url: string };
  history: {
    canonicalUrl: string;
    noncanonicalUrl: string;
    canonicalTarget: string;
    malformedInitialUrl: string;
    malformedPopstateUrl: string;
    malformedTarget: string;
    oracleMutations: Array<{ name: string; writes: string[] }>;
  };
};

function historyWriteTargets(spy: ReturnType<typeof vi.spyOn>): string[] {
  return spy.mock.calls.map((call) => String(call[2]));
}

function isExactHistoryRepair(writes: readonly string[], target: string): boolean {
  return writes.length === 1 && writes[0] === target;
}

/**
 * MapShell.test.tsx mocks the shared CodeMap, so hover-relay, controlled
 * zoom/expand, and keyboard parity can regress there while its prop-plumbing
 * assertions stay green. This file renders the REAL production
 * `@peasant-labs/fairtrade/graph` CodeMap: the exact component end users
 * get, and drives it through actual DOM interaction (real clicks, real
 * hover/focus/blur events, real `userEvent.keyboard` key presses on the
 * canvas's own roving-focus surface). Only the WS channel and REST `fetch`
 * are mocked (the app's own I/O boundary); CodeMap/MapCanvas are never
 * mocked. Stubs stay for page wiring in MapShell.test.tsx; this file is the
 * real-composition/real-interaction proof.
 */

const ws = vi.hoisted(() => ({
  data: undefined as SessionsPayload | undefined,
  connected: true,
}));

vi.mock('@/contexts/WebSocketContext', () => ({
  useChannel: () => ({ data: ws.data, connected: ws.connected, error: null }),
}));

type StubBody = unknown | 'pending' | 'error';

function respond(body: StubBody): Promise<Response> {
  if (body === 'pending') return new Promise<Response>(() => {});
  return Promise.resolve({ ok: true, json: async () => body } as Response);
}

function stubFetch(
  overrides: Partial<{ summary: StubBody; graph: StubBody; review: StubBody; node: StubBody; tasks: StubBody }> = {},
) {
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes('/projects/resolve?')) return respond({ project: PROJECT, projectHash: PROJECT_HASH });
      if (url.includes('/projects/summary')) return respond(overrides.summary ?? SUMMARIES);
      if (url.includes('/node?')) return respond(overrides.node ?? NODE_DETAIL);
      if (url.includes('/tasks')) return respond(overrides.tasks ?? TASKS);
      if (url.includes('/api/v1/review/')) return respond(overrides.review ?? REVIEW);
      return respond(overrides.graph ?? GRAPH_WITH_FILE);
    }),
  );
}

describe('MapShell - REAL CodeMap composition + interaction', () => {
  beforeEach(() => {
    ws.data = { sessions: SESSIONS };
    ws.connected = true;
    stubFetch();
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
    ws.data = undefined;
    ws.connected = true;
    window.history.replaceState({}, '', '/');
  });

  it('mounts the REAL CodeMap/MapCanvas (not a stub) with its shared grain toolbar', async () => {
    render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} />);

    // the real MapCanvas application landmark + its own shared toolbar (grain
    // segmented control + node search): the SAME composition the fairtrade
    // demo mounts, not a peasant-only or mocked reimplementation.
    const app = await screen.findByRole('application', { name: `Code map of ${PROJECT}` });
    expect(app).toHaveClass('mc');
    expect(screen.getByRole('radiogroup', { name: 'detail grain' })).toBeInTheDocument();
    expect(screen.getAllByRole('radio')).toHaveLength(3);

    // Opens to Folders (package) grain by default: cmd (root, no
    // children) and internal/ingest (internal collapses into its one child).
    expect(screen.getByRole('button', { name: /cmd: folder/i })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /ingest: folder/i })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /pipeline\.go: file/i })).not.toBeInTheDocument();
  });

  it('controlled grain: files stay aggregated until the REAL folder disclosure expands them', async () => {
    const user = userEvent.setup();
    render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} />);
    await screen.findByRole('application', { name: `Code map of ${PROJECT}` });

    await user.click(screen.getByRole('radio', { name: 'files' }));

    expect(screen.getByRole('radio', { name: 'files' })).toHaveAttribute('aria-checked', 'true');
    expect(screen.queryByRole('button', { name: /pipeline\.go: file/i })).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: /show children for ingest/i }));
    expect(await screen.findByRole('button', { name: /pipeline\.go: file/i })).toBeInTheDocument();
  });

  for (const testCase of mountedSessionCases) {
    it(testCase.name, async () => {
      const commitRow = await renderMountedCommitRow(testCase);
      for (const expected of testCase.expectedRows) {
        const link = findMountedCommitRowLink(commitRow, testCase, expected.sessionId);
        expect(link).toHaveAttribute(
          'href',
          transcriptHref(PROJECT_HASH, expected.sessionId, {
            origin: RouteOrigin.Map,
            originNode: testCase.selectedNode,
          }),
        );
        assertCommitRowLink(link, expected);
      }
    });
  }

  for (const mutation of mountedSessionManifest.mutations) {
    it(mutation.name, async () => {
      await proveCommitRowMutation(
        mountedSessionCasesSource,
        mutation,
        loadMountedSessionCases,
        async (fixture, currentMutation) => {
          const testCase = fixture.cases.find(({ name }) => name === currentMutation.caseName);
          if (!testCase) {
            throw new Error(`mounted commit-row session fixtures mutation ${currentMutation.name} has no case named ${currentMutation.caseName}`);
          }
          const commitRow = await renderMountedCommitRow(testCase);
          return findMountedCommitRowLink(commitRow, testCase, currentMutation.sessionId);
        },
        (fixture, currentMutation) => {
          const testCase = fixture.cases.find(({ name }) => name === currentMutation.caseName);
          if (!testCase) {
            throw new Error(`mounted commit-row session fixtures mutation ${currentMutation.name} has no case named ${currentMutation.caseName}`);
          }
          const expected = testCase.expectedRows.find(({ sessionId }) => sessionId === currentMutation.sessionId);
          if (!expected) {
            throw new Error(`mounted commit-row session fixtures mutation ${currentMutation.name} has no expected row for ${currentMutation.sessionId}`);
          }
          return expected;
        },
      );
    });
  }

  it('hover-relay: hovering a Recent AI conversations row highlights the REAL node; leaving clears it', async () => {
    const user = userEvent.setup();
    render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} />);
    await screen.findByRole('application', { name: `Code map of ${PROJECT}` });

    // TASK_INGEST edited internal/ingest/pipeline.go; at Folders grain the file
    // collapses into its visible ancestor, internal/ingest (liftIdsToVisible).
    const row = (
      await screen.findAllByRole('link', { name: 'Open task at turn 12 of session sess-new' })
    )[0];
    const ingestNode = screen.getByRole('button', { name: /ingest: folder/i });
    expect(ingestNode).not.toHaveAttribute('data-highlighted');

    await user.hover(row);
    await waitFor(() => expect(ingestNode).toHaveAttribute('data-highlighted', 'true'));

    await user.unhover(row);
    await waitFor(() => expect(ingestNode).not.toHaveAttribute('data-highlighted'));
  });

  it('keyboard parity: focusing a Recent AI conversations row highlights the REAL node; blurring clears it', async () => {
    render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} />);
    await screen.findByRole('application', { name: `Code map of ${PROJECT}` });

    const row = (
      await screen.findAllByRole('link', { name: 'Open task at turn 12 of session sess-new' })
    )[0];
    const ingestNode = screen.getByRole('button', { name: /ingest: folder/i });

    fireEvent.focus(row);
    await waitFor(() => expect(ingestNode).toHaveAttribute('data-highlighted', 'true'));
    fireEvent.blur(row);
    await waitFor(() => expect(ingestNode).not.toHaveAttribute('data-highlighted'));
  });

  it('keyboard: real arrow-key roving focus + Enter selects a node on the REAL canvas (keyboard parity)', async () => {
    const user = userEvent.setup();
    render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} />);
    await screen.findByRole('application', { name: `Code map of ${PROJECT}` });

    // MapCanvas's roving-focus surface (a distinct group from the outer
    // role=application landmark); arrow keys move a keyboard cursor, Enter
    // selects the focused node, exactly as a real user would drive it.
    const surface = screen.getByRole('group', { name: /map canvas/i });
    surface.focus();
    await user.keyboard('{ArrowRight}'); // no prior focus -> lands on the first node in reading order
    await user.keyboard('{Enter}');

    // selecting swaps the rail from the project panel to the node panel.
    expect((await screen.findAllByText('Code area'))[0]).toBeInTheDocument();
  });

  it('real route composition: MapShell renders the wire→adapter→CodeMap chain with no props dropped', async () => {
    render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} />);
    const app = await screen.findByRole('application', { name: `Code map of ${PROJECT}` });

    // the cycle violation (GRAPH.violations: internal -> cmd) reaches the REAL
    // canvas as a rendered violation badge on the visible ancestor it aggregates
    // onto (cmd); proving the adapter's violations array, not just its
    // nodes/edges, survives the real MapPageClient -> adapter -> CodeMap ->
    // MapCanvas chain (a mocked CodeMap can't prove this).
    expect(app.querySelector('.mc-node-viol')).toBeTruthy();
    expect(screen.getByRole('button', { name: /cmd: folder.*1 violation/i })).toBeInTheDocument();
  });

  it('real /map/{project} PAGE composition: MapPageClient (breadcrumbs + heading + keyed shell) mounts the REAL CodeMap', async () => {
    const user = userEvent.setup();
    render(<MapPageClient projectHash={PROJECT_HASH} />);

    // route-owned chrome (breadcrumbs, heading) + the real canvas in one tree,
    // not a standalone CodeMap harness rendered apart from the actual route.
    expect(await screen.findByRole('link', { name: 'map' })).toBeInTheDocument();
    expect(await screen.findByRole('heading', { name: PROJECT })).toBeInTheDocument();
    expect(await screen.findByRole('tree', { name: 'code areas' })).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'spatial map' }));
    const app = await screen.findByRole('application', { name: `Code map of ${PROJECT}` });
    expect(app).toHaveClass('mc');
    expect(screen.getByRole('button', { name: /cmd: folder/i })).toBeInTheDocument();
  });

  it('mounted navigator keyboard moves the real roving tab stop and selects the focused code area', async () => {
    const user = userEvent.setup();
    render(<MapPageClient projectName={PROJECT} />);

    const tree = await screen.findByRole('tree', { name: 'code areas' });
    const rows = screen.getAllByRole('treeitem');
    expect(rows.length).toBeGreaterThan(1);
    const expandable = rows.find((row) => row.hasAttribute('aria-expanded'));
    expect(expandable).toBeDefined();
    await user.click(expandable!);
    await user.keyboard('{Home}{ArrowUp}');
    expect(document.activeElement).toBe(rows[0]);
    await user.keyboard('{End}{ArrowDown}');
    expect(document.activeElement).toBe(rows.at(-1));
    expandable!.focus();
    await user.keyboard('{ArrowRight}');
    expect(expandable).toHaveAttribute('aria-expanded', 'true');
    await user.keyboard('{ArrowRight}');
    const expandedRows = screen.getAllByRole('treeitem');
    const expandableIndex = expandedRows.indexOf(expandable!);
    expect(expandableIndex).toBeGreaterThanOrEqual(0);
    expect(document.activeElement).toBe(expandedRows[expandableIndex + 1]);
    await user.keyboard('{ArrowLeft}');
    expect(document.activeElement).toBe(expandable);
    await user.keyboard('{ArrowLeft}');
    expect(expandable).toHaveAttribute('aria-expanded', 'false');
    await user.keyboard('{ArrowDown}{Enter}');
    expect(document.activeElement).toHaveAttribute('aria-selected', 'true');
    expect(tree.querySelectorAll('[role="treeitem"][tabindex="0"]')).toHaveLength(1);

    const filter = screen.getByRole('textbox', { name: 'filter code areas' });
    await user.clear(filter);
    await user.type(filter, 'ingest');
    await waitFor(() => {
      expect(
        screen.getAllByRole('treeitem').some((row) => row.textContent?.includes('ingest')),
      ).toBe(true);
    });

    const filteredRows = screen.getAllByRole('treeitem');
    fireEvent.focus(filteredRows[0]);
    await user.keyboard('{End}{Enter}');
    await waitFor(() => {
      expect(
        screen.getAllByRole('treeitem').some((row) => row.getAttribute('aria-selected') === 'true'),
      ).toBe(true);
    });
  });

  it('keeps pointer selection, filtering, and open-in-map in one canonical mounted state', async () => {
    const user = userEvent.setup();
    window.history.replaceState({}, '', navigatorRouteFixture.navigator_interaction.url);
    render(<MapPageClient projectName={PROJECT} />);

    const tree = await screen.findByRole('tree', { name: 'code areas' });
    expect(tree).toBeInTheDocument();
    const selected = screen.getByRole('treeitem', {
      name: new RegExp(`^${navigatorRouteFixture.navigator_interaction.selectedNode}\\b`, 'i'),
    });
    await user.click(selected);
    expect(selected).toHaveAttribute('aria-selected', 'true');

    const filter = screen.getByRole('textbox', { name: 'filter code areas' });
    await user.type(filter, navigatorRouteFixture.navigator_interaction.filter);
    expect(new URLSearchParams(window.location.search).get('filter'))
      .toBe(navigatorRouteFixture.navigator_interaction.filter);

    await user.click(screen.getByRole('button', { name: /open in map/i }));
    expect(await screen.findByRole('application', { name: `Code map of ${PROJECT}` }))
      .toBeInTheDocument();
    const route = new URLSearchParams(window.location.search);
    expect(route.get('mode')).toBe('canvas');
    expect(route.get('node')).toBe(navigatorRouteFixture.navigator_interaction.selectedNode);
    expect(route.get('filter')).toBe(navigatorRouteFixture.navigator_interaction.filter);
  });

  it('persists real canvas disclosure and viewport controls through the canonical route', async () => {
    const user = userEvent.setup();
    window.history.replaceState({}, '', navigatorRouteFixture.canvas_interaction.url);
    render(<MapPageClient projectName={PROJECT} />);

    await screen.findByRole('application', { name: `Code map of ${PROJECT}` });
    const disclosure = await screen.findByRole('button', {
      name: new RegExp(navigatorRouteFixture.canvas_interaction.disclosureLabel, 'i'),
    });
    await user.click(disclosure);
    expect(new URLSearchParams(window.location.search).getAll('expand')).toContain('internal/ingest');

    await user.click(screen.getByRole('button', { name: 'zoom in' }));
    await waitFor(() => {
      const route = new URLSearchParams(window.location.search);
      const scale = Number(route.get('scale'));
      expect(isCodeMapViewportScale(scale)).toBe(true);
      expect(route.get('panX')).not.toBeNull();
      expect(route.get('panY')).not.toBeNull();
    });
  });

  it('restores a deep-linked canvas and returns to the same navigator selection, filter, focus, and grain', async () => {
    const user = userEvent.setup();
    window.history.replaceState({}, '', navigatorRouteFixture.deep_link.url);
    render(<MapPageClient projectName={PROJECT} />);

    const app = await screen.findByRole('application', { name: `Code map of ${PROJECT}` });
    expect(app).toBeInTheDocument();
    expect(screen.getByRole('radio', { name: navigatorRouteFixture.deep_link.grain })).toHaveAttribute(
      'aria-checked',
      'true',
    );
    expect(screen.getByRole('button', { name: /pipeline\.go: file/i })).toHaveAttribute('data-selected', 'true');
    expect(app.querySelector('.mc-stage')).toHaveStyle({
      transform: `translate(${navigatorRouteFixture.deep_link.viewport.panX}px, ${navigatorRouteFixture.deep_link.viewport.panY}px) scale(${navigatorRouteFixture.deep_link.viewport.scale})`,
    });

    await user.click(screen.getByRole('button', { name: 'back to browse' }));
    const tree = await screen.findByRole('tree', { name: 'code areas' });
    expect(tree).toBeInTheDocument();
    expect(screen.getByRole('textbox', { name: 'filter code areas' })).toHaveValue(
      navigatorRouteFixture.deep_link.filter,
    );
    expect(screen.getByRole('treeitem', { name: /pipeline\.go/i })).toHaveAttribute('aria-selected', 'true');
    expect(window.location.search).toContain('mode=navigator');
    expect(new URLSearchParams(window.location.search).get('focus')).toBe(navigatorRouteFixture.deep_link.focus);
  });

  it('reconciles populated, bare, and populated history states through mounted popstate', async () => {
    window.history.replaceState({}, '', navigatorRouteFixture.deep_link.url);
    render(<MapPageClient projectName={PROJECT} />);
    const app = await screen.findByRole('application', { name: `Code map of ${PROJECT}` });
    expect(screen.getByRole('radio', { name: navigatorRouteFixture.deep_link.grain })).toHaveAttribute('aria-checked', 'true');
    expect(app.querySelector('.mc-stage')).toHaveStyle({
      transform: `translate(${navigatorRouteFixture.deep_link.viewport.panX}px, ${navigatorRouteFixture.deep_link.viewport.panY}px) scale(${navigatorRouteFixture.deep_link.viewport.scale})`,
    });

    window.history.replaceState({}, '', `/map/${PROJECT_HASH}`);
    fireEvent(window, new PopStateEvent('popstate'));
    expect(await screen.findByRole('tree', { name: 'code areas' })).toHaveAttribute('data-grain', 'package');
    expect(screen.getByRole('textbox', { name: 'filter code areas' })).toHaveValue('');

    window.history.replaceState({}, '', navigatorRouteFixture.deep_link.url);
    fireEvent(window, new PopStateEvent('popstate'));
    const restored = await screen.findByRole('application', { name: `Code map of ${PROJECT}` });
    expect(screen.getByRole('radio', { name: navigatorRouteFixture.deep_link.grain })).toHaveAttribute('aria-checked', 'true');
    expect(screen.getByRole('button', { name: /pipeline\.go: file/i })).toHaveAttribute('data-selected', 'true');
    expect(restored.querySelector('.mc-stage')).toHaveStyle({
      transform: `translate(${navigatorRouteFixture.deep_link.viewport.panX}px, ${navigatorRouteFixture.deep_link.viewport.panY}px) scale(${navigatorRouteFixture.deep_link.viewport.scale})`,
    });
  });

  it('recovers stale selection explicitly and rejects an out-of-policy viewport', async () => {
    const user = userEvent.setup();
    window.history.replaceState({}, '', navigatorRouteFixture.stale_selection.url);
    render(<MapPageClient projectName={PROJECT} />);

    expect(await screen.findByText(/selected code area is not present/i)).toBeInTheDocument();
    expect(new URLSearchParams(window.location.search).get('node'))
      .toBe(navigatorRouteFixture.stale_selection.selectedNode);
    await user.click(screen.getByRole('button', { name: 'clear selection' }));
    await waitFor(() => expect(new URLSearchParams(window.location.search).get('node')).toBeNull());

    window.history.replaceState({}, '', navigatorRouteFixture.invalid_viewport.url);
    fireEvent(window, new PopStateEvent('popstate'));
    expect(await screen.findByRole('tree', { name: 'code areas' })).toHaveAttribute('data-grain', 'package');
    await waitFor(() => {
      const route = new URLSearchParams(window.location.search);
      expect(route.get('mode')).toBe('navigator');
      expect(route.get('scale')).toBeNull();
    });
  });

  it('does not write history for exact canonical initial hydration', async () => {
    window.history.replaceState({}, '', navigatorRouteFixture.history.canonicalUrl);
    const initialHref = `${window.location.pathname}${window.location.search}`;
    const replaceState = vi.spyOn(window.history, 'replaceState');
    render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} showLedger />);
    await screen.findByRole('application', { name: `Code map of ${PROJECT}` });
    expect(`${window.location.pathname}${window.location.search}`).toBe(initialHref);
    expect(historyWriteTargets(replaceState)).toEqual([]);
    replaceState.mockRestore();
  });

  it('repairs one noncanonical initial URL exactly once and then settles', async () => {
    window.history.replaceState({}, '', navigatorRouteFixture.history.noncanonicalUrl);
    const replaceState = vi.spyOn(window.history, 'replaceState');
    render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} showLedger />);
    await screen.findByRole('tree', { name: 'code areas' });
    await waitFor(() => {
      expect(historyWriteTargets(replaceState)).toEqual([
        navigatorRouteFixture.history.canonicalTarget,
      ]);
    });
    expect(isExactHistoryRepair(
      historyWriteTargets(replaceState),
      navigatorRouteFixture.history.canonicalTarget,
    )).toBe(true);
    expect(`${window.location.pathname}${window.location.search}`)
      .toBe(navigatorRouteFixture.history.canonicalTarget);
    await Promise.resolve();
    expect(historyWriteTargets(replaceState)).toEqual([
      navigatorRouteFixture.history.canonicalTarget,
    ]);
    replaceState.mockRestore();
  });

  it.each(historyOracleMutations)('history repair oracle rejects $name', (mutation) => {
    expect(isExactHistoryRepair(
      mutation.writes,
      navigatorRouteFixture.history.canonicalTarget,
    )).toBe(false);
  });

  it('repairs malformed expansion state on initial load and popstate without crashing', async () => {
    window.history.replaceState({}, '', navigatorRouteFixture.history.malformedInitialUrl);
    const replaceState = vi.spyOn(window.history, 'replaceState');
    render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} showLedger />);
    expect(await screen.findByRole('tree', { name: 'code areas' })).toBeInTheDocument();
    await waitFor(() => {
      expect(historyWriteTargets(replaceState)).toEqual([
        navigatorRouteFixture.history.malformedTarget,
      ]);
    });

    window.history.pushState({}, '', navigatorRouteFixture.history.malformedPopstateUrl);
    replaceState.mockClear();
    fireEvent(window, new PopStateEvent('popstate'));
    expect(await screen.findByRole('tree', { name: 'code areas' })).toBeInTheDocument();
    await waitFor(() => {
      expect(historyWriteTargets(replaceState)).toEqual([
        navigatorRouteFixture.history.malformedTarget,
      ]);
    });
    expect(`${window.location.pathname}${window.location.search}`)
      .toBe(navigatorRouteFixture.history.malformedTarget);
    replaceState.mockRestore();
  });

  it('does not echo canonical popstate hydration back into history', async () => {
    window.history.replaceState({}, '', navigatorRouteFixture.history.canonicalTarget);
    render(<MapShell projectHash={PROJECT_HASH} projectName={PROJECT} showLedger />);
    await screen.findByRole('tree', { name: 'code areas' });

    window.history.pushState({}, '', navigatorRouteFixture.history.canonicalUrl);
    const replaceState = vi.spyOn(window.history, 'replaceState');
    fireEvent(window, new PopStateEvent('popstate'));
    await screen.findByRole('application', { name: `Code map of ${PROJECT}` });
    expect(historyWriteTargets(replaceState)).toEqual([]);
    replaceState.mockRestore();
  });
});
