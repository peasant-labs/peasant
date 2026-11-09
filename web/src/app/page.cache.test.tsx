import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { parse } from 'yaml';
import HomePage from './page';
import { PublishProvider } from '@/contexts/PublishContext';
import { zLocalSyncProjectOptions, type LocalSyncProjectOption } from '@peasant-labs/schema';
import { chooseProject, chooseStatus, filtersTrigger, projectChoice } from '@/test/homeMenus';
import { decodeHomeViewState, homeCacheKey } from '@/lib/home/viewState';

vi.mock('next/navigation', () => ({ useRouter: () => ({ push: vi.fn() }) }));
vi.mock('@/contexts/WebSocketContext', () => ({ useChannel: () => { throw new Error('Home must not subscribe to full-library channels'); } }));

interface PageEntry {
  ids?: string[];
  nextIds?: string[];
  total?: number;
  revision?: string;
  servedPage?: number;
  status?: number;
  code?: string;
  failOnce?: { status: number; code: string };
  deferred?: boolean;
}
interface Case {
  name: string;
  search?: string;
  pages: Record<string, PageEntry>;
  children?: Record<string, Record<string, PageEntry>>;
  refreshed?: Record<string, PageEntry>;
  resized?: Record<string, PageEntry>;
  deferOptions?: boolean;
  optionsError?: { status: number; code: string };
  rejectRestart?: boolean;
}
const fixture = parse(readFileSync(resolve(process.cwd(), 'src/app/testdata/home-page-cache.yaml'), 'utf8')) as { requiredNames: string[]; projects: LocalSyncProjectOption[]; cases: Case[] };
const required = [
  'page-offset-and-next-items',
  'child-page-offset',
  'revision-change-clears-cache',
  'scope-error-restarts-once',
  'generic400-does-not-restart',
  'project-options-do-not-block-rows',
  'project-filter-aborts-old-continuation',
  'late-options-cannot-win-filter-reset',
  'account-reset-reloads-project-options',
  'project-options-error-retains-rows-and-retries',
  'project-options-selection-error-retains-rows-and-retries',
  'beyond-last-clamps-and-rewrites-url',
  'page-size-resets-to-one',
  'omitted-view-serves-home-page',
  'empty-store-page-one-total-zero',
];
expect([...fixture.requiredNames].sort()).toEqual([...required].sort());
expect(fixture.cases.map(row => row.name).sort()).toEqual([...required].sort());
const projects = zLocalSyncProjectOptions.parse({ projects: fixture.projects }).projects;
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });

beforeEach(() => { window.history.replaceState({}, '', '/'); window.sessionStorage.clear(); window.localStorage.clear(); });
afterEach(() => { cleanup(); vi.unstubAllGlobals(); window.history.replaceState({}, '', '/'); window.sessionStorage.clear(); window.localStorage.clear(); });

describe('mounted Home offset pages', () => {
  for (const testCase of fixture.cases) it(testCase.name, async () => {
    if (testCase.search) window.history.replaceState({}, '', `/?${testCase.search}`);
    const requests: URL[] = [];
    const signals: AbortSignal[] = [];
    const attempts = new Map<string, number>();
    const releases: (() => void)[] = [];
    const optionSignals: AbortSignal[] = [];
    let fresh = false;
    let username = 'first-account';
    let options = projects;
    let optionsRead = 0;
    const row = (id: string) => ({
      sync: {
        id,
        harness: 'claude-code',
        projectName: fresh ? 'beta' : 'alpha',
        projectHash: projects[fresh ? 1 : 0].projectHash,
        hostSlug: 'fixture-host',
        startTime: '2026-06-01T10:00:00Z',
        durationMs: 60000,
        totalTokens: 10,
        turnCount: 2,
        model: 'fixture',
        syncStatus: 'new',
        previouslyPushed: false,
      },
      publication: { sessionId: id, state: 'unpublished', autoPublish: false, outsideSelection: false },
      title: id,
      preview: '',
      childCount: testCase.children && Object.prototype.hasOwnProperty.call(testCase.children, id) ? 1 : 0,
    });
    const homeReads = () => requests.filter(url => url.searchParams.get('view') === 'home');
    const pageOf = (url: URL) => Number(url.searchParams.get('page') ?? '1');
    const rootReads = () => homeReads().filter(url => !url.searchParams.has('parentSessionId'));
    const childReads = () => homeReads().filter(url => url.searchParams.has('parentSessionId'));

    vi.stubGlobal('fetch', vi.fn(async (input: string, init?: RequestInit) => {
      const url = new URL(input, 'http://localhost');
      requests.push(url);
      if (url.pathname === '/api/v1/sync/auth') return json({ authenticated: true, username, villageUrl: 'https://village.example' });
      expect(url.pathname).toBe('/api/v1/sync/sessions');
      if (url.searchParams.get('view') === 'project-options') {
        optionsRead++;
        optionSignals.push(init?.signal as AbortSignal);
        if (testCase.optionsError && optionsRead === 1) return json({ error: 'project choices could not be read; retry', code: testCase.optionsError.code }, testCase.optionsError.status);
        if (testCase.deferOptions && optionsRead === 1) {
          const snapshot = options;
          return new Promise<Response>(done => releases.push(() => done(json({ projects: snapshot }))));
        }
        return json({ projects: options });
      }
      expect(url.searchParams.get('view')).toBe('home');
      expect(url.searchParams.has('pageToken')).toBe(false);
      expect(url.searchParams.has('offset')).toBe(false);
      expect(url.searchParams.has('limit')).toBe(false);
      signals.push(init?.signal as AbortSignal);
      const parent = url.searchParams.get('parentSessionId');
      if (parent) { expect(url.searchParams.has('projectHash')).toBe(false); expect(url.searchParams.has('publication')).toBe(false); }
      const page = pageOf(url);
      const size = Number(url.searchParams.get('maxItems') ?? '25');
      const source = parent
        ? testCase.children?.[parent]
        : size !== 25 && testCase.resized
          ? testCase.resized
          : fresh
            ? testCase.refreshed ?? testCase.pages
            : testCase.pages;
      const entry = source?.[String(page)];
      if (!entry) throw new Error(`Unexpected page ${page} for ${parent ?? 'root'}`);
      const key = `${parent ?? ''}:${page}:${fresh}:${size}`;
      const attempt = (attempts.get(key) ?? 0) + 1;
      attempts.set(key, attempt);
      if (testCase.rejectRestart && entry.failOnce && attempt === 1) return json({ error: 'scope changed; restart', code: entry.failOnce.code }, entry.failOnce.status);
      if (entry.status) return json({ error: 'page refused', code: entry.code }, entry.status);
      if (entry.failOnce && attempt === 1) return json({ error: 'page refused; retry the same page', code: entry.failOnce.code }, entry.failOnce.status);
      const body = {
        items: (entry.ids ?? []).map(row),
        nextItems: (entry.nextIds ?? []).map(row),
        page: entry.servedPage ?? page,
        perPage: size,
        total: entry.total ?? 0,
        revision: entry.revision ?? '1',
      };
      if (entry.deferred) return new Promise<Response>(done => releases.push(() => done(json(body))));
      return json(body);
    }));

    render(<PublishProvider><HomePage /></PublishProvider>);
    const idle = () => waitFor(() => expect(screen.getByRole('region', { name: 'sessions' })).not.toHaveAttribute('aria-busy'));
    const currentPage = (label: string) => waitFor(() => expect(screen.getByRole('button', { name: label })).toHaveAttribute('aria-current', 'page'));

    if (testCase.name === 'page-offset-and-next-items') {
      await screen.findByRole('link', { name: 'root-one' });
      await currentPage('page 1');
      expect(rootReads().map(pageOf)).toEqual([1]);
      fireEvent.click(screen.getByRole('button', { name: 'next page' }));
      await screen.findByRole('link', { name: 'root-two' });
      await currentPage('page 2');
      expect(rootReads().map(pageOf)).toEqual([1, 2]);
      // Going back to a page read in this tab is warm: no request.
      fireEvent.click(screen.getByRole('button', { name: 'page 1' }));
      await screen.findByRole('link', { name: 'root-one' });
      expect(rootReads()).toHaveLength(2);
      return;
    }
    if (testCase.name === 'child-page-offset') {
      await screen.findByRole('link', { name: 'root-one' });
      fireEvent.click(screen.getByRole('button', { name: 'subagents 1' }));
      await screen.findByRole('link', { name: 'child-one' });
      fireEvent.click(screen.getByRole('button', { name: 'load more subagents' }));
      await screen.findByRole('link', { name: 'child-two' });
      expect(childReads().map(pageOf)).toEqual([1, 2]);
      return;
    }
    if (testCase.name === 'revision-change-clears-cache') {
      await screen.findByRole('link', { name: 'root-one' });
      fireEvent.click(screen.getByRole('button', { name: 'next page' }));
      await screen.findByRole('link', { name: 'root-two' });
      fresh = true;
      fireEvent(window, new Event('focus'));
      await screen.findByRole('link', { name: 'fresh-root-two' });
      const before = rootReads().length;
      // The new revision cleared every cached page, so page one is a cold read
      // again even though it was read in this tab.
      fireEvent.click(screen.getByRole('button', { name: 'page 1' }));
      await screen.findByRole('link', { name: 'fresh-root-one' });
      expect(rootReads().length).toBe(before + 1);
      expect(rootReads().at(-1)?.searchParams.get('page')).toBe('1');
      return;
    }
    if (testCase.name === 'scope-error-restarts-once') {
      await screen.findByRole('link', { name: 'root-one' });
      fireEvent.click(screen.getByRole('button', { name: 'next page' }));
      await screen.findByRole('link', { name: 'root-two' });
      await currentPage('page 2');
      // One refusal, then the same page restarts exactly once.
      expect(rootReads().map(pageOf)).toEqual([1, 2, 2]);
      return;
    }
    if (testCase.name === 'generic400-does-not-restart') {
      await screen.findByRole('link', { name: 'root-one' });
      fireEvent.click(screen.getByRole('button', { name: 'next page' }));
      await screen.findByRole('button', { name: 'retry the session list' });
      // A generic refusal is a fault, not a scope restart.
      expect(rootReads().map(pageOf)).toEqual([1, 2]);
      expect(screen.queryByRole('link', { name: 'root-one' })).not.toBeInTheDocument();
      return;
    }
    if (testCase.name === 'project-options-do-not-block-rows') {
      await screen.findByRole('link', { name: 'root-one' });
      await waitFor(() => expect(releases).toHaveLength(1));
      expect(screen.getByRole('link', { name: 'root-one' })).toBeInTheDocument();
      expect(projectChoice('beta')).toBeNull();
      await act(async () => { releases[0](); });
      await waitFor(() => expect(projectChoice('beta')).not.toBeNull());
      return;
    }
    if (testCase.name === 'project-filter-aborts-old-continuation') {
      await screen.findByRole('link', { name: 'root-one' });
      await waitFor(() => expect(projectChoice('beta')).not.toBeNull());
      fireEvent.click(screen.getByRole('button', { name: 'next page' }));
      await waitFor(() => expect(releases).toHaveLength(1));
      const oldSignal = signals.at(-1)!;
      fresh = true;
      await chooseProject('beta');
      await screen.findByRole('link', { name: 'fresh-root-one' });
      expect(oldSignal.aborted).toBe(true);
      await act(async () => { releases[0](); });
      expect(screen.queryByRole('link', { name: 'old-off-page' })).not.toBeInTheDocument();
      expect(rootReads().at(-1)?.searchParams.get('projectHash')).toBe(projects[1].projectHash);
      return;
    }
    if (testCase.name === 'late-options-cannot-win-filter-reset') {
      await screen.findByRole('link', { name: 'root-one' });
      await waitFor(() => expect(releases).toHaveLength(1));
      options = [projects[1]];
      await chooseStatus('auto-publish on');
      await waitFor(() => expect(projectChoice('beta')).not.toBeNull());
      await act(async () => { releases[0](); });
      expect(optionSignals[0].aborted).toBe(true);
      expect(projectChoice('alpha')).toBeNull();
      return;
    }
    if (testCase.name === 'account-reset-reloads-project-options') {
      await screen.findByRole('link', { name: 'root-one' });
      await waitFor(() => expect(projectChoice('alpha')).not.toBeNull());
      username = 'second-account';
      options = [projects[1]];
      fresh = true;
      fireEvent(window, new Event('focus'));
      await screen.findByRole('link', { name: 'fresh-root-one' });
      await waitFor(() => expect(projectChoice('beta')).not.toBeNull());
      await waitFor(() => expect(projectChoice('alpha')).toBeNull());
      expect(screen.queryByRole('link', { name: 'root-one' })).not.toBeInTheDocument();
      return;
    }
    if (testCase.name === 'project-options-error-retains-rows-and-retries' || testCase.name === 'project-options-selection-error-retains-rows-and-retries') {
      await screen.findByRole('link', { name: 'root-one' });
      await waitFor(() => expect(screen.getByRole('menuitem', { name: 'retry project choices', hidden: true })).toBeInTheDocument());
      expect(screen.getByRole('link', { name: 'root-one' })).toBeInTheDocument();
      fireEvent.click(filtersTrigger());
      fireEvent.click(screen.getByRole('menuitem', { name: 'retry project choices' }));
      await waitFor(() => expect(projectChoice('beta')).not.toBeNull());
      expect(optionsRead).toBe(2);
      return;
    }
    if (testCase.name === 'beyond-last-clamps-and-rewrites-url') {
      await screen.findByRole('link', { name: 'root-last' });
      await currentPage('page 2');
      // The server clamped page 99 to page 2, and the host rewrote the URL.
      expect(new URLSearchParams(window.location.search).get('page')).toBe('2');
      expect(rootReads().map(pageOf)).toEqual([99]);
      // The clamped page is the one in hand: no second request for it.
      expect(rootReads().map(pageOf)).not.toContain(2);
      return;
    }
    if (testCase.name === 'page-size-resets-to-one') {
      await screen.findByRole('link', { name: 'root-one' });
      fireEvent.click(screen.getByRole('button', { name: 'next page' }));
      await screen.findByRole('link', { name: 'root-two' });
      fireEvent.change(screen.getByRole('combobox', { name: 'sessions per page' }), { target: { value: '50' } });
      await screen.findByRole('link', { name: 'root-one' });
      await currentPage('page 1');
      const last = rootReads().at(-1)!;
      expect(last.searchParams.get('maxItems')).toBe('50');
      expect(last.searchParams.get('page')).toBe('1');
      const search = new URLSearchParams(window.location.search);
      expect(search.get('perPage')).toBe('50');
      expect(search.get('page')).toBeNull();
      return;
    }
    if (testCase.name === 'omitted-view-serves-home-page') {
      await screen.findByRole('link', { name: 'root-one' });
      const first = rootReads()[0];
      expect(first.searchParams.get('view')).toBe('home');
      expect(first.searchParams.get('page')).toBe('1');
      expect(first.searchParams.get('maxItems')).toBe('25');
      expect(first.searchParams.has('browse')).toBe(false);
      expect(first.searchParams.has('publication')).toBe(false);
      expect(window.location.search).toBe('');
      return;
    }
    if (testCase.name === 'empty-store-page-one-total-zero') {
      await screen.findByText('no ai work recorded yet');
      expect(screen.getByRole('link', { name: /^how to record sessions/ })).toBeInTheDocument();
      expect(screen.queryByRole('button', { name: 'next page' })).not.toBeInTheDocument();
      expect(screen.queryByRole('button', { name: 'page 1' })).not.toBeInTheDocument();
      await idle();
      return;
    }
    throw new Error(`No mounted steps for ${testCase.name}`);
  });
});

describe('grouping changes', () => {
  it('a group change resets to page one and clears the open groups but keeps the columns', async () => {
    const homeReads: URL[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: string) => {
      const url = new URL(input, 'http://localhost');
      if (url.pathname === '/api/v1/sync/auth') return json({ authenticated: true, username: 'first-account', villageUrl: 'https://village.example' });
      if (url.searchParams.get('view') === 'project-options') return json({ projects: [fixture.projects[0]] });
      const page = Number(url.searchParams.get('page') ?? '1');
      const size = Number(url.searchParams.get('maxItems') ?? '25');
      homeReads.push(url);
      const row = (id: string) => ({
        sync: {
          id, harness: 'claude-code', projectName: 'alpha', projectHash: fixture.projects[0].projectHash, hostSlug: 'fixture-host',
          startTime: '2026-06-01T10:00:00Z', durationMs: 60000, totalTokens: 10, turnCount: 2, model: 'fixture', syncStatus: 'new', previouslyPushed: false,
        },
        publication: { sessionId: id, state: 'unpublished', autoPublish: false, outsideSelection: false },
        title: id, preview: '', childCount: 0,
      });
      return json({
        items: Array.from({ length: size }, (_, index) => row(`root-${(page - 1) * size + index}`)),
        nextItems: [], page, perPage: size, total: 60, revision: '0',
      });
    }));
    window.localStorage.setItem('peasant.home', JSON.stringify({ visibleColumns: ['session', 'state'], open: ['project:alpha'] }));
    render(<PublishProvider><HomePage /></PublishProvider>);
    await screen.findByRole('link', { name: 'root-0' });
    fireEvent.click(screen.getByRole('button', { name: 'next page' }));
    await screen.findByRole('link', { name: 'root-25' });
    expect(new URLSearchParams(window.location.search).get('page')).toBe('2');

    fireEvent.click(screen.getByRole('button', { name: 'group by: project' }));
    fireEvent.click(screen.getByRole('menuitemradio', { name: 'none' }));
    await waitFor(() => expect(new URLSearchParams(window.location.search).get('groupBy')).toBe('none'));
    // A grouping change starts at page one; the page was already in the tab.
    expect(new URLSearchParams(window.location.search).get('page')).toBeNull();
    await screen.findByRole('link', { name: 'root-0' });
    // The open groups are device state and the new grouping names different
    // groups, so the open set clears; the columns stay.
    const stored = JSON.parse(window.localStorage.getItem('peasant.home') ?? '{}');
    expect(stored.open).toEqual([]);
    expect(stored.visibleColumns).toEqual(['session', 'state']);
    expect(homeReads.map(url => url.searchParams.get('page'))).toEqual(['1', '2']);
  });
});

describe('tab cache', () => {
  it('a cold load renders the cached page then makes one quiet revalidation request', async () => {
    window.history.replaceState({}, '', '/?page=3');
    const auth = { authenticated: true, username: 'first-account', villageUrl: 'https://village.example' };
    const state = decodeHomeViewState('?page=3');
    const key = homeCacheKey(state, JSON.stringify(auth));
    const row = (id: string) => ({
      sync: {
        id, harness: 'claude-code', projectName: 'alpha', projectHash: fixture.projects[0].projectHash, hostSlug: 'fixture-host',
        startTime: '2026-06-01T10:00:00Z', durationMs: 60000, totalTokens: 10, turnCount: 2, model: 'fixture', syncStatus: 'new', previouslyPushed: false,
      },
      publication: { sessionId: id, state: 'unpublished', autoPublish: false, outsideSelection: false },
      title: id, preview: '', childCount: 0,
    });
    window.sessionStorage.setItem(`peasant.home.page:${key}`, JSON.stringify({
      items: [row('cached-root')], nextItems: [], page: 3, perPage: 25, total: 75, revision: '0',
    }));
    const reads: URL[] = [];
    let release!: () => void;
    const held = new Promise<void>(done => { release = done; });
    vi.stubGlobal('fetch', vi.fn(async (input: string) => {
      const url = new URL(input, 'http://localhost');
      if (url.pathname === '/api/v1/sync/auth') return json(auth);
      if (url.searchParams.get('view') === 'project-options') return json({ projects: [fixture.projects[0]] });
      reads.push(url);
      await held;
      return json({ items: [row('fresh-root')], nextItems: [], page: 3, perPage: 25, total: 75, revision: '0' });
    }));
    const view = render(<PublishProvider><HomePage /></PublishProvider>);
    // The cached page is on screen while the one revalidation read is in flight,
    // and the reader sees no loading skeleton over it.
    await screen.findByRole('link', { name: 'cached-root' });
    expect(reads).toHaveLength(1);
    expect(screen.queryByLabelText('Loading your sessions')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'page 3' })).toHaveAttribute('aria-current', 'page');
    // A re-render in the same tab is warm.
    release();
    await screen.findByRole('link', { name: 'fresh-root' });
    expect(reads).toHaveLength(1);
    // A warm re-render in the same tab makes no request.
    view.rerender(<PublishProvider><HomePage /></PublishProvider>);
    await new Promise(done => setTimeout(done, 0));
    expect(reads).toHaveLength(1);
  });
});

describe('history', () => {
  it('back and forward restore the prior view and page from the URL with no request', async () => {
    const reads: URL[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: string) => {
      const url = new URL(input, 'http://localhost');
      if (url.pathname === '/api/v1/sync/auth') return json({ authenticated: true, username: 'first-account', villageUrl: 'https://village.example' });
      if (url.searchParams.get('view') === 'project-options') return json({ projects: [fixture.projects[0]] });
      reads.push(url);
      const page = Number(url.searchParams.get('page') ?? '1');
      const size = Number(url.searchParams.get('maxItems') ?? '25');
      const row = (id: string) => ({
        sync: {
          id, harness: 'claude-code', projectName: 'alpha', projectHash: fixture.projects[0].projectHash, hostSlug: 'fixture-host',
          startTime: '2026-06-01T10:00:00Z', durationMs: 60000, totalTokens: 10, turnCount: 2, model: 'fixture', syncStatus: 'new', previouslyPushed: false,
        },
        publication: { sessionId: id, state: 'unpublished', autoPublish: false, outsideSelection: false },
        title: id, preview: '', childCount: 0,
      });
      return json({
        items: Array.from({ length: size }, (_, index) => row(`root-${(page - 1) * size + index}`)),
        nextItems: [], page, perPage: size, total: 60, revision: '0',
      });
    }));
    render(<PublishProvider><HomePage /></PublishProvider>);
    await screen.findByRole('link', { name: 'root-0' });
    fireEvent.click(screen.getByRole('button', { name: 'next page' }));
    await screen.findByRole('link', { name: 'root-25' });
    expect(reads).toHaveLength(2);

    // Back: the prior URL names page one, and the tab already holds it.
    window.history.replaceState({}, '', '/');
    fireEvent(window, new PopStateEvent('popstate'));
    await screen.findByRole('link', { name: 'root-0' });
    await waitFor(() => expect(screen.getByRole('button', { name: 'page 1' })).toHaveAttribute('aria-current', 'page'));

    // Forward: the next URL names page two, also already in this tab.
    window.history.replaceState({}, '', '/?page=2');
    fireEvent(window, new PopStateEvent('popstate'));
    await screen.findByRole('link', { name: 'root-25' });
    await waitFor(() => expect(screen.getByRole('button', { name: 'page 2' })).toHaveAttribute('aria-current', 'page'));
    expect(reads).toHaveLength(2);
  });
});
