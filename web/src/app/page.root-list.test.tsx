import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import HomePage from './page';
import type { DecodedProjectSummariesPayload } from '@/lib/api/map';
import { ALPHA_HASH, BETA_HASH, makeSession } from '@/app/review/[[...segments]]/test-fixtures';
import { formatMinutes } from '@/lib/home/rootStats';
import { formatRelative } from '@/app/review/[[...segments]]/format';
import {
  parseStrictYAML,
  requireExactFields,
  requireExactRequiredFields,
  requireRecord,
  requireUniqueNames,
} from '@/test/strictYaml';

// The root page's session list, mounted: the real publications client decodes
// what a stubbed fetch serves for GET /api/v1/sync/sessions and
// GET /api/v1/publications, and the counts and rows are read off the page.

vi.mock('next/navigation', () => ({ useRouter: () => ({ push: vi.fn() }) }));

// One payload per WebSocket topic; a subscription reads its own topic.
const topics: Record<string, unknown> = {};
vi.mock('@/contexts/WebSocketContext', () => ({
  useChannel: (subs: unknown) => {
    const first = Array.isArray(subs) ? subs[0] : subs;
    const topic = typeof first === 'string' ? first : (first as { topic: string }).topic;
    return { data: topics[topic], connected: true, error: null, errorCode: undefined };
  },
}));

const api = vi.hoisted(() => ({
  fetchProjectSummaries: vi.fn<() => Promise<DecodedProjectSummariesPayload>>(),
  cachedProjectSummaries: () => null,
}));
vi.mock('@/lib/api/map', () => api);

const PROJECTS = {
  alpha: { name: 'alpha-project', hash: ALPHA_HASH },
  beta: { name: 'beta-project', hash: BETA_HASH },
} as const;
type ProjectKey = keyof typeof PROJECTS;

const FILTER_LABELS = {
  all: 'all',
  notPublished: 'not published',
  published: 'published',
  auto: 'auto',
} as const;
type CountKey = keyof typeof FILTER_LABELS;
const FILTER_KEYS: Record<string, CountKey> = {
  all: 'all',
  'not-published': 'notPublished',
  published: 'published',
  auto: 'auto',
};

interface SessionSpec {
  id: string;
  count?: number;
  chainLength?: number;
  project: ProjectKey;
  syncStatus: 'new' | 'updated' | 'synced';
  state: 'unpublished' | 'published';
  autoPublish?: boolean;
  audience?: number;
  audienceStatuses?: ('approved' | 'pending')[];
  outsideSelection?: boolean;
  publication?: boolean;
  parentSessionId?: string;
  parentUnresolved?: boolean;
  title?: string;
  preview?: string;
  turnCount?: number;
  durationMs?: number;
  startTime?: string;
}

interface FilterCase {
  name: string;
  readState?: 'pending' | 'error';
  expectedSessions?: null;
  sessions: SessionSpec[];
  filter: string;
  expectedCounts: Record<CountKey, number>;
  expectedRows: string[];
  expectedPages?: number[];
  expectedLabels?: Record<string, string>;
  expectedGroups?: string[];
  steps?: { expand?: string; children: string[]; groups?: string[]; refresh?: boolean; filter?: string }[];
  childLabels?: Record<string, string>;
  searchChild?: string;
}

const REQUIRED_NAMES = [
  'pending-root-read-omits-session-stat',
  'failed-root-read-omits-session-stat',
  'all-counts',
  'not-published-counts',
  'published-counts',
  'auto-counts',
  'outside-selection-hidden',
  'load-more',
  'pending-collectives-cannot-read',
  'parent-visible-child-grouped',
  'recursive-family-and-refresh',
  'deep-family-expands-one-level-at-a-time',
  'snapshot-and-evidence-children-with-absent-parents-hidden',
  'unresolved-child-hidden',
  'cache-only-child-grouped',
  'filtered-parent-omits-child',
  'selection-hidden-parent-and-child',
  'fork-without-markers-top-level',
  'paging-keeps-child-with-parent',
  'child-search-keeps-publish-label',
] as const;

function loadFilterCases(): FilterCase[] {
  const source = readFileSync(resolve(process.cwd(), 'src/app/testdata/root-publish-filters.yaml'), 'utf8');
  const root = requireRecord(parseStrictYAML(source, 'root publish filters fixture'), 'root publish filters fixture');
  requireExactRequiredFields(root, ['requiredNames', 'cases'], 'root publish filters fixture');
  if (!Array.isArray(root.requiredNames) || [...root.requiredNames].sort().join() !== [...REQUIRED_NAMES].sort().join()) {
    throw new Error(`root publish filters fixture requiredNames must be exactly ${REQUIRED_NAMES.join(', ')}`);
  }
  if (!Array.isArray(root.cases)) throw new Error('root publish filters fixture cases must be a list');
  const cases = root.cases.map((value, index) => requireRecord(value, `root publish filters fixture.cases[${index}]`));
  requireUniqueNames(cases, 'root publish filters fixture.cases');
  cases.forEach((testCase, index) => {
    const where = `root publish filters fixture.cases[${index}]`;
    requireExactFields(testCase, ['name', 'readState', 'expectedSessions', 'sessions', 'filter', 'expectedCounts', 'expectedRows', 'expectedPages', 'expectedLabels', 'expectedGroups', 'steps', 'childLabels', 'searchChild'], where);
    if (testCase.readState !== undefined && (!['pending', 'error'].includes(String(testCase.readState)) || testCase.expectedSessions !== null)) {
      throw new Error(`${where}: an unavailable root read must name pending or error and expect null sessions`);
    }
    if (testCase.steps !== undefined) {
      if (!Array.isArray(testCase.steps)) throw new Error(`${where}.steps must be a list`);
      for (const step of testCase.steps) requireExactFields(requireRecord(step, where), ['expand', 'children', 'groups', 'refresh', 'filter'], where);
    }
    for (const field of ['name', 'sessions', 'filter', 'expectedCounts', 'expectedRows']) {
      if (!(field in testCase)) throw new Error(`${where} is missing ${field}`);
    }
    if (!(String(testCase.filter) in FILTER_KEYS)) throw new Error(`${where}.filter ${String(testCase.filter)} is not a filter`);
    requireExactRequiredFields(requireRecord(testCase.expectedCounts, `${where}.expectedCounts`), Object.keys(FILTER_LABELS), `${where}.expectedCounts`);
    if (!Array.isArray(testCase.sessions)) throw new Error(`${where}.sessions must be a list`);
    testCase.sessions.forEach((session, sessionIndex) => {
      const at = `${where}.sessions[${sessionIndex}]`;
      const spec = requireRecord(session, at);
      requireExactFields(spec, ['id', 'count', 'chainLength', 'project', 'syncStatus', 'state', 'autoPublish', 'audience', 'audienceStatuses', 'outsideSelection', 'publication', 'parentSessionId', 'parentUnresolved', 'title', 'preview', 'turnCount', 'durationMs', 'startTime'], at);
      if (spec.chainLength !== undefined && (!Number.isSafeInteger(spec.chainLength) || Number(spec.chainLength) < 2 || spec.count !== undefined || spec.parentSessionId !== undefined)) {
        throw new Error(`${at}.chainLength must be an integer >= 2 on a root without count or parentSessionId`);
      }
      if (!(String(spec.project) in PROJECTS)) throw new Error(`${at}.project must be alpha or beta`);
      if (!['new', 'updated', 'synced'].includes(String(spec.syncStatus))) throw new Error(`${at}.syncStatus is invalid`);
      if (!['unpublished', 'published'].includes(String(spec.state))) throw new Error(`${at}.state is invalid`);
    });
  });
  const names = cases.map((testCase) => testCase.name);
  const missing = REQUIRED_NAMES.filter((name) => !names.includes(name));
  const unknown = names.filter((name) => !(REQUIRED_NAMES as readonly unknown[]).includes(name));
  if (missing.length || unknown.length) {
    throw new Error(`root publish filters fixture must hold exactly the required cases; missing ${missing.join(', ') || 'none'}, unknown ${unknown.join(', ') || 'none'}`);
  }
  return cases as unknown as FilterCase[];
}

/** Expand fixture repetitions and predecessor-linked chains into served session rows. */
function expand(specs: SessionSpec[]): SessionSpec[] {
  return specs.flatMap((spec) =>
    spec.chainLength
      ? Array.from({ length: spec.chainLength }, (_, index) => ({ ...spec, id: `${spec.id}-${index}`, parentSessionId: index ? `${spec.id}-${index - 1}` : undefined, chainLength: undefined }))
      : spec.count && spec.count > 1
      ? Array.from({ length: spec.count }, (_, index) => ({ ...spec, id: `${spec.id}-${String(index).padStart(2, '0')}`, count: undefined }))
      : [spec],
  );
}

function uuid(prefix: string, index: number): string {
  return `${prefix}-0000-4000-8000-${String(index).padStart(12, '0')}`;
}

function syncRow(spec: SessionSpec, index: number) {
  const project = PROJECTS[spec.project];
  return {
    id: spec.id,
    harness: 'claude-code',
    projectName: project.name,
    projectHash: project.hash,
    hostSlug: 'fixture-host',
    startTime: spec.startTime ?? new Date(Date.UTC(2026, 5, 1) - index * 3_600_000).toISOString(),
    durationMs: spec.durationMs ?? 25 * 60_000,
    totalTokens: 12_000,
    turnCount: spec.turnCount ?? 8,
    model: 'fixture-model',
    syncStatus: spec.syncStatus,
    previouslyPushed: spec.state === 'published',
    parentSessionId: spec.parentSessionId,
    parentUnresolved: spec.parentUnresolved,
  };
}

function publicationRow(spec: SessionSpec, index: number, withAudience: boolean) {
  const published = spec.state === 'published';
  return {
    sessionId: spec.id,
    state: spec.state,
    autoPublish: spec.autoPublish ?? false,
    outsideSelection: spec.outsideSelection ?? false,
    ...(published
      ? {
          transcriptId: uuid('00000000', index),
          transcriptUrl: `https://village.example/transcripts/${index}`,
          publishedAt: '2026-06-01T10:00:00Z',
        }
      : {}),
    ...(published && withAudience
      ? {
          audience: Array.from({ length: spec.audience ?? 0 }, (_, member) => ({
            collectiveId: uuid('10000000', member),
            name: `collective ${member + 1}`,
            status: spec.audienceStatuses?.[member] ?? 'approved',
          })),
        }
      : {}),
  };
}

interface ServeOptions {
  syncPending?: Promise<void>;
  audienceStatus?: number;
  publicationsError?: { status: number; code: string; error: string };
  search?: unknown;
}

/** Every audience read, with whether its row was rendered at request time. */
const audienceChecks: Array<{ id: string; rendered: boolean }> = [];

function json(body: unknown, status = 200) {
  return { ok: status >= 200 && status < 300, status, json: async () => body, text: async () => JSON.stringify(body) };
}

function serve(specs: SessionSpec[], options: ServeOptions = {}) {
  const requests: URL[] = [];
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
    const url = new URL(String(input));
    requests.push(url);
    if (url.searchParams.get('include') === 'audience') {
      for (const id of url.searchParams.get('sessionIds')!.split(',')) {
        audienceChecks.push({ id, rendered: !!document.querySelector(`[data-session-id="${id}"], [data-session-group-child="${id}"]`) });
      }
    }
    if (url.pathname === '/api/v1/sync/sessions') {
      await options.syncPending;
      return json({ sessions: specs.map(syncRow) });
    }
    if (url.pathname === '/api/v1/publications') {
      if (options.publicationsError) {
        return json({ error: options.publicationsError.error, code: options.publicationsError.code }, options.publicationsError.status);
      }
      const withAudience = url.searchParams.get('include') === 'audience';
      if (withAudience && options.audienceStatus) return json({ error: 'Village could not be read', code: 'village_unreachable' }, options.audienceStatus);
      const ids = new Set((url.searchParams.get('sessionIds') ?? '').split(','));
      return json({
        publications: specs
          .map((spec, index) => ({ spec, index }))
          .filter(({ spec }) => ids.has(spec.id) && spec.publication !== false)
          .map(({ spec, index }) => publicationRow(spec, index, withAudience)),
      });
    }
    if (url.pathname === '/api/v1/search' && options.search) return json(options.search);
    return json({ error: `unexpected ${url.pathname}` }, 404);
  }));
  return requests;
}

function summaries(): DecodedProjectSummariesPayload {
  return {
    projects: Object.values(PROJECTS).map((project) => ({
      projectHash: project.hash,
      project: project.name,
      sessions: 4,
      recordedFiles: 3,
      totalFiles: 4,
      lastWorkMs: Date.UTC(2026, 5, 1),
      openChanges: 0,
    })),
    selection: { active: false, hiddenProjects: 0, hiddenSessions: 0 },
  } as DecodedProjectSummariesPayload;
}

function filterButtons(list: HTMLElement) {
  return within(within(list).getByRole('group', { name: 'filter by publish state' })).getAllByRole('button');
}

function filterButton(list: HTMLElement, key: CountKey): HTMLElement {
  const label = FILTER_LABELS[key];
  const button = filterButtons(list).find((candidate) => candidate.textContent?.replace(/\s+[\d,]+$/, '') === label);
  if (!button) throw new Error(`no ${label} filter option`);
  return button;
}

function countOf(button: HTMLElement): number {
  return Number(button.textContent?.replace(/^.*\s([\d,]+)$/, '$1').replace(/,/g, ''));
}

function rowsOnScreen(list: HTMLElement): string[] {
  return [...list.querySelectorAll('tbody [data-session-id]')].map((node) => node.getAttribute('data-session-id') ?? '');
}

/** Presses load more until it is gone; returns the rows on screen after each step. */
function loadAll(list: HTMLElement): number[] {
  const pages = [rowsOnScreen(list).length];
  for (let press = 0; press < 100; press++) {
    const more = within(list).queryByRole('button', { name: 'load more' });
    if (!more) break;
    fireEvent.click(more);
    pages.push(rowsOnScreen(list).length);
  }
  return pages;
}

function stateLabel(list: HTMLElement, id: string): string {
  const row = list.querySelector(`[data-session-id="${id}"], [data-session-group-child="${id}"]`)?.closest('tr');
  return row?.querySelector('.pub-state-text')?.textContent ?? '';
}

beforeEach(() => {
  for (const key of Object.keys(topics)) delete topics[key];
  audienceChecks.length = 0;
  topics.sessions = { sessions: [makeSession({ id: 'channel-row', project: 'alpha-project' })] };
  api.fetchProjectSummaries.mockResolvedValue(summaries());
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.clearAllMocks();
});

describe('root page publish filters', () => {
  for (const testCase of loadFilterCases()) {
    it(testCase.name, async () => {
      const specs = expand(testCase.sessions);
      topics.dashboard = { totalSessions: 1284 };
      if (testCase.readState) {
        let release!: () => void;
        const syncPending = new Promise<void>((resolve) => { release = resolve; });
        serve(specs, testCase.readState === 'pending' ? { syncPending } : {
          publicationsError: { status: 503, code: 'service_unavailable', error: 'publication state is unavailable; retry the session list' },
        });
        render(<HomePage />);
        try {
          const strip = await screen.findByRole('list', { name: 'your sessions in numbers' });
          if (testCase.readState === 'error') await screen.findByRole('button', { name: 'retry the session list' });
          const pair = within(strip).getAllByRole('listitem').find((item) => / sessions$/.test(item.textContent ?? ''));
          expect(pair?.textContent ?? null).toBe(testCase.expectedSessions);
        } finally {
          cleanup();
          await act(async () => { release(); });
        }
        return;
      }
       const searchSpec = specs.find((spec) => spec.id === testCase.searchChild);
       const search = searchSpec ? {
         items: [{ kind: 'transcript', transcript: {
           session: makeSession({ id: searchSpec.id, project: PROJECTS[searchSpec.project].name, projectHash: PROJECTS[searchSpec.project].hash }),
           matches: [{ entryIndex: 0, project: PROJECTS[searchSpec.project].name, projectHash: PROJECTS[searchSpec.project].hash, role: 'user', score: 1, sessionId: searchSpec.id, snippet: 'child search match' }],
         }, helperGroups: [] }], page: 1, limit: 20, totalItems: 1, ordinarySessionTotal: 1, helperThreadTotal: 0,
       } : undefined;
       const requests = serve(specs, { search });
       topics.quality = { sessions: specs.filter((spec) => spec.title).map((spec) => ({ id: spec.id, title: spec.title })) };
       if (specs.some((spec) => spec.preview)) topics.sessions = { sessions: specs.filter((spec) => spec.preview).map((spec) => makeSession({ id: spec.id, preview: spec.preview })) };
       const view = render(<HomePage />);
      // Under full-suite parallel load the mounted region can take longer than the
      // 1s default findBy timeout (observed on a cold Vite dep cache); wait up to
      // five seconds. The assertions below are unchanged.
      let list = await screen.findByRole('region', { name: 'sessions' }, { timeout: 5000 });
      const assertSessionsStat = () => {
        const strip = screen.getByRole('list', { name: 'your sessions in numbers' });
        expect(within(strip).getAllByRole('listitem').find((item) => / sessions$/.test(item.textContent ?? ''))?.textContent).toBe(`${testCase.expectedCounts.all.toLocaleString('en-US')} sessions`);
      };
      assertSessionsStat();

      const assertGroupAttachment = (group: Element) => {
        const parentId = group.getAttribute('data-session-group');
        const parent = list.querySelector(`[data-session-id="${parentId}"], [data-session-group-child="${parentId}"]`);
        expect(parent, `parent content for ${parentId}`).not.toBeNull();
        const cell = parent!.closest('td');
        expect(cell, `primary cell for ${parentId}`).not.toBeNull();
        expect(group.closest('td')).toBe(cell);
        expect(cell!.parentElement!.firstElementChild).toBe(cell);
        expect(parent!.nextElementSibling).toBe(group);
        expect(parent!.contains(group)).toBe(false);
        expect(group.querySelector('[data-session-id]')).toBeNull();
        expect(group.querySelector('.sgd')).toHaveClass('sgd-bare', 'sgd-indent');
      };

      for (const key of Object.keys(FILTER_LABELS) as CountKey[]) {
        expect(countOf(filterButton(list, key)), `${key} count`).toBe(testCase.expectedCounts[key]);
      }

      fireEvent.click(filterButton(list, FILTER_KEYS[testCase.filter]));
      assertSessionsStat();
      if (testCase.expectedGroups) {
        expect([...list.querySelectorAll('[data-session-group]')].map((node) => node.getAttribute('data-session-group'))).toEqual(testCase.expectedGroups);
        expect(list.querySelectorAll('[data-session-group-child]')).toHaveLength(0);
        for (const group of list.querySelectorAll('[data-session-group]')) {
          assertGroupAttachment(group);
          expect(within(group as HTMLElement).getByRole('button')).toHaveAttribute('aria-expanded', 'false');
        }
      }
      const pages = loadAll(list);
      assertSessionsStat();
      expect(pages).toEqual(testCase.expectedPages ?? [testCase.expectedRows.length]);
      expect(rowsOnScreen(list)).toEqual(testCase.expectedRows);

      const errors = vi.spyOn(console, 'error');
      for (const step of testCase.steps ?? []) {
        if (step.filter) fireEvent.click(filterButton(list, FILTER_KEYS[step.filter]));
        if (step.expand) {
          const group = list.querySelector(`[data-session-group="${step.expand}"]`) as HTMLElement;
          fireEvent.click(within(group).getAllByRole('button')[0]);
        }
        if (step.refresh) {
          const before = requests.filter((url) => url.pathname === '/api/v1/sync/sessions').length;
          topics.sessions = { sessions: [...specs.filter((spec) => spec.preview).map((spec) => makeSession({ id: spec.id, preview: spec.preview })), makeSession({ id: 'refresh-root', project: 'alpha-project' })] };
          await act(async () => { view.rerender(<HomePage />); });
          await waitFor(() => expect(requests.filter((url) => url.pathname === '/api/v1/sync/sessions').length).toBeGreaterThan(before));
        }
        const children = [...list.querySelectorAll('[data-session-group-child]')];
        expect(children.map((node) => node.getAttribute('data-session-group-child'))).toEqual(step.children);
        if (step.groups) {
          expect([...list.querySelectorAll('[data-session-group]')].map((node) => node.getAttribute('data-session-group'))).toEqual(step.groups);
          for (const group of list.querySelectorAll('[data-session-group]')) {
            assertGroupAttachment(group);
            const toggle = within(group as HTMLElement).getAllByRole('button')[0];
            expect(toggle).toHaveAttribute('aria-expanded', String(step.children.some((id) => specs.find((spec) => spec.id === id)?.parentSessionId === group.getAttribute('data-session-group'))));
          }
        }
        expect(rowsOnScreen(list)).toEqual(testCase.expectedRows);
        assertSessionsStat();
        expect(list.querySelectorAll('[data-session-group]')).toHaveLength(new Set([...list.querySelectorAll('[data-session-group]')].map((node) => node.getAttribute('data-session-group'))).size);
        for (const child of children) {
          const spec = specs.find((row) => row.id === child.getAttribute('data-session-group-child'))!;
          expect(child).not.toHaveAttribute('data-session-id');
          expect(child.querySelector('.pub-state')).toBeNull();
          const link = within(child as HTMLElement).getByRole('link');
          expect(link).toHaveAttribute('href', `/projects/${PROJECTS[spec.project].hash}/${spec.id}`);
          expect(link.textContent?.trim()).not.toBe('');
          if (testCase.childLabels?.[spec.id]) expect(link).toHaveTextContent(testCase.childLabels[spec.id]);
          // Each child is its own table row, immediately under its parent, with
          // every column showing the child's own served values.
          const row = child.closest('tr')!;
          const cells = [...row.querySelectorAll('td')];
          expect(cells).toHaveLength(6);
          const served = syncRow(spec, specs.indexOf(spec));
          expect(cells[1].textContent).toContain(PROJECTS[spec.project].name);
          expect(cells[1].querySelector('a')?.getAttribute('href')).toContain(PROJECTS[spec.project].hash);
          expect(cells[2].textContent?.trim()).toBe(served.turnCount.toLocaleString('en-US'));
          expect(cells[3].textContent?.trim()).toBe(formatMinutes(served.durationMs / 60_000));
          const started = Date.parse(served.startTime);
          expect(cells[4].textContent?.trim()).toBe(Number.isNaN(started) ? '—' : formatRelative(started));
          expect(child.id).toBe(`session-group-rows:${spec.id}`);
          const parentRow = list.querySelector(`[data-session-id="${spec.parentSessionId}"], [data-session-group-child="${spec.parentSessionId}"]`)!.closest('tr');
          if (row.previousElementSibling !== parentRow) {
            const prevId = row.previousElementSibling?.querySelector('[data-session-group-child]')?.getAttribute('data-session-group-child');
            expect(prevId).toBeTruthy();
            expect(specs.find((entry) => entry.id === prevId)?.parentSessionId).toBe(spec.parentSessionId);
          }
        }
      }
      expect(errors.mock.calls.flat().join(' ')).not.toMatch(/same key|unique.*key/i);
      errors.mockRestore();
      const audienceReads = requests.filter((url) => url.searchParams.get('include') === 'audience');
      for (const check of audienceChecks) {
        expect(check.rendered, `audience was read for a row that was not rendered at request time: ${check.id}`).toBe(true);
      }
      for (const request of audienceReads) {
        for (const id of request.searchParams.get('sessionIds')!.split(',')) {
          expect(specs.find((spec) => spec.id === id)?.state).toBe('published');
        }
      }
      if (searchSpec) {
        fireEvent.change(screen.getByRole('searchbox', { name: 'search transcripts' }), { target: { value: 'child' } });
        const match = await screen.findByText('child search match');
        expect(match.closest('[data-grouped-sessions-section]')?.querySelector('.pub-state-text')?.textContent).toBe('published, up to date');
        fireEvent.click(screen.getByRole('button', { name: 'clear search' }));
        list = await screen.findByRole('region', { name: 'sessions' });
      }

      for (const [id, label] of Object.entries(testCase.expectedLabels ?? {})) {
        await waitFor(() => expect(stateLabel(list, id)).toBe(label));
      }

      // Every filter lists exactly as many rows as its count says.
      for (const key of Object.keys(FILTER_LABELS) as CountKey[]) {
        const button = filterButton(list, key);
        fireEvent.click(button);
        expect(button).toHaveAttribute('aria-pressed', 'true');
        loadAll(list);
        expect(rowsOnScreen(list).length, `${key} rows`).toBe(countOf(button));
        assertSessionsStat();
      }

      // A session the selection leaves out, or one the publications read did
      // not return, appears nowhere on the page.
      for (const spec of specs.filter((candidate) => candidate.outsideSelection || candidate.publication === false)) {
        expect(document.body.textContent).not.toContain(spec.id);
      }
    });
  }
});

describe('root page list reads', () => {
  const SPECS: SessionSpec[] = [
    { id: 's-new', project: 'alpha', syncStatus: 'new', state: 'unpublished' },
    { id: 's-pub', project: 'beta', syncStatus: 'synced', state: 'published', audience: 2 },
  ];

  it('links each row to its transcript and to its project changes', async () => {
    serve(SPECS);
    render(<HomePage />);
    const list = await screen.findByRole('region', { name: 'sessions' });
    const row = list.querySelector('[data-session-id="s-pub"]')!.closest('tr')!;
    expect(within(row).getByRole('link', { name: 's-pub' })).toHaveAttribute('href', `/projects/${BETA_HASH}/s-pub`);
    expect(within(row).getByRole('link', { name: 'changes in beta-project' })).toHaveAttribute('href', `/review/${BETA_HASH}`);
  });

  it('reads the list again when the session set changes, not when a live session grows', async () => {
    const requests = serve(SPECS);
    const view = render(<HomePage />);
    await screen.findByRole('region', { name: 'sessions' });
    const syncReads = () => requests.filter((url) => url.pathname === '/api/v1/sync/sessions').length;
    const before = syncReads();

    topics.sessions = { sessions: [makeSession({ id: 'channel-row', project: 'alpha-project', turnCount: 99 })] };
    view.rerender(<HomePage />);
    await new Promise((settle) => setTimeout(settle, 50));
    expect(syncReads()).toBe(before);

    topics.sessions = {
      sessions: [
        makeSession({ id: 'channel-row', project: 'alpha-project', turnCount: 99 }),
        makeSession({ id: 'channel-new', project: 'alpha-project' }),
      ],
    };
    view.rerender(<HomePage />);
    await waitFor(() => expect(syncReads()).toBe(before + 1));
  });

  it('states no collective count when Village cannot name the audience', async () => {
    const requests = serve(SPECS, { audienceStatus: 502 });
    render(<HomePage />);
    const list = await screen.findByRole('region', { name: 'sessions' });
    await waitFor(() =>
      expect(requests.some((url) => url.pathname === '/api/v1/publications' && url.searchParams.get('include') === 'audience')).toBe(true),
    );
    expect(stateLabel(list, 's-pub')).toBe('published, up to date');
  });

  it('fails closed with the selection guidance when the publications read cannot apply the saved selection', async () => {
    serve(SPECS, {
      publicationsError: { status: 500, code: 'selection_visibility', error: 'failed to read the publication state: saved selection could not be applied' },
    });
    render(<HomePage />);
    expect(await screen.findByText(/peasant kickstart/)).toBeInTheDocument();
    expect(screen.queryByRole('region', { name: 'sessions' })).not.toBeInTheDocument();
    expect(document.body.textContent).not.toContain('s-pub');
    expect(screen.getByRole('button', { name: 'retry the session list' })).toBeInTheDocument();
  });

  it('shows each stats-strip value from its own source', async () => {
    topics.dashboard = { totalSessions: 1284, totalTokens: 0, avgDurationMins: 0, avgTurnsPerSession: 0, acceptanceRate: 0, harnessBreakdown: null };
    topics.trends = {
      days: [
        { date: '2025-01-06', sessions: 2, tokens: 0 },
        { date: '2025-01-14', sessions: 1, tokens: 0 },
        { date: '2025-01-22', sessions: 4, tokens: 0 },
      ],
      totalSessions: 7,
      totalTokens: 0,
    };
    topics.quality = { sessions: [{ id: 'q1', durationMinutes: 12 }, { id: 'q2', durationMinutes: 40 }, { id: 'q3', durationMinutes: 34 }] };
    serve(SPECS);
    render(<HomePage />);
    await screen.findByRole('region', { name: 'sessions' });
    const strip = await screen.findByRole('list', { name: 'your sessions in numbers' });
    await waitFor(() =>
      expect(within(strip).getAllByRole('listitem').map((item) => item.textContent)).toEqual([
        '2 sessions', // selection-visible top-level roots
        '2 projects', // GET /api/v1/projects/summary
        '1 published', // the publications read
        '0 this week', // trends: no day this week
        'longest streak 3 wk', // trends: three weeks in a row
        'median session 34m', // quality: the middle duration
      ]),
    );
    expect(screen.getByText(/of/, { selector: 'header p' })).toHaveTextContent('1 of 2 published. the rest stay on this machine.');
  });

  it('searches through the grouped search route and shows the match and the publish state', async () => {
    const search = {
      items: [
        {
          kind: 'transcript',
          transcript: {
            session: makeSession({ id: 's-pub', project: 'beta-project', projectHash: BETA_HASH }),
            matches: [{ entryIndex: 3, project: 'beta-project', projectHash: BETA_HASH, role: 'user', score: 1, sessionId: 's-pub', snippet: 'the ingest test flakes on CI' }],
          },
          helperGroups: [],
        },
      ],
      page: 1,
      limit: 20,
      totalItems: 1,
      ordinarySessionTotal: 1,
      helperThreadTotal: 0,
    };
    const requests = serve(SPECS, { search });
    render(<HomePage />);
    await screen.findByRole('region', { name: 'sessions' });
    fireEvent.change(screen.getByRole('searchbox', { name: 'search transcripts' }), { target: { value: 'flakes' } });

    const results = await screen.findByText('search results');
    const section = results.closest('[data-grouped-sessions-section]') as HTMLElement;
    expect(await within(section).findByText('the ingest test flakes on CI')).toBeInTheDocument();
    expect(within(section).getByText('you:', { exact: false })).toBeInTheDocument();
    expect(section.querySelector('.pub-state-text')?.textContent).toBe('published, up to date');
    expect(screen.queryByRole('region', { name: 'sessions' })).not.toBeInTheDocument();
    const searched = requests.find((url) => url.pathname === '/api/v1/search');
    expect(searched?.searchParams.get('q')).toBe('flakes');
    expect(searched?.searchParams.get('view')).toBe('grouped');

    fireEvent.click(screen.getByRole('button', { name: 'clear search' }));
    expect(await screen.findByRole('region', { name: 'sessions' })).toBeInTheDocument();
  });
});

const tipRoot = requireRecord(parseStrictYAML(readFileSync(resolve(process.cwd(), 'src/app/testdata/root-auto-publish-tip.yaml'), 'utf8'), 'root automatic publishing tip'), 'root automatic publishing tip');
requireExactRequiredFields(tipRoot, ['requiredNames', 'cases'], 'root automatic publishing tip');
const requiredTipNames = ['home-guidance-can-be-dismissed-without-changing-the-transcript-exit'];
if (!Array.isArray(tipRoot.requiredNames) || [...tipRoot.requiredNames].sort().join() !== requiredTipNames.sort().join() || !Array.isArray(tipRoot.cases)) throw new Error('automatic publishing tip fixture requires its named mounted case');
const tipCases = tipRoot.cases.map((row) => requireRecord(row, 'root automatic publishing tip case'));
requireUniqueNames(tipCases, 'root automatic publishing tip cases');
if (tipCases.map((row) => row.name).sort().join() !== requiredTipNames.sort().join()) throw new Error('automatic publishing tip cases differ from the required names');
for (const row of tipCases) {
  requireExactRequiredFields(row, ['name', 'session', 'copy', 'command', 'dismiss'], 'root automatic publishing tip case');
  const spec = requireRecord(row.session, 'automatic publishing tip session') as unknown as SessionSpec;
  it(String(row.name), async () => {
    const requests = serve([spec]);
    render(<HomePage />);
    const list = await screen.findByRole('region', { name: 'sessions' });
    const tip = screen.getByRole('complementary', { name: 'auto-publish tip' });
    expect(tip.textContent?.replace(/\s+/g, ' ').trim()).toBe(row.copy);
    expect(tip.querySelector('code')).toHaveTextContent(String(row.command));
    expect(tip.querySelector('[data-brand="claude"]')).not.toBeNull();
    const exit = within(list).getByRole('link', { name: spec.id });
    expect(exit).toHaveAttribute('href', `/projects/${PROJECTS[spec.project].hash}/${spec.id}`);
    fireEvent.click(within(tip).getByRole('button', { name: String(row.dismiss) }));
    expect(screen.queryByRole('complementary', { name: 'auto-publish tip' })).not.toBeInTheDocument();
    expect(exit).toHaveAttribute('href', `/projects/${PROJECTS[spec.project].hash}/${spec.id}`);
    expect(requests.every((url) => !url.pathname.startsWith('/api/v1/settings'))).toBe(true);
  });
}

interface AudienceRefreshCase {
  name: string;
  initialCount: number;
  initialStatus: number;
  nextCount: number;
  expectedBefore: string;
  expectedAfter: string;
}

function loadAudienceRefreshCases(): AudienceRefreshCase[] {
  const source = readFileSync(resolve(process.cwd(), 'src/app/testdata/root-audience-refresh.yaml'), 'utf8');
  const root = requireRecord(parseStrictYAML(source, 'root audience refresh'), 'root audience refresh');
  requireExactRequiredFields(root, ['requiredNames', 'cases'], 'root audience refresh');
  const required = [
    'a-root-refresh-updates-the-audience-of-the-same-session',
    'a-root-refresh-retries-a-failed-audience-read-for-the-same-session',
  ];
  if (!Array.isArray(root.requiredNames) || [...root.requiredNames].sort().join() !== [...required].sort().join()) {
    throw new Error('root audience refresh required names changed');
  }
  if (!Array.isArray(root.cases)) throw new Error('root audience refresh cases must be a list');
  const cases = root.cases.map((value, index) => {
    const where = `root audience refresh case ${index}`;
    const row = requireRecord(value, where);
    requireExactRequiredFields(row, ['name', 'initialCount', 'initialStatus', 'nextCount', 'expectedBefore', 'expectedAfter'], where);
    for (const key of ['initialCount', 'nextCount']) {
      if (!Number.isSafeInteger(row[key]) || Number(row[key]) < 0) throw new Error(`${where}.${key} is not an audience count`);
    }
    if (![200, 502].includes(Number(row.initialStatus))) throw new Error(`${where}.initialStatus is invalid`);
    if (typeof row.expectedBefore !== 'string' || typeof row.expectedAfter !== 'string') throw new Error(`${where} lacks expected copy`);
    return row;
  });
  requireUniqueNames(cases, 'root audience refresh cases');
  if (cases.map((row) => row.name).sort().join() !== [...required].sort().join()) throw new Error('root audience refresh case names changed');
  return cases as unknown as AudienceRefreshCase[];
}

describe('root publication refresh also refreshes visible audience counts', () => {
  for (const testCase of loadAudienceRefreshCases()) {
    it(testCase.name, async () => {
      const specs: SessionSpec[] = [{ id: 'same-published-session', project: 'alpha', syncStatus: 'synced', state: 'published', audience: testCase.initialCount }];
      const options: ServeOptions = { audienceStatus: testCase.initialStatus === 200 ? undefined : testCase.initialStatus };
      const requests = serve(specs, options);
      const audienceReads = () => requests.filter((url) => url.pathname === '/api/v1/publications' && url.searchParams.get('include') === 'audience');
      const syncReads = () => requests.filter((url) => url.pathname === '/api/v1/sync/sessions').length;
      const view = render(<HomePage />);
      const list = await screen.findByRole('region', { name: 'sessions' });
      await waitFor(() => expect(audienceReads()).toHaveLength(1));
      await waitFor(() => expect(stateLabel(list, specs[0].id)).toBe(testCase.expectedBefore));
      const originalSyncReads = syncReads();
      // The WS set changes while the actual published REST row remains the
      // same session. This executes the production root refresh boundary.
      specs[0].audience = testCase.nextCount;
      options.audienceStatus = undefined;
      topics.sessions = { sessions: [makeSession({ id: 'new-channel-row', project: 'alpha-project' })] };
      await act(async () => { view.rerender(<HomePage />); });
      await waitFor(() => expect(syncReads()).toBeGreaterThan(originalSyncReads));
      await waitFor(() => expect(audienceReads()).toHaveLength(2));
      await waitFor(() => expect(stateLabel(list, specs[0].id)).toBe(testCase.expectedAfter));
      expect(rowsOnScreen(list)).toEqual([specs[0].id]);
      for (const request of audienceReads()) expect(request.searchParams.get('sessionIds')).toBe(specs[0].id);
    });
  }
});
