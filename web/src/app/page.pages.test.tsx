import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { parse } from 'yaml';
import HomePage from './page';
import { PublishProvider, usePublishState } from '@/contexts/PublishContext';
import { zSyncPushRequest, zSyncPushResponse } from '@peasant-labs/schema';
import { ALPHA_HASH, makeSession } from '@/app/review/[[...segments]]/test-fixtures';
import type { LocalSyncSessionRow } from '@/lib/api/home';
import { browseBy, browseTrigger, chooseStatus, choiceRows } from '@/test/homeMenus';

const channels = vi.hoisted(() => vi.fn<(subscriptions: unknown) => { connected: boolean; data: unknown; error: null }>());
vi.mock('@/contexts/WebSocketContext', () => ({ useChannel: channels }));
vi.mock('next/navigation', () => ({ useRouter: () => ({ push: vi.fn() }) }));

interface Fixture { name: string; roots: number; children: number; parentErrorCode?: string; cachedSessionId?: string; cachedProject?: string; accountAfterFailure?: string; teammate?: { team: string; name: string }; assertTeammateLine?: boolean }
const fixture = parse(readFileSync(resolve(process.cwd(), 'src/app/testdata/home-pages.yaml'), 'utf8')) as { requiredNames: string[]; cases: Fixture[]; publish: { request: unknown; response: unknown; loadedTokens: { parent: string | null; token: string }[]; failedRefreshTokens: { parent: string | null; token: string }[] } };
const required = ['first-page-and-next-page', 'loaded-refresh-keeps-content-on-failure', 'stale-filter-response', 'lazy-children-and-child-more', 'account-change-clears-old-rows', 'collapsed-late-child-is-ignored', 'selection-error-keeps-loaded-content', 'overlapping-refresh-does-not-race', 'collapse-cancels-expanded-refresh', 'collapsed-ancestor-skips-descendant-refresh', 'successful-refresh-invalidates-collapsed-family', 'successful-refresh-invalidates-collapsed-descendant', 'canonical-missing-parent-commits-refreshed-roots', 'generic-child404-retains-visible-content', 'publish-settlement-refreshes-loaded-window', 'publish-during-refresh-queues-one-loaded-window', 'selection-error-retry-failure-keeps-loaded-content', 'selection-error-account-change-refreshes-loaded-content', 'claude-teammate-line', 'no-claude-teammate-line'];
expect([...fixture.requiredNames].sort()).toEqual([...required].sort());
expect(fixture.cases.map(row => row.name).sort()).toEqual([...required].sort());

// The mounted capture and this component suite read one fixture. Keep the
// visual harness identity and the case identity tied to the same value so a
// one-sided edit cannot publish a screenshot that proves a different name.
const visualTeammate = (fixture as { visual?: { teammate?: { team: string; name: string } } }).visual?.teammate;
if (!visualTeammate) throw new Error('home-pages.yaml visual.teammate must name the identity the mounted capture serves');
expect(fixture.cases.find(row => row.name === 'claude-teammate-line')?.teammate).toEqual({ team: visualTeammate.team, name: visualTeammate.name });

/** One fixture row; `teammate` is the identity the fixture case serves on it. */
function row(id: string, children = 0, teammate?: { team: string; name: string }): LocalSyncSessionRow {
  return {
    sync: {
      id, harness: 'claude-code', projectName: 'fixture-project', projectHash: ALPHA_HASH, hostSlug: 'fixture-host', startTime: '2026-06-01T10:00:00Z', durationMs: BigInt(60000), totalTokens: 10, turnCount: 2, model: 'fixture', syncStatus: 'new', previouslyPushed: false,
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
const pageRequests = () => requests.filter(url => url.pathname === '/api/v1/sync/sessions' && url.searchParams.get('view') === 'home');
const tokens = (urls: URL[]) => urls.map(url => ({ parent: url.searchParams.get('parentSessionId'), token: url.searchParams.get('pageToken') }));
function PublishAction() {
  const { publish, settled } = usePublishState();
  return <><button onClick={() => { void publish('root-0', zSyncPushRequest.parse(fixture.publish.request), 0).catch(() => {}); }}>publish fixture session</button><output aria-label="publish completed">{settled.get('root-0') ?? 0}</output></>;
}
function mount() { return render(<PublishProvider><HomePage /><PublishAction /></PublishProvider>); }

beforeEach(() => {
  requests = []; signals = []; fail = false; selectionFailure = false; account = 'first'; delayed = undefined; delayFilter = ''; delayParent = '';
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
      if (delayFilter && url.searchParams.get('publication') === delayFilter && delayed) return delayed.promise;
      if (delayParent && url.searchParams.get('parentSessionId') === delayParent && delayed) return delayed.promise;
      if (selectionFailure) return json({ error: 'saved selection metadata could not be read', code: 'selection_visibility' }, 500);
      if (fail) return json({ error: 'temporary read failure; retry' }, 503);
      if (parentFailure && url.searchParams.has('parentSessionId')) return json({ error: 'parent read refused; choose a visible parent', code: parentFailure }, 404);
      const start = url.searchParams.get('pageToken') === 'after-first' ? 25 : url.searchParams.get('pageToken') === 'after-second' ? 50 : 0;
      const parent = url.searchParams.get('parentSessionId');
      const source = parent ? childSources.get(parent) ?? children : roots;
      const body = { items: source.slice(start, start + 25), next: start + 25 < source.length ? start === 0 ? 'after-first' : 'after-second' : '' };
      if (holdPages) return new Promise<Response>(done => { releasePages.push(() => done(json(body))); });
      return json(body);
    }
    throw new Error(`Unexpected eager request ${url.pathname}`);
  }));
});
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

describe('mounted bounded Home', () => {
  for (const testCase of fixture.cases) {
    it(testCase.name, async () => {
      roots = Array.from({ length: testCase.roots }, (_, index) => row(`root-${index}`, index === 0 ? testCase.children : 0));
      children = Array.from({ length: testCase.children }, (_, index) => row(`child-${index}`, index === 0 ? 1 : 0, index === 0 ? testCase.teammate : undefined));
      mount();
      await screen.findByRole('link', { name: 'title root-0' });
      expect(channels).not.toHaveBeenCalled();
      expect(pageRequests()).toHaveLength(1);
      expect(pageRequests()[0].searchParams.get('maxItems')).toBe('25');
      expect(screen.queryByRole('link', { name: 'title root-25' })).not.toBeInTheDocument();
      expect(screen.getByRole('link', { name: 'title root-0' }).getAttribute('href')).toContain('root-0');
      expect(choiceRows('status').find(row => row.getAttribute('aria-checked') === 'true')).toHaveTextContent('all');

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
      if (testCase.name === 'first-page-and-next-page') {
        fireEvent.click(screen.getByRole('button', { name: 'next page' }));
        await screen.findByRole('link', { name: 'title root-25' });
        // One page at a time: the first page's rows leave the table.
        expect(screen.queryByRole('link', { name: 'title root-0' })).not.toBeInTheDocument();
        expect(pageRequests().map(url => url.searchParams.get('pageToken'))).toEqual(['', 'after-first']);
        // The server said more follow, so the pager offers the page after the ones read.
        expect(screen.getByRole('button', { name: 'page 3' })).toBeInTheDocument();
        // A page already read opens without asking the server again.
        fireEvent.click(screen.getByRole('button', { name: 'page 1' }));
        await screen.findByRole('link', { name: 'title root-0' });
        expect(pageRequests()).toHaveLength(2);
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
        expect(screen.getByRole('link', { name: 'title root-25' })).toBeInTheDocument();
        fireEvent.click(screen.getByRole('button', { name: 'page 1' }));
        expect(await screen.findByRole('link', { name: 'title child-0' })).toBeInTheDocument();
        expect(tokens(pageRequests().slice(before))).toEqual(fixture.publish.failedRefreshTokens);
        fail = false;
        fireEvent.click(screen.getByRole('button', { name: 'retry the session list' }));
        await waitFor(() => expect(screen.queryByRole('button', { name: 'retry the session list' })).not.toBeInTheDocument());
      }
      if (testCase.name === 'stale-filter-response') {
        delayed = deferred<Response>(); delayFilter = 'published';
        await chooseStatus('published');
        await waitFor(() => expect(pageRequests().at(-1)?.searchParams.get('publication')).toBe('published'));
        fireEvent.change(screen.getByRole('searchbox', { name: 'search your sessions' }), { target: { value: '' } });
        // Filter controls stay available during a first-page load.
        await chooseStatus('auto-publish on');
        await screen.findByRole('link', { name: 'title root-0' });
        await act(async () => { delayed?.resolve(json({ items: [row('stale')], next: '' })); });
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
        await act(async () => { delayed?.resolve(json({ items: children.slice(0,25), next: 'after-first' })); });
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
        await act(async () => { delayed?.resolve(json({ items: roots.slice(0,25), next: 'after-first' })); });
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
        await act(async () => { delayed?.resolve(json({ items: children.slice(0,25), next: 'after-first' })); });
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
        expect(pageRequests().at(-1)?.searchParams.get('pageToken')).toBe('');
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
        await act(async () => { pushReply.resolve(json(zSyncPushResponse.parse(fixture.publish.response))); });
        await waitFor(() => expect(screen.getByLabelText('publish completed')).toHaveTextContent('1'));
        expect(screen.getByRole('link', { name: 'title root-25' })).toBeInTheDocument();
        if (overlap) {
          expect(tokens(pageRequests().slice(before))).toEqual(fixture.publish.failedRefreshTokens);
          holdPages = false;
          await act(async () => { releasePages.forEach(release => release()); });
        }
        await waitFor(() => expect(screen.getByRole('region', { name: 'sessions' })).not.toHaveAttribute('aria-busy'));
        fireEvent.click(screen.getByRole('button', { name: 'page 1' }));
        await waitFor(() => expect(screen.getByRole('link', { name: 'title root-0' }).closest('tr')).toHaveTextContent('published'));
        expect(tokens(pageRequests().slice(before))).toEqual(overlap ? [...fixture.publish.loadedTokens, ...fixture.publish.loadedTokens] : fixture.publish.loadedTokens);
        expect(screen.getByRole('link', { name: 'title child-0' })).toBeInTheDocument();
      }
    });
  }
});
