import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { parse } from 'yaml';
import HomePage from './page';
import { PublishProvider } from '@/contexts/PublishContext';
import { ALPHA_HASH } from '@/app/review/[[...segments]]/test-fixtures';
import type { LocalHomeSessionRow } from '@/lib/api/home';

const channels = vi.hoisted(() => vi.fn(() => ({ connected: true, data: { sessions: [] }, error: null })));
vi.mock('@/contexts/WebSocketContext', () => ({ useChannel: channels }));
vi.mock('next/navigation', () => ({ useRouter: () => ({ push: vi.fn() }) }));

interface Fixture { name: string; roots: number; children: number }
const fixture = parse(readFileSync(resolve(process.cwd(), 'src/app/testdata/home-pages.yaml'), 'utf8')) as { requiredNames: string[]; cases: Fixture[] };
const required = ['first-page-and-explicit-more', 'loaded-refresh-keeps-content-on-failure', 'stale-filter-response', 'lazy-children-and-child-more', 'account-change-clears-old-rows', 'collapsed-late-child-is-ignored', 'selection-refresh-clears-loaded-content', 'overlapping-refresh-does-not-race', 'collapse-cancels-expanded-refresh', 'collapsed-ancestor-skips-descendant-refresh'];
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
const pageRequests = () => requests.filter(url => url.pathname === '/api/v1/home/sessions');
function mount() { return render(<PublishProvider><HomePage /></PublishProvider>); }

beforeEach(() => {
  requests = []; signals = []; fail = false; selectionFailure = false; account = 'first'; delayed = undefined; delayFilter = ''; delayParent = '';
  channels.mockClear();
  vi.stubGlobal('fetch', vi.fn(async (input: string, init?: RequestInit) => {
    const url = new URL(input, 'http://localhost');
    requests.push(url);
    if (url.pathname === '/api/v1/sync/auth') return json({ authenticated: true, username: account, villageUrl: 'https://village.example' });
    if (url.pathname === '/api/v1/publications') return json({ publications: [] });
    if (url.pathname === '/api/v1/home/sessions') {
      signals.push(init?.signal as AbortSignal);
      if (delayFilter && url.searchParams.get('publication') === delayFilter && delayed) return delayed.promise;
      if (delayParent && url.searchParams.get('parentSessionId') === delayParent && delayed) return delayed.promise;
      if (selectionFailure) return json({ error: 'saved selection could not be applied; run peasant kickstart', code: 'selection_visibility' }, 500);
      if (fail) return json({ error: 'temporary read failure; retry' }, 503);
      const offset = Number(url.searchParams.get('offset'));
      const source = url.searchParams.has('parentSessionId') ? children : roots;
      return json({ rows: source.slice(offset, offset + 25), offset, limit: 25, ...(offset + 25 < source.length ? { nextOffset: offset + 25 } : {}) });
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
    });
  }
});
