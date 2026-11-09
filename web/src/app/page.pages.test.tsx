import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { parse } from 'yaml';
import HomePage from './page';
import { PublishProvider, usePublishState } from '@/contexts/PublishContext';
import { newProjectHash, zSyncPushRequest, zSyncPushResponse } from '@peasant-labs/schema';
import { ALPHA_HASH, BETA_HASH, makeSession } from '@/app/review/[[...segments]]/test-fixtures';
import type { LocalSyncSessionRow } from '@/lib/api/home';
import { decodeHomeViewState } from '@/lib/home/viewState';
import { browseBy, browseTrigger, chooseStatus, choiceRows } from '@/test/homeMenus';

const channels = vi.hoisted(() => vi.fn<(subscriptions: unknown) => { connected: boolean; data: unknown; error: null }>());
vi.mock('@/contexts/WebSocketContext', () => ({ useChannel: channels }));
vi.mock('next/navigation', () => ({ useRouter: () => ({ push: vi.fn() }) }));

interface Fixture {
  name: string;
  roots: number;
  children: number;
  parentErrorCode?: string;
  cachedSessionId?: string;
  cachedProject?: string;
  accountAfterFailure?: string;
  teammate?: { team: string; name: string };
  assertTeammateLine?: boolean;
  search?: string;
  rows?: { id: string; project: 'alpha' | 'beta' | 'gamma' | 'delta' }[];
  visibleColumns?: string[];
  expectView?: { browse: string; publication: string; project: string | null; perPage: number; page: number; groupBy: string };
}
const fixture = parse(readFileSync(resolve(process.cwd(), 'src/app/testdata/home-pages.yaml'), 'utf8')) as { requiredNames: string[]; cases: Fixture[]; publish: { request: unknown; response: unknown; loadedPages: { parent: string | null; page: number }[]; failedRefreshPages: { parent: string | null; page: number }[] } };
const required = ['numbered-pages-from-the-url', 'loaded-refresh-keeps-content-on-failure', 'stale-filter-response', 'lazy-children-and-child-more', 'account-change-clears-old-rows', 'collapsed-late-child-is-ignored', 'selection-error-keeps-loaded-content', 'overlapping-refresh-does-not-race', 'collapse-cancels-expanded-refresh', 'collapsed-ancestor-skips-descendant-refresh', 'successful-refresh-invalidates-collapsed-family', 'successful-refresh-invalidates-collapsed-descendant', 'canonical-missing-parent-commits-refreshed-roots', 'generic-child404-retains-visible-content', 'publish-settlement-refreshes-loaded-window', 'publish-during-refresh-queues-one-loaded-window', 'selection-error-retry-failure-keeps-loaded-content', 'selection-error-account-change-refreshes-loaded-content', 'claude-teammate-line', 'no-claude-teammate-line', 'refresh-restores-view-and-page', 'interleaved-project-grouping', 'hidden-session-column-grouping', 'collapsed-group-does-not-hide-page-two'];
expect([...fixture.requiredNames].sort()).toEqual([...required].sort());
expect(fixture.cases.map(row => row.name).sort()).toEqual([...required].sort());

// The mounted capture and this component suite read one fixture. Keep the
// visual harness identity and the case identity tied to the same value so a
// one-sided edit cannot publish a screenshot that proves a different name.
const visualTeammate = (fixture as { visual?: { teammate?: { team: string; name: string } } }).visual?.teammate;
if (!visualTeammate) throw new Error('home-pages.yaml visual.teammate must name the identity the mounted capture serves');
expect(fixture.cases.find(row => row.name === 'claude-teammate-line')?.teammate).toEqual({ team: visualTeammate.team, name: visualTeammate.name });

/** One fixture row; `teammate` is the identity the fixture case serves on it. */
const FIXTURE_PROJECTS = {
  alpha: { name: 'fixture-project', hash: ALPHA_HASH },
  beta: { name: 'beta-project', hash: BETA_HASH },
  gamma: { name: 'gamma-project', hash: newProjectHash('c'.repeat(64)) },
  delta: { name: 'delta-project', hash: newProjectHash('d'.repeat(64)) },
} as const;

function row(id: string, children = 0, teammate?: { team: string; name: string }, project: keyof typeof FIXTURE_PROJECTS = 'alpha'): LocalSyncSessionRow {
  const projectRow = FIXTURE_PROJECTS[project];
  return {
    sync: {
      id, harness: 'claude-code', projectName: projectRow.name, projectHash: projectRow.hash, hostSlug: 'fixture-host', startTime: '2026-06-01T10:00:00Z', durationMs: BigInt(60000), totalTokens: 10, turnCount: 2, model: 'fixture', syncStatus: 'new', previouslyPushed: false,
      ...(teammate ? { teammate: { team: teammate.team, name: teammate.name, evidence: 'native_dynamic_workflow' as const } } : {}),
    },
    publication: { sessionId: id, state: 'unpublished', autoPublish: false, outsideSelection: false },
    title: `title ${id}`, preview: '', childCount: children,
  };
}
function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>(done => { resolve = done; });
  return { promise, resolve };
}
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body, (_, value) => typeof value === 'bigint' ? Number(value) : value), { status, headers: { 'Content-Type': 'application/json' } });
let requests: URL[];
let fail = false;
let selectionFailure = false;
let account = 'first';
let roots: LocalSyncSessionRow[];
let children: LocalSyncSessionRow[];
let delayed: ReturnType<typeof deferred<Response>> | undefined;
let delayFilter = '';
let delayParent = '';
let signals: AbortSignal[];
let childSources: Map<string, LocalSyncSessionRow[]>;
let parentFailure: string | undefined;
let pushReply: ReturnType<typeof deferred<Response>>;
let holdPages: boolean;
let releasePages: (() => void)[];
let revision: string;
const pageRequests = () => requests.filter(url => url.pathname === '/api/v1/sync/sessions' && url.searchParams.get('view') === 'home');
const pagesOf = (urls: URL[]) => urls.map(url => ({ parent: url.searchParams.get('parentSessionId'), page: Number(url.searchParams.get('page') ?? '1') }));
function PublishAction() {
  const { publish, settled } = usePublishState();
  return <><button onClick={() => { void publish('root-0', zSyncPushRequest.parse(fixture.publish.request), 0).catch(() => {}); }}>publish fixture session</button><output aria-label="publish completed">{settled.get('root-0') ?? 0}</output></>;
}
function mount() { return render(<PublishProvider><HomePage /><PublishAction /></PublishProvider>); }

beforeEach(() => {
  requests = []; signals = []; fail = false; selectionFailure = false; account = 'first'; delayed = undefined; delayFilter = ''; delayParent = '';
  window.history.replaceState({}, '', '/'); window.sessionStorage.clear(); window.localStorage.clear(); revision = '0';
  channels.mockReset();
  channels.mockReturnValue({ connected: true, data: { sessions: [] }, error: null });
  childSources = new Map(); parentFailure = undefined; pushReply = deferred<Response>(); holdPages = false; releasePages = [];
  vi.stubGlobal('fetch', vi.fn(async (input: string, init?: RequestInit) => {
    const url = new URL(input, 'http://localhost');
    requests.push(url);
    if (url.pathname === '/api/v1/sync/auth') return json({ authenticated: true, username: account, villageUrl: 'https://village.example' });
    if (url.pathname === '/api/v1/publications') return json({ publications: [] });
    if (url.pathname === '/api/v1/sync/push') {
      expect(JSON.parse(String(init?.body))).toEqual(fixture.publish.request);
      return pushReply.promise;
    }
    if (url.pathname === '/api/v1/sync/sessions' && url.searchParams.get('view') === 'project-options') return json({ projects: [{ projectHash: ALPHA_HASH, projectName: 'fixture-project' }] });
    if (url.pathname === '/api/v1/sync/sessions' && url.searchParams.get('view') === 'home') {
      signals.push(init?.signal as AbortSignal);
      if (delayFilter && (url.searchParams.get('publication') ?? 'all') === delayFilter && delayed) return delayed.promise;
      if (delayParent && url.searchParams.get('parentSessionId') === delayParent && delayed) return delayed.promise;
      if (selectionFailure) return json({ error: 'saved selection metadata could not be read', code: 'selection_visibility' }, 500);
      if (fail) return json({ error: 'temporary read failure; retry' }, 503);
      if (parentFailure && url.searchParams.has('parentSessionId')) return json({ error: 'parent read refused; choose a visible parent', code: parentFailure }, 404);
      const page = Number(url.searchParams.get('page') ?? '1');
      const size = Number(url.searchParams.get('maxItems') ?? '25');
      const start = (page - 1) * size;
      const parent = url.searchParams.get('parentSessionId');
      const source = parent ? childSources.get(parent) ?? children : roots;
      const body = {
        items: source.slice(start, start + size),
        nextItems: source.slice(start + size, start + 2 * size),
        page,
        perPage: size,
        total: source.length,
        revision,
      };
      if (holdPages) return new Promise<Response>(done => { releasePages.push(() => done(json(body))); });
      return json(body);
    }
    throw new Error(`Unexpected eager request ${url.pathname}`);
  }));
});
afterEach(() => { cleanup(); vi.unstubAllGlobals(); window.history.replaceState({}, '', '/'); window.sessionStorage.clear(); window.localStorage.clear(); });

describe('mounted bounded Home', () => {
  for (const testCase of fixture.cases) {
    it(testCase.name, async () => {
      if (testCase.search) window.history.replaceState({}, '', `/?${testCase.search}`);
      if (testCase.visibleColumns) window.localStorage.setItem('peasant.home', JSON.stringify({ visibleColumns: testCase.visibleColumns, collapsed: null }));
      const customRows = testCase.rows !== undefined;
      roots = customRows
        ? testCase.rows!.map(entry => row(entry.id, 0, undefined, entry.project))
        : Array.from({ length: testCase.roots }, (_, index) => row(`root-${index}`, index === 0 ? testCase.children : 0));
      children = Array.from({ length: testCase.children }, (_, index) => row(`child-${index}`, index === 0 ? 1 : 0, index === 0 ? testCase.teammate : undefined));
      mount();
      const restoring = testCase.name === 'refresh-restores-view-and-page';
      const hiddenSession = testCase.visibleColumns !== undefined && !testCase.visibleColumns.includes('session');
      if (hiddenSession) {
        await waitFor(() => expect(document.querySelector('[data-home-group]')).not.toBeNull());
      } else {
        await screen.findByRole('link', { name: customRows ? `title ${testCase.rows![0].id}` : restoring ? 'title root-50' : 'title root-0' });
      }
      expect(channels).not.toHaveBeenCalled();
      expect(pageRequests()).toHaveLength(1);
      if (customRows) {
        // The grouping/columns cases do not share the generated-root assertions.
      } else if (restoring) {
        expect(pageRequests()[0].searchParams.get('maxItems')).toBe('50');
        expect(pageRequests()[0].searchParams.get('page')).toBe('2');
      } else {
        expect(pageRequests()[0].searchParams.get('maxItems')).toBe('25');
        expect(pageRequests()[0].searchParams.get('page')).toBe('1');
        expect(screen.queryByRole('link', { name: 'title root-25' })).not.toBeInTheDocument();
        expect(screen.getByRole('link', { name: 'title root-0' }).getAttribute('href')).toContain('root-0');
        expect(choiceRows('status').find(row => row.getAttribute('aria-checked') === 'true')).toHaveTextContent('all');
      }

      if (testCase.assertTeammateLine) {
        fireEvent.click(screen.getByRole('button', { name: 'subagents 1' }));
        await screen.findByRole('link', { name: 'title child-0' });
        const childCell = screen.getByRole('link', { name: 'title child-0' }).closest('.home-session') as HTMLElement;
        if (testCase.teammate) {
          // The line sits under the title and beside the facts that already
          // render: the child title, its agent mark, its token count.
          const line = childCell.querySelector('.home-session-teammate');
          expect(line).toHaveTextContent(`teammate ${testCase.teammate.name} · ${testCase.teammate.team}`);
          expect(childCell.querySelector('.home-session-title')).toHaveTextContent('title child-0');
          expect(childCell.querySelector('.home-session-meta')).toHaveTextContent('tokens');
          // The identity is its own line in the session cell, never folded into
          // the phone block that carries the other columns' facts.
          const phone = childCell.querySelector('.home-session-phone');
          expect(phone).not.toBeNull();
          expect(phone?.querySelector('.home-session-teammate')).toBeNull();
          expect(line?.closest('.home-session-phone')).toBeNull();
        } else {
          expect(document.querySelector('.home-session-teammate')).toBeNull();
        }
        expect(screen.getByRole('link', { name: 'title root-0' })).toBeInTheDocument();
      }
      if (testCase.name === 'numbered-pages-from-the-url') {
        fireEvent.click(screen.getByRole('button', { name: 'next page' }));
        await screen.findByRole('link', { name: 'title root-25' });
        // One page at a time: the first page's rows leave the table.
        expect(screen.queryByRole('link', { name: 'title root-0' })).not.toBeInTheDocument();
        expect(pageRequests().map(url => url.searchParams.get('page'))).toEqual(['1', '2']);
        expect(new URLSearchParams(window.location.search).get('page')).toBe('2');
        // The server's total names the last page (60 roots at 25 per page).
        expect(screen.getByRole('button', { name: 'page 3' })).toBeInTheDocument();
        // A page already read opens without asking the server again.
        fireEvent.click(screen.getByRole('button', { name: 'page 1' }));
        await screen.findByRole('link', { name: 'title root-0' });
        expect(pageRequests()).toHaveLength(2);
        expect(new URLSearchParams(window.location.search).get('page')).toBeNull();
        // A direct jump to a numbered page reads just that page.
        fireEvent.click(screen.getByRole('button', { name: 'page 3' }));
        await screen.findByRole('link', { name: 'title root-50' });
        expect(pageRequests().map(url => url.searchParams.get('page'))).toEqual(['1', '2', '3']);
        expect(new URLSearchParams(window.location.search).get('page')).toBe('3');
      }
      if (testCase.name === 'loaded-refresh-keeps-content-on-failure') {
        fireEvent.click(screen.getByRole('button', { name: 'subagents 30' }));
        await screen.findByRole('link', { name: 'title child-0' });
        fireEvent.click(screen.getByRole('button', { name: 'next page' }));
        await screen.findByRole('link', { name: 'title root-25' });
        const before = pageRequests().length;
        fail = true;
        fireEvent(window, new Event('focus'));
        await screen.findByRole('button', { name: 'retry the session list' });
        // The failed refresh kept the visible page and its loaded family.
        expect(screen.getByRole('link', { name: 'title root-25' })).toBeInTheDocument();
        expect(pagesOf(pageRequests().slice(before))).toEqual(fixture.publish.failedRefreshPages);
        fail = false;
        fireEvent.click(screen.getByRole('button', { name: 'retry the session list' }));
        await waitFor(() => expect(screen.queryByRole('button', { name: 'retry the session list' })).not.toBeInTheDocument());
        // Returning to a page read in this tab stays warm and keeps the family.
        const afterRetry = pageRequests().length;
        fireEvent.click(screen.getByRole('button', { name: 'page 1' }));
        expect(await screen.findByRole('link', { name: 'title child-0' })).toBeInTheDocument();
        expect(pageRequests()).toHaveLength(afterRetry);
      }
      if (testCase.name === 'refresh-restores-view-and-page') {
        // The cold document load restores the view, page, size, and grouping
        // from the URL; columns and open groups never travel in it.
        const decoded = decodeHomeViewState(window.location.search);
        expect({
          browse: decoded.browse.wire,
          publication: decoded.publication,
          project: decoded.project,
          perPage: decoded.pageSize,
          page: decoded.page,
          groupBy: decoded.groupBy.wire,
        }).toEqual(testCase.expectView);
        const first = pageRequests()[0];
        expect(first.searchParams.get('page')).toBe('2');
        expect(first.searchParams.get('maxItems')).toBe('50');
        expect(first.searchParams.get('projectHash')).toBe(testCase.expectView?.project);
        await screen.findByRole('link', { name: 'title root-50' });
        await waitFor(() => expect(screen.getByRole('button', { name: 'page 2' })).toHaveAttribute('aria-current', 'page'));
        expect(screen.queryByRole('link', { name: 'title root-0' })).not.toBeInTheDocument();
        // groupBy=none comes from the URL, so the local list renders no project
        // group header even though the device holds an open set.
        expect(document.querySelector('[data-home-group]')).toBeNull();
      }
      if (testCase.name === 'stale-filter-response') {
        delayed = deferred<Response>(); delayFilter = 'published';
        await chooseStatus('published');
        await waitFor(() => expect(pageRequests().at(-1)?.searchParams.get('publication')).toBe('published'));
        fireEvent.change(screen.getByRole('searchbox', { name: 'search your sessions' }), { target: { value: '' } });
        // Filter controls stay available during a first-page load.
        await chooseStatus('auto-publish on');
        await screen.findByRole('link', { name: 'title root-0' });
        await act(async () => { delayed?.resolve(json({ items: [row('stale')], nextItems: [], page: 1, perPage: 25, total: 1, revision: '0' })); });
        expect(screen.queryByRole('link', { name: 'title stale' })).not.toBeInTheDocument();
        expect(signals.some(signal => signal.aborted)).toBe(true);
      }
      if (testCase.name === 'lazy-children-and-child-more') {
        expect(pageRequests().some(url => url.searchParams.has('parentSessionId'))).toBe(false);
        fireEvent.click(screen.getByRole('button', { name: 'subagents 30' }));
        await screen.findByRole('link', { name: 'title child-0' });
        expect(pageRequests().at(-1)?.searchParams.has('publication')).toBe(false);
        expect(screen.queryByRole('link', { name: 'title child-25' })).not.toBeInTheDocument();
        fireEvent.click(screen.getByRole('button', { name: 'load more subagents' }));
        await screen.findByRole('link', { name: 'title child-25' });
        const before = pageRequests().length;
        fireEvent.click(screen.getByRole('button', { name: 'subagents 30' }));
        fireEvent(window, new Event('focus'));
        await waitFor(() => expect(pageRequests().length).toBe(before + 1));
        expect(pageRequests().at(-1)?.searchParams.has('parentSessionId')).toBe(false);
      }
      if (testCase.name === 'account-change-clears-old-rows') {
        account = 'second';
        roots = [row('new-account')];
        fireEvent(window, new Event('focus'));
        await screen.findByRole('link', { name: 'title new-account' });
        expect(screen.queryByRole('link', { name: 'title root-0' })).not.toBeInTheDocument();
      }
      if (testCase.name === 'collapsed-late-child-is-ignored') {
        delayed = deferred<Response>(); delayParent = 'root-0';
        fireEvent.click(screen.getByRole('button', { name: 'subagents 30' }));
        await waitFor(() => expect(pageRequests()).toHaveLength(2));
        const childSignal = signals.at(-1)!;
        fireEvent.click(screen.getByRole('button', { name: 'subagents 30' }));
        expect(childSignal.aborted).toBe(true);
        await act(async () => { delayed?.resolve(json({ items: children.slice(0,25), nextItems: children.slice(25,50), page: 1, perPage: 25, total: children.length, revision: '0' })); });
        expect(screen.queryByRole('link', { name: 'title child-0' })).not.toBeInTheDocument();
        delayParent = '';
        fireEvent.click(screen.getByRole('button', { name: 'subagents 30' }));
        await screen.findByRole('link', { name: 'title child-0' });
        expect(pageRequests()).toHaveLength(3);
      }
      if (testCase.name === 'selection-error-keeps-loaded-content') {
        selectionFailure = true;
        fireEvent(window, new Event('focus'));
        await screen.findByRole('button', { name: 'retry the session list' });
        // The failed read keeps the loaded continuation and its rows on screen.
        expect(screen.getByRole('link', { name: 'title root-0' })).toBeInTheDocument();
        expect(screen.getByRole('region', { name: 'sessions' })).toBeInTheDocument();
        expect(screen.queryByText(/peasant kickstart/)).not.toBeInTheDocument();
        selectionFailure = false;
        fireEvent.click(screen.getByRole('button', { name: 'retry the session list' }));
        await waitFor(() => expect(screen.queryByRole('button', { name: 'retry the session list' })).not.toBeInTheDocument());
        expect(screen.getByRole('link', { name: 'title root-0' })).toBeInTheDocument();
      }
      if (testCase.name === 'selection-error-retry-failure-keeps-loaded-content') {
        selectionFailure = true;
        fireEvent(window, new Event('focus'));
        await screen.findByRole('button', { name: 'retry the session list' });
        expect(screen.getByRole('link', { name: 'title root-0' })).toBeInTheDocument();

        // A retry that fails for an unrelated reason keeps the same loaded
        // continuation; it does not swap the library for a selection state.
        selectionFailure = false;
        fail = true;
        fireEvent.click(screen.getByRole('button', { name: 'retry the session list' }));
        await waitFor(() => expect(screen.getByText(/temporary read failure/)).toBeInTheDocument());
        expect(screen.getByRole('link', { name: 'title root-0' })).toBeInTheDocument();
        expect(screen.queryByText(/peasant kickstart/)).not.toBeInTheDocument();

        fail = false;
        roots = [row('revalidated-root')];
        fireEvent.click(screen.getByRole('button', { name: 'retry the session list' }));
        await screen.findByRole('link', { name: 'title revalidated-root' });
        expect(screen.queryByRole('link', { name: 'title root-0' })).not.toBeInTheDocument();
      }
      if (testCase.name === 'selection-error-account-change-refreshes-loaded-content') {
        await import('@/components/home/LegacyHomeBrowse');
        channels.mockReturnValue({ connected: true, data: { sessions: [makeSession({ id: 'cached-browse-session', project: 'cached-browse-project' })] }, error: null });
        selectionFailure = true;
        await act(async () => { fireEvent(window, new Event('focus')); });
        await screen.findByRole('button', { name: 'retry the session list' });
        expect(screen.getByRole('link', { name: 'title root-0' })).toBeInTheDocument();
        expect(screen.queryByText(/peasant kickstart/)).not.toBeInTheDocument();

        // The retained browse flows still mount while the session list reports a
        // legacy selection error.
        await browseBy('every session');
        expect(browseTrigger()).toHaveTextContent('browse by: every session');
        await waitFor(() => expect(channels).toHaveBeenCalled());

        // An account change is a genuine boundary: it refreshes the list rather
        // than continuing another account's loaded rows.
        account = testCase.accountAfterFailure!;
        roots = [row('second-account-root')];
        selectionFailure = false;
        await act(async () => { fireEvent(window, new Event('focus')); });
        await browseBy('sessions');
        await screen.findByRole('link', { name: 'title second-account-root' });
        expect(screen.queryByRole('link', { name: 'title root-0' })).not.toBeInTheDocument();
      }
      if (testCase.name === 'overlapping-refresh-does-not-race') {
        delayed = deferred<Response>(); delayFilter = 'all';
        fireEvent(window, new Event('focus'));
        await waitFor(() => expect(pageRequests()).toHaveLength(2));
        fireEvent(window, new Event('focus'));
        expect(pageRequests()).toHaveLength(2);
        expect(screen.getByRole('link', { name: 'title root-0' })).toBeInTheDocument();
        await act(async () => { delayed?.resolve(json({ items: roots.slice(0,25), nextItems: roots.slice(25,50), page: 1, perPage: 25, total: roots.length, revision: '0' })); });
        expect(pageRequests()).toHaveLength(2);
      }
      if (testCase.name === 'collapse-cancels-expanded-refresh') {
        fireEvent.click(screen.getByRole('button', { name: 'subagents 30' }));
        await screen.findByRole('link', { name: 'title child-0' });
        delayed = deferred<Response>(); delayParent = 'root-0';
        fireEvent(window, new Event('focus'));
        await waitFor(() => expect(pageRequests()).toHaveLength(4));
        const childSignal = signals.at(-1)!;
        fireEvent.click(screen.getByRole('button', { name: 'subagents 30' }));
        expect(childSignal.aborted).toBe(true);
        await act(async () => { delayed?.resolve(json({ items: children.slice(0,25), nextItems: children.slice(25,50), page: 1, perPage: 25, total: children.length, revision: '0' })); });
        expect(screen.queryByRole('link', { name: 'title child-0' })).not.toBeInTheDocument();
        const before = pageRequests().length;
        fireEvent(window, new Event('focus'));
        await waitFor(() => expect(pageRequests()).toHaveLength(before + 1));
        expect(pageRequests().at(-1)?.searchParams.has('parentSessionId')).toBe(false);
      }
      if (testCase.name === 'collapsed-ancestor-skips-descendant-refresh') {
        fireEvent.click(screen.getByRole('button', { name: 'subagents 30' }));
        await screen.findByRole('link', { name: 'title child-0' });
        fireEvent.click(screen.getByRole('button', { name: 'subagents 1' }));
        await waitFor(() => expect(pageRequests()).toHaveLength(3));
        await waitFor(() => expect(screen.queryByText('loading subagents…')).not.toBeInTheDocument());
        fireEvent.click(screen.getByRole('button', { name: 'subagents 30' }));
        const before = pageRequests().length;
        fireEvent(window, new Event('focus'));
        await waitFor(() => expect(pageRequests()).toHaveLength(before + 1));
        expect(pageRequests().at(-1)?.searchParams.has('parentSessionId')).toBe(false);
      }
      if (testCase.name.startsWith('successful-refresh-invalidates-collapsed-')) {
        childSources.set('child-0', [row('old-grandchild'), row('kept-grandchild')]);
        fireEvent.click(screen.getByRole('button', { name: 'subagents 30' }));
        await screen.findByRole('link', { name: 'title child-0' });
        const descendant = testCase.name.endsWith('descendant');
        if (descendant) {
          fireEvent.click(screen.getByRole('button', { name: 'subagents 1' }));
          await screen.findByRole('link', { name: 'title old-grandchild' });
        }
        fireEvent.click(screen.getByRole('button', { name: descendant ? 'subagents 1' : 'subagents 30' }));
        children = [row('child-0', 1)];
        roots[0] = row('root-0', 1);
        childSources.set('child-0', [row('kept-grandchild')]);
        const before = pageRequests().length;
        fireEvent(window, new Event('focus'));
        await waitFor(() => expect(screen.getByRole('button', { name: 'subagents 1' })).toBeInTheDocument());
        await waitFor(() => expect(pageRequests().length).toBeGreaterThan(before));
        // Wait for the root replacement before reopening the discarded group.
        await waitFor(() => expect(screen.getByRole('region', { name: 'sessions' })).not.toHaveAttribute('aria-busy'));
        const reopen = pageRequests().length;
        const toggles = screen.getAllByRole('button', { name: 'subagents 1' });
        fireEvent.click(toggles[descendant ? 1 : 0]);
        await screen.findByRole('link', { name: descendant ? 'title kept-grandchild' : 'title child-0' });
        expect(pageRequests()).toHaveLength(reopen + 1);
        expect(pageRequests().at(-1)?.searchParams.get('parentSessionId')).toBe(descendant ? 'child-0' : 'root-0');
        expect(pageRequests().at(-1)?.searchParams.get('page')).toBe('1');
        expect(pageRequests().at(-1)?.searchParams.get('maxItems')).toBe('25');
        expect(screen.queryByRole('link', { name: descendant ? 'title old-grandchild' : 'title child-1' })).not.toBeInTheDocument();
      }
      if (testCase.parentErrorCode) {
        childSources.set('child-0', [row('grandchild')]);
        fireEvent.click(screen.getByRole('button', { name: 'subagents 30' }));
        await screen.findByRole('link', { name: 'title child-0' });
        fireEvent.click(screen.getByRole('button', { name: 'subagents 1' }));
        await screen.findByRole('link', { name: 'title grandchild' });
        roots = [row('replacement-root')];
        parentFailure = testCase.parentErrorCode;
        fireEvent(window, new Event('focus'));
        if (testCase.parentErrorCode === 'home_parent_not_found') {
          await screen.findByRole('link', { name: 'title replacement-root' });
          expect(screen.queryByRole('link', { name: 'title root-0' })).not.toBeInTheDocument();
          expect(screen.queryByRole('link', { name: 'title child-0' })).not.toBeInTheDocument();
          expect(screen.queryByRole('link', { name: 'title grandchild' })).not.toBeInTheDocument();
          const before = pageRequests().length;
          fireEvent(window, new Event('focus'));
          await waitFor(() => expect(pageRequests()).toHaveLength(before + 1));
          expect(pageRequests().at(-1)?.searchParams.has('parentSessionId')).toBe(false);
        } else {
          await screen.findByRole('button', { name: 'retry the session list' });
          expect(screen.getByRole('link', { name: 'title root-0' })).toBeInTheDocument();
          expect(screen.getByRole('link', { name: 'title grandchild' })).toBeInTheDocument();
          expect(screen.queryByRole('link', { name: 'title replacement-root' })).not.toBeInTheDocument();
        }
      }
      if (testCase.name.startsWith('publish-')) {
        fireEvent.click(screen.getByRole('button', { name: 'subagents 30' }));
        await screen.findByRole('link', { name: 'title child-0' });
        fireEvent.click(screen.getByRole('button', { name: 'next page' }));
        await screen.findByRole('link', { name: 'title root-25' });
        const before = pageRequests().length;
        const overlap = testCase.name === 'publish-during-refresh-queues-one-loaded-window';
        if (overlap) {
          holdPages = true;
          fireEvent(window, new Event('focus'));
          await waitFor(() => expect(releasePages).toHaveLength(2));
        }
        fireEvent.click(screen.getByRole('button', { name: 'publish fixture session' }));
        roots[0] = { ...roots[0], publication: { ...roots[0].publication, state: 'published', transcriptId: '00000000-0000-4000-8000-000000000001', transcriptUrl: 'https://village.example/transcripts/fixture', publishedAt: '2026-06-01T10:00:00Z' } };
        // A publish bumps the store revision, so the tab cache is stale.
        revision = '1';
        await act(async () => { pushReply.resolve(json(zSyncPushResponse.parse(fixture.publish.response))); });
        await waitFor(() => expect(screen.getByLabelText('publish completed')).toHaveTextContent('1'));
        expect(screen.getByRole('link', { name: 'title root-25' })).toBeInTheDocument();
        if (overlap) {
          expect(pagesOf(pageRequests().slice(before))).toEqual(fixture.publish.failedRefreshPages);
          holdPages = false;
          await act(async () => { releasePages.forEach(release => release()); });
        }
        await waitFor(() => expect(screen.getByRole('region', { name: 'sessions' })).not.toHaveAttribute('aria-busy'));
        fireEvent.click(screen.getByRole('button', { name: 'page 1' }));
        await waitFor(() => expect(screen.getByRole('link', { name: 'title root-0' }).closest('tr')).toHaveTextContent('published'));
        expect(pagesOf(pageRequests().slice(before))).toEqual(overlap ? [...fixture.publish.failedRefreshPages, ...fixture.publish.loadedPages] : fixture.publish.loadedPages);
        expect(screen.getByRole('link', { name: 'title child-0' })).toBeInTheDocument();
      }
      if (testCase.name === 'interleaved-project-grouping') {
        expect(new URLSearchParams(window.location.search).get('groupBy')).toBe('project');
        const groups = [...document.querySelectorAll('[data-home-group]')];
        expect(groups.map(group => group.getAttribute('data-home-group'))).toEqual([
          `project:${ALPHA_HASH}`,
          `project:${BETA_HASH}`,
        ]);
        expect(groups.map(group => group.textContent?.trim())).toEqual(['fixture-project', 'beta-project']);
        // Stable partition: A1, B1, A2 renders A1, A2 under A, then B1 under B.
        expect([...document.querySelectorAll('[data-session-id]')].map(node => node.getAttribute('data-session-id'))).toEqual(['a1', 'a2', 'b1']);
        // Collapsing a project group hides exactly its rows and writes the
        // device open set; expanding it restores them.
        const alphaGroup = () => document.querySelector(`[data-home-group="project:${ALPHA_HASH}"]`) as HTMLElement;
        fireEvent.click(within(alphaGroup()).getByRole('button'));
        await waitFor(() => expect(screen.queryByRole('link', { name: 'title a1' })).not.toBeInTheDocument());
        expect(screen.getByRole('link', { name: 'title b1' })).toBeInTheDocument();
        const stored = JSON.parse(window.localStorage.getItem('peasant.home') ?? '{}');
        expect(stored.collapsed).toEqual([`project:${ALPHA_HASH}`]);
        fireEvent.click(within(alphaGroup()).getByRole('button'));
        await screen.findByRole('link', { name: 'title a1' });
        return;
      }
      if (testCase.name === 'collapsed-group-does-not-hide-page-two') {
        const alphaGroup = () => document.querySelector(`[data-home-group="project:${ALPHA_HASH}"]`) as HTMLElement;
        fireEvent.click(within(alphaGroup()).getByRole('button'));
        await waitFor(() => expect(screen.queryByRole('link', { name: 'title a01' })).not.toBeInTheDocument());
        fireEvent.click(screen.getByRole('button', { name: 'next page' }));
        await screen.findByRole('link', { name: 'title c01' });
        // Page 2's projects were never collapsed, so they are open.
        for (const [id, hash] of [['c01', FIXTURE_PROJECTS.gamma.hash], ['d01', FIXTURE_PROJECTS.delta.hash]] as const) {
          expect(await screen.findByRole('link', { name: `title ${id}` })).toBeInTheDocument();
          const header = document.querySelector(`[data-home-group="project:${hash}"]`);
          expect(header?.querySelector('button')).toHaveAttribute('aria-expanded', 'true');
        }
        // Page 1 keeps its own collapsed project.
        fireEvent.click(screen.getByRole('button', { name: 'page 1' }));
        await screen.findByRole('link', { name: 'title b01' });
        expect(screen.queryByRole('link', { name: 'title a01' })).not.toBeInTheDocument();
        return;
      }
      if (testCase.name === 'hidden-session-column-grouping') {
        const table = document.querySelector('.home-table') as HTMLElement;
        expect(within(table).getAllByRole('columnheader').map(cell => cell.textContent?.trim())).toEqual(['project', 'status']);
        const group = document.querySelector('[data-home-group]');
        expect(group).not.toBeNull();
        expect(group).toHaveTextContent('fixture-project');
        expect(screen.getAllByText('fixture-project').length).toBeGreaterThan(0);
        return;
      }
    });
  }

});