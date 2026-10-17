import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { parse } from 'yaml';
import HomePage from './page';
import { PublishProvider, usePublishState } from '@/contexts/PublishContext';
import { zSyncPushRequest, zSyncPushResponse } from '@peasant-labs/schema';
import { ALPHA_HASH } from '@/app/review/[[...segments]]/test-fixtures';
import type { LocalHomeSessionRow } from '@/lib/api/home';

const channels = vi.hoisted(() => vi.fn(() => ({ connected: true, data: { sessions: [] }, error: null })));
vi.mock('@/contexts/WebSocketContext', () => ({ useChannel: channels }));
vi.mock('next/navigation', () => ({ useRouter: () => ({ push: vi.fn() }) }));

interface Fixture { name: string; roots: number; children: number; parentErrorCode?: string }
const fixture = parse(readFileSync(resolve(process.cwd(), 'src/app/testdata/home-pages.yaml'), 'utf8')) as { requiredNames: string[]; cases: Fixture[]; publish: { request: unknown; response: unknown; loadedOffsets: { parent: string | null; offset: string }[] } };
const required = ['first-page-and-explicit-more', 'loaded-refresh-keeps-content-on-failure', 'stale-filter-response', 'lazy-children-and-child-more', 'account-change-clears-old-rows', 'collapsed-late-child-is-ignored', 'selection-refresh-clears-loaded-content', 'overlapping-refresh-does-not-race', 'collapse-cancels-expanded-refresh', 'collapsed-ancestor-skips-descendant-refresh', 'successful-refresh-invalidates-collapsed-family', 'successful-refresh-invalidates-collapsed-descendant', 'canonical-missing-parent-commits-refreshed-roots', 'generic-child404-retains-visible-content', 'publish-settlement-refreshes-loaded-window', 'publish-during-refresh-queues-one-loaded-window'];
expect([...fixture.requiredNames].sort()).toEqual([...required].sort());
expect(fixture.cases.map(row => row.name).sort()).toEqual([...required].sort());

function row(id: string, children = 0): LocalHomeSessionRow {
  return {
    sync: { id, harness: 'claude-code', projectName: 'fixture-project', projectHash: ALPHA_HASH, hostSlug: 'fixture-host', startTime: '2026-06-01T10:00:00Z', durationMs: BigInt(60000), totalTokens: 10, turnCount: 2, model: 'fixture', syncStatus: 'new', previouslyPushed: false },
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
let roots: LocalHomeSessionRow[];
let children: LocalHomeSessionRow[];
let delayed: ReturnType<typeof deferred<Response>> | undefined;
let delayFilter = '';
let delayParent = '';
let signals: AbortSignal[];
let childSources: Map<string, LocalHomeSessionRow[]>;
let parentFailure: string | undefined;
let pushReply: ReturnType<typeof deferred<Response>>;
let holdPages: boolean;
let releasePages: (() => void)[];
const pageRequests = () => requests.filter(url => url.pathname === '/api/v1/home/sessions');
const offsets = (urls: URL[]) => urls.map(url => ({ parent: url.searchParams.get('parentSessionId'), offset: url.searchParams.get('offset') }));
function PublishAction() {
  const { publish, settled } = usePublishState();
  return <><button onClick={() => { void publish('root-0', zSyncPushRequest.parse(fixture.publish.request), 0).catch(() => {}); }}>publish fixture session</button><output aria-label="publish completed">{settled.get('root-0') ?? 0}</output></>;
}
function mount() { return render(<PublishProvider><HomePage /><PublishAction /></PublishProvider>); }

beforeEach(() => {
  requests = []; signals = []; fail = false; selectionFailure = false; account = 'first'; delayed = undefined; delayFilter = ''; delayParent = '';
  channels.mockClear();
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
    if (url.pathname === '/api/v1/home/sessions') {
      signals.push(init?.signal as AbortSignal);
      if (delayFilter && url.searchParams.get('publication') === delayFilter && delayed) return delayed.promise;
      if (delayParent && url.searchParams.get('parentSessionId') === delayParent && delayed) return delayed.promise;
      if (selectionFailure) return json({ error: 'saved selection could not be applied; run peasant kickstart', code: 'selection_visibility' }, 500);
      if (fail) return json({ error: 'temporary read failure; retry' }, 503);
      if (parentFailure && url.searchParams.has('parentSessionId')) return json({ error: 'parent read refused; choose a visible parent', code: parentFailure }, 404);
      const offset = Number(url.searchParams.get('offset'));
      const parent = url.searchParams.get('parentSessionId');
      const source = parent ? childSources.get(parent) ?? children : roots;
      const body = { rows: source.slice(offset, offset + 25), offset, limit: 25, ...(offset + 25 < source.length ? { nextOffset: offset + 25 } : {}) };
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
      children = Array.from({ length: testCase.children }, (_, index) => row(`child-${index}`, index === 0 ? 1 : 0));
      mount();
      await screen.findByRole('link', { name: 'title root-0' });
      expect(channels).not.toHaveBeenCalled();
      expect(pageRequests()).toHaveLength(1);
      expect(pageRequests()[0].searchParams.get('limit')).toBe('25');
      expect(screen.queryByRole('link', { name: 'title root-25' })).not.toBeInTheDocument();
      expect(screen.getByRole('link', { name: 'title root-0' }).getAttribute('href')).toContain('root-0');
      expect(screen.getByRole('button', { name: 'all' })).toBeInTheDocument();

      if (testCase.name === 'first-page-and-explicit-more') {
        fireEvent.click(screen.getByRole('button', { name: 'load more' }));
        await screen.findByRole('link', { name: 'title root-25' });
        expect(pageRequests().map(url => url.searchParams.get('offset'))).toEqual(['0', '25']);
      }
      if (testCase.name === 'loaded-refresh-keeps-content-on-failure') {
        fireEvent.click(screen.getByRole('button', { name: 'load more' }));
        await screen.findByRole('link', { name: 'title root-25' });
        fireEvent.click(screen.getByRole('button', { name: 'subagents 30' }));
        await screen.findByRole('link', { name: 'title child-0' });
        const before = pageRequests().length;
        fail = true;
        fireEvent(window, new Event('focus'));
        await screen.findByRole('button', { name: 'retry the session list' });
        expect(screen.getByRole('link', { name: 'title root-25' })).toBeInTheDocument();
        expect(screen.getByRole('link', { name: 'title child-0' })).toBeInTheDocument();
        expect(pageRequests().slice(before).map(url => [url.searchParams.get('parentSessionId'), url.searchParams.get('offset')])).toEqual([[null, '0'], [null, '25'], ['root-0', '0']]);
        fail = false;
        fireEvent.click(screen.getByRole('button', { name: 'retry the session list' }));
        await waitFor(() => expect(screen.queryByRole('button', { name: 'retry the session list' })).not.toBeInTheDocument());
      }
      if (testCase.name === 'stale-filter-response') {
        delayed = deferred<Response>(); delayFilter = 'published';
        fireEvent.click(screen.getByRole('button', { name: 'published' }));
        await waitFor(() => expect(pageRequests().at(-1)?.searchParams.get('publication')).toBe('published'));
        fireEvent.change(screen.getByRole('searchbox', { name: 'search transcripts' }), { target: { value: '' } });
        // Filter controls stay available during a first-page load.
        fireEvent.click(screen.getByRole('button', { name: 'auto' }));
        await screen.findByRole('link', { name: 'title root-0' });
        await act(async () => { delayed?.resolve(json({ rows: [row('stale')], limit: 25, offset: 0 })); });
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
        await act(async () => { delayed?.resolve(json({ rows: children.slice(0,25), limit: 25, offset: 0, nextOffset: 25 })); });
        expect(screen.queryByRole('link', { name: 'title child-0' })).not.toBeInTheDocument();
        delayParent = '';
        fireEvent.click(screen.getByRole('button', { name: 'subagents 30' }));
        await screen.findByRole('link', { name: 'title child-0' });
        expect(pageRequests()).toHaveLength(3);
      }
      if (testCase.name === 'selection-refresh-clears-loaded-content') {
        selectionFailure = true;
        fireEvent(window, new Event('focus'));
        await screen.findByRole('button', { name: 'retry the session list' });
        expect(screen.queryByRole('link', { name: 'title root-0' })).not.toBeInTheDocument();
        expect(screen.queryByRole('region', { name: 'sessions' })).not.toBeInTheDocument();
        selectionFailure = false;
        fireEvent.click(screen.getByRole('button', { name: 'retry the session list' }));
        await screen.findByRole('link', { name: 'title root-0' });
      }
      if (testCase.name === 'overlapping-refresh-does-not-race') {
        delayed = deferred<Response>(); delayFilter = 'all';
        fireEvent(window, new Event('focus'));
        await waitFor(() => expect(pageRequests()).toHaveLength(2));
        fireEvent(window, new Event('focus'));
        expect(pageRequests()).toHaveLength(2);
        expect(screen.getByRole('link', { name: 'title root-0' })).toBeInTheDocument();
        await act(async () => { delayed?.resolve(json({ rows: roots.slice(0,25), limit: 25, offset: 0, nextOffset: 25 })); });
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
        await act(async () => { delayed?.resolve(json({ rows: children.slice(0,25), limit: 25, offset: 0, nextOffset: 25 })); });
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
        await waitFor(() => expect(screen.getByRole('button', { name: 'load more' })).not.toBeDisabled());
        const reopen = pageRequests().length;
        const toggles = screen.getAllByRole('button', { name: 'subagents 1' });
        fireEvent.click(toggles[descendant ? 1 : 0]);
        await screen.findByRole('link', { name: descendant ? 'title kept-grandchild' : 'title child-0' });
        expect(pageRequests()).toHaveLength(reopen + 1);
        expect(pageRequests().at(-1)?.searchParams.get('parentSessionId')).toBe(descendant ? 'child-0' : 'root-0');
        expect(pageRequests().at(-1)?.searchParams.get('offset')).toBe('0');
        expect(pageRequests().at(-1)?.searchParams.get('limit')).toBe('25');
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
        fireEvent.click(screen.getByRole('button', { name: 'load more' }));
        await screen.findByRole('link', { name: 'title root-25' });
        fireEvent.click(screen.getByRole('button', { name: 'subagents 30' }));
        await screen.findByRole('link', { name: 'title child-0' });
        const before = pageRequests().length;
        const overlap = testCase.name === 'publish-during-refresh-queues-one-loaded-window';
        if (overlap) {
          holdPages = true;
          fireEvent(window, new Event('focus'));
          await waitFor(() => expect(releasePages).toHaveLength(fixture.publish.loadedOffsets.length));
        }
        fireEvent.click(screen.getByRole('button', { name: 'publish fixture session' }));
        roots[0] = { ...roots[0], publication: { ...roots[0].publication, state: 'published', transcriptId: '00000000-0000-4000-8000-000000000001', transcriptUrl: 'https://village.example/transcripts/fixture', publishedAt: '2026-06-01T10:00:00Z' } };
        await act(async () => { pushReply.resolve(json(zSyncPushResponse.parse(fixture.publish.response))); });
        await waitFor(() => expect(screen.getByLabelText('publish completed')).toHaveTextContent('1'));
        expect(screen.getByRole('link', { name: 'title root-25' })).toBeInTheDocument();
        if (overlap) {
          expect(offsets(pageRequests().slice(before))).toEqual(fixture.publish.loadedOffsets);
          holdPages = false;
          await act(async () => { releasePages.forEach(release => release()); });
        }
        await waitFor(() => expect(screen.getByRole('link', { name: 'title root-0' }).closest('tr')).toHaveTextContent('published · up to date'));
        expect(offsets(pageRequests().slice(before))).toEqual(overlap ? [...fixture.publish.loadedOffsets, ...fixture.publish.loadedOffsets] : fixture.publish.loadedOffsets);
        expect(screen.getByRole('link', { name: 'title child-0' })).toBeInTheDocument();
      }
    });
  }
});
