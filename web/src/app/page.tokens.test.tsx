import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { parse } from 'yaml';
import HomePage from './page';
import { PublishProvider } from '@/contexts/PublishContext';
import { zLocalSyncProjectOptions, type LocalSyncProjectOption } from '@peasant-labs/schema';

vi.mock('next/navigation', () => ({ useRouter: () => ({ push: vi.fn() }) }));
vi.mock('@/contexts/WebSocketContext', () => ({ useChannel: () => { throw new Error('Home must not subscribe to full-library channels'); } }));

interface Page { ids: string[]; next: string; status?: number; code?: string; failOnce?: number; deferred?: boolean }
interface Case { name: string; pages: Record<string, Page>; children?: Record<string, Page>; refreshed?: Record<string, Page>; refreshedChildren?: Record<string, Page>; expectedTokens: string[]; expectedChildTokens?: string[]; rejectRestart?: boolean; deferOptions?: boolean; optionsError?: { status: number; code: string } }
const fixture = parse(readFileSync(resolve(process.cwd(), 'src/app/testdata/home-token-pages.yaml'), 'utf8')) as { requiredNames: string[]; projects: LocalSyncProjectOption[]; cases: Case[] };
const required = ['short-and-empty-root-continuation', 'empty-first-page-can-continue', 'continuation-retries-same-token', 'child-empty-continuation-retries', 'finite-refresh-follows-fresh-root-and-child-tokens', 'scope-error-restarts-once', 'generic400-does-not-restart', 'project-options-do-not-block-rows', 'project-filter-aborts-old-continuation', 'late-options-cannot-win-filter-reset', 'account-reset-reloads-project-options', 'project-options-error-retains-rows-and-retries', 'project-options-selection-error-clears-content'];
expect([...fixture.requiredNames].sort()).toEqual([...required].sort());
expect(fixture.cases.map(row => row.name).sort()).toEqual([...required].sort());
const projects = zLocalSyncProjectOptions.parse({ projects: fixture.projects }).projects;
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

describe('mounted Home opaque continuation', () => {
  for (const testCase of fixture.cases) it(testCase.name, async () => {
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
      sync: { id, harness: 'claude-code', projectName: fresh ? 'beta' : 'alpha', projectHash: projects[fresh ? 1 : 0].projectHash, hostSlug: 'fixture-host', startTime: '2026-06-01T10:00:00Z', durationMs: 60000, totalTokens: 10, turnCount: 2, model: 'fixture', syncStatus: 'new', previouslyPushed: false },
      publication: { sessionId: id, state: 'unpublished', autoPublish: false, outsideSelection: false },
      title: `${fresh ? 'fresh ' : ''}${id}`, preview: '', childCount: id === 'root-one' && testCase.children ? 1 : 0,
    });
    const homeReads = () => requests.filter(url => url.searchParams.get('view') === 'home');
    const tokens = (urls: URL[]) => urls.map(url => url.searchParams.get('pageToken'));
    vi.stubGlobal('fetch', vi.fn(async (input: string, init?: RequestInit) => {
      const url = new URL(input, 'http://localhost');
      requests.push(url);
      if (url.pathname === '/api/v1/sync/auth') return json({ authenticated: true, username, villageUrl: 'https://village.example' });
      expect(url.pathname).toBe('/api/v1/sync/sessions');
      if (url.searchParams.get('view') === 'project-options') {
        optionsRead++;
        optionSignals.push(init?.signal as AbortSignal);
        if (testCase.pages[''].ids.length) expect(screen.queryByRole('link', { name: /root-one|root-last/ })).toBeInTheDocument();
        else expect(screen.getByText('no session to list here.')).toBeInTheDocument();
        if (testCase.optionsError && optionsRead === 1) return json({ error: 'project choices could not be read; retry or run peasant kickstart', code: testCase.optionsError.code }, testCase.optionsError.status);
        if (testCase.deferOptions && optionsRead === 1) {
          const snapshot = options;
          return new Promise<Response>(done => releases.push(() => done(json({ projects: snapshot }))));
        }
        return json({ projects: options });
      }
      expect(url.searchParams.get('view')).toBe('home');
      expect(url.searchParams.get('maxItems')).toBe('25');
      expect(url.searchParams.has('offset')).toBe(false);
      expect(url.searchParams.has('limit')).toBe(false);
      signals.push(init?.signal as AbortSignal);
      const token = url.searchParams.get('pageToken') ?? '';
      const parent = url.searchParams.get('parentSessionId');
      if (parent) { expect(url.searchParams.has('projectHash')).toBe(false); expect(url.searchParams.has('publication')).toBe(false); }
      const pages = parent ? fresh ? testCase.refreshedChildren ?? testCase.children : testCase.children : fresh ? testCase.refreshed ?? testCase.pages : testCase.pages;
      const page = pages?.[token];
      if (!page) throw new Error(`Unexpected continuation ${token}: the client drained an unvisited page`);
      const key = `${parent ?? ''}:${token}:${fresh}`;
      const attempt = (attempts.get(key) ?? 0) + 1;
      attempts.set(key, attempt);
      if (testCase.rejectRestart && token === '' && attempt > 1) return json({ error: 'scope changed again; restart explicitly', code: 'sync_page_scope_changed' }, 400);
      if (page.status || page.failOnce && attempt === 1) return json({ error: 'page refused; retry the same continuation', code: page.code }, page.status ?? page.failOnce);
      const body = { items: page.ids.map(row), next: page.next };
      if (page.deferred) return new Promise<Response>(done => releases.push(() => done(json(body))));
      return json(body);
    }));
    render(<PublishProvider><HomePage /></PublishProvider>);
    if (testCase.optionsError?.code === 'selection_visibility') {
      await screen.findByRole('button', { name: 'retry the session list' });
      expect(screen.queryByRole('link', { name: 'root-one' })).not.toBeInTheDocument();
      expect(screen.queryByRole('option', { name: 'alpha' })).not.toBeInTheDocument();
      expect(tokens(homeReads())).toEqual(testCase.expectedTokens);
      return;
    }
    if (testCase.pages[''].ids.length) await screen.findByRole('link', { name: 'root-one' });
    else await screen.findByRole('button', { name: 'load more' });

    if (testCase.name === 'short-and-empty-root-continuation' || testCase.name === 'empty-first-page-can-continue') {
      for (let index = 1; index < testCase.expectedTokens.length; index++) {
        expect(homeReads()).toHaveLength(index);
        fireEvent.click(screen.getByRole('button', { name: 'load more' }));
        await waitFor(() => expect(homeReads()).toHaveLength(index + 1));
        await waitFor(() => expect(screen.queryByRole('button', { name: 'load more' })?.hasAttribute('disabled') ?? false).toBe(false));
      }
      await screen.findByRole('link', { name: 'root-last' });
      expect(screen.queryByRole('button', { name: 'load more' })).not.toBeInTheDocument();
    }
    if (testCase.name === 'continuation-retries-same-token') {
      fireEvent.click(screen.getByRole('button', { name: 'load more' }));
      await screen.findByRole('button', { name: 'retry the session list' });
      expect(screen.getByRole('link', { name: 'root-one' })).toBeInTheDocument();
      fireEvent.click(screen.getByRole('button', { name: 'retry the session list' }));
      await screen.findByRole('link', { name: 'root-last' });
    }
    if (testCase.name === 'child-empty-continuation-retries') {
      fireEvent.click(screen.getByRole('button', { name: 'subagents 1' }));
      await screen.findByRole('button', { name: 'load more subagents' });
      fireEvent.click(screen.getByRole('button', { name: 'load more subagents' }));
      await screen.findByRole('button', { name: 'retry subagents' });
      fireEvent.click(screen.getByRole('button', { name: 'retry subagents' }));
      await screen.findByRole('link', { name: 'child-one' });
      expect(tokens(homeReads().filter(url => url.searchParams.has('parentSessionId')))).toEqual(testCase.expectedTokens);
      return;
    }
    if (testCase.refreshed) {
      fireEvent.click(screen.getByRole('button', { name: 'load more' }));
      await waitFor(() => expect(homeReads()).toHaveLength(2));
      await waitFor(() => expect(screen.getByRole('button', { name: 'load more' })).not.toBeDisabled());
      fireEvent.click(screen.getByRole('button', { name: 'load more' }));
      await screen.findByRole('link', { name: 'root-last' });
      fireEvent.click(screen.getByRole('button', { name: 'subagents 1' }));
      await screen.findByRole('link', { name: 'child-one' });
      fireEvent.click(screen.getByRole('button', { name: 'load more subagents' }));
      await waitFor(() => expect(homeReads()).toHaveLength(5));
      await waitFor(() => expect(screen.getByRole('button', { name: 'load more subagents' })).not.toBeDisabled());
      const before = homeReads().length;
      fresh = true;
      fireEvent(window, new Event('focus'));
      await screen.findByRole('link', { name: 'fresh root-one' });
      expect(tokens(homeReads().slice(before).filter(url => !url.searchParams.has('parentSessionId')))).toEqual(testCase.expectedTokens);
      expect(tokens(homeReads().slice(before).filter(url => url.searchParams.has('parentSessionId')))).toEqual(testCase.expectedChildTokens);
      expect(screen.getByRole('button', { name: 'load more' })).toBeInTheDocument();
      expect(screen.queryByRole('link', { name: 'root-last' })).not.toBeInTheDocument();
      return;
    }
    if (testCase.name.endsWith('does-not-restart') || testCase.rejectRestart) {
      fireEvent.click(screen.getByRole('button', { name: 'load more' }));
      await screen.findByRole('button', { name: 'retry the session list' });
      expect(!!screen.queryByRole('link', { name: 'root-one' })).toBe(!testCase.rejectRestart);
    }
    if (testCase.name === 'project-options-do-not-block-rows') {
      await waitFor(() => expect(releases).toHaveLength(1));
      expect(screen.getByRole('link', { name: 'root-one' })).toBeInTheDocument();
      expect(screen.queryByRole('option', { name: 'beta' })).not.toBeInTheDocument();
      await act(async () => { releases[0](); });
      await screen.findByRole('option', { name: 'beta' });
    }
    if (testCase.name === 'project-filter-aborts-old-continuation') {
      await screen.findByRole('option', { name: 'beta' });
      fireEvent.click(screen.getByRole('button', { name: 'load more' }));
      await waitFor(() => expect(releases).toHaveLength(1));
      const oldSignal = signals.at(-1)!;
      fresh = true;
      fireEvent.change(screen.getByRole('combobox', { name: 'project' }), { target: { value: projects[1].projectHash } });
      await screen.findByRole('link', { name: 'fresh root-one' });
      expect(oldSignal.aborted).toBe(true);
      expect(homeReads().at(-1)?.searchParams.get('projectHash')).toBe(projects[1].projectHash);
      await act(async () => { releases[0](); });
      expect(screen.queryByRole('link', { name: 'old-off-page' })).not.toBeInTheDocument();
    }
    if (testCase.name === 'late-options-cannot-win-filter-reset') {
      await waitFor(() => expect(releases).toHaveLength(1));
      options = [projects[1]];
      fireEvent.click(screen.getByRole('button', { name: 'auto' }));
      await screen.findByRole('option', { name: 'beta' });
      await act(async () => { releases[0](); });
      expect(optionSignals[0].aborted).toBe(true);
      expect(screen.queryByRole('option', { name: 'alpha' })).not.toBeInTheDocument();
    }
    if (testCase.name === 'account-reset-reloads-project-options') {
      await screen.findByRole('option', { name: 'alpha' });
      username = 'second-account';
      options = [projects[1]];
      fresh = true;
      fireEvent(window, new Event('focus'));
      await screen.findByRole('option', { name: 'beta' });
      await waitFor(() => expect(screen.queryByRole('option', { name: 'alpha' })).not.toBeInTheDocument());
      expect(screen.queryByRole('link', { name: 'root-one' })).not.toBeInTheDocument();
    }
    if (testCase.name === 'project-options-error-retains-rows-and-retries') {
      await screen.findByRole('button', { name: 'retry project choices' });
      expect(screen.getByRole('link', { name: 'root-one' })).toBeInTheDocument();
      fireEvent.click(screen.getByRole('button', { name: 'retry project choices' }));
      await screen.findByRole('option', { name: 'beta' });
      expect(optionsRead).toBe(2);
    }
    expect(tokens(homeReads())).toEqual(testCase.expectedTokens);
  });
});
