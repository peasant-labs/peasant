import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { cleanup, fireEvent, render, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  parseSessionDetailPayloadValue,
  type SessionDetailPayload,
  type SessionRelationship,
  type SessionRelationshipNavigation,
} from '@peasant-labs/schema';
import { ProjectsRouter } from './ProjectsRouter';
import { transcriptHref, type ProjectHash } from '@/lib/navigation/projectRoutes';
import {
  loadContextNavigationFixture,
  type ContextNavigationCase,
} from '@/test/contextNavigationFixture';

/**
 * Mounts the REAL production transcript route
 * (`ProjectsRouter` -> `SessionDetailV2` -> fairtrade's `TranscriptViewer`):
 * only the WebSocket channel, project-identity fetch, and theme are replaced.
 * The assertions read the rendered session-context markup and drive the actual
 * navigation callback, so a stub that never reached Fairtrade's adapter would
 * fail here. Following a context link also mounts the actual TARGET session so
 * the route a reader is sent to is production, not an asserted string.
 */

const PROJECT_HASH = 'a'.repeat(64) as ProjectHash;
const CHILD_ID = 'sess_contextchild';
const CASES_PATH = 'src/components/session-detail/v2/testdata/context_navigation.yaml';
const MANIFEST_PATH = 'src/components/session-detail/v2/testdata/context_navigation.manifest.yaml';

const casesSource = readFileSync(resolve(process.cwd(), CASES_PATH), 'utf8');
const manifestSource = readFileSync(resolve(process.cwd(), MANIFEST_PATH), 'utf8');
const fixture = loadContextNavigationFixture(casesSource, manifestSource);

let pathname = `/projects/${PROJECT_HASH}/${CHILD_ID}`;
let search = '';
const pushed: string[] = [];
const replaced: string[] = [];
const channelBySession = new Map<string, unknown>();
const fetchProjectResolution = vi.fn();

vi.mock('next/navigation', () => ({
  usePathname: () => pathname,
  useSearchParams: () => new URLSearchParams(search),
  useRouter: () => ({
    push: (href: string) => { pushed.push(href); },
    replace: (href: string) => { replaced.push(href); },
  }),
}));
vi.mock('@/contexts/WebSocketContext', () => ({
  useChannel: (sub: { id?: string }) => ({
    data: sub?.id ? channelBySession.get(sub.id) : undefined,
    connected: true,
    error: null,
  }),
}));
vi.mock('@/lib/api/map', () => ({
  fetchProjectResolution: (...args: unknown[]) => fetchProjectResolution(...args),
}));
vi.mock('@/components/session-detail/v2/lib/useEntryLabels', () => ({
  useEntryLabels: () => ({ entryTypes: [], labelsByEntry: new Map(), addLabel: vi.fn() }),
}));
vi.mock('@/hooks/useTheme', () => ({
  useTheme: () => ({ theme: 'dark', setTheme: vi.fn(), toggle: vi.fn() }),
}));
vi.mock('@peasant-labs/fairtrade/graph', () => ({ TrajectoryGraph: () => null }));

type FlatReadWire = SessionDetailPayload & { relationshipNavigation?: SessionRelationshipNavigation[] };

function childWire(c: ContextNavigationCase): FlatReadWire {
  const durable: SessionDetailPayload = {
    id: CHILD_ID,
    harness: 'codex',
    project: 'alpha-project',
    startTime: '2026-08-28T09:00:00.000Z',
    endTime: '2026-08-28T09:02:00.000Z',
    durationMins: 2,
    totalTokens: 20,
    tokensIn: 12,
    tokensOut: 8,
    turnCount: 2,
    toolCallCount: 0,
    relationships: c.relationships as unknown as SessionRelationship[],
    turns: [
      { index: 0, role: 'user', entryType: 'text', depth: 0, content: 'child request', timestamp: '2026-08-28T09:00:00.000Z' },
      { index: 1, role: 'assistant', entryType: 'text', depth: 0, content: 'child response', timestamp: '2026-08-28T09:01:00.000Z' },
    ],
  };
  if (c.earlierTurnContent) {
    durable.earlierHistory = [
      {
        state: 'uncertain_migrated',
        turns: [
          { index: 0, role: 'user', entryType: 'text', depth: 0, content: c.earlierTurnContent, timestamp: '2026-08-28T08:00:00.000Z' },
        ],
      },
    ];
  }
  // The fixture is producer-shaped: prove each durable payload parses with the
  // same canonical parser the adapter runs, so a case cannot drift from the
  // published wire contract.
  parseSessionDetailPayloadValue(durable);
  return { ...durable, relationshipNavigation: c.navigation as unknown as SessionRelationshipNavigation[] };
}

/** The current target's own durable content, served by the followed route. */
function targetWire(targetId: string): SessionDetailPayload {
  const durable: SessionDetailPayload = {
    id: targetId,
    harness: 'codex',
    project: 'alpha-project',
    startTime: '2026-08-28T10:00:00.000Z',
    endTime: '2026-08-28T10:01:00.000Z',
    durationMins: 1,
    totalTokens: 5,
    tokensIn: 3,
    tokensOut: 2,
    turnCount: 1,
    toolCallCount: 0,
    turns: [
      { index: 0, role: 'user', entryType: 'text', depth: 0, content: `current target ${targetId}`, timestamp: '2026-08-28T10:00:00.000Z' },
    ],
  };
  parseSessionDetailPayloadValue(durable);
  return durable;
}

function caseNamed(name: string): ContextNavigationCase {
  const found = fixture.cases.find((entry) => entry.name === name);
  if (!found) throw new Error(`fixture case ${name} is missing`);
  return found;
}

function contextSources(): HTMLElement[] {
  return [...document.querySelectorAll<HTMLElement>('.txn-context-source')];
}

function contextLinks(): HTMLButtonElement[] {
  return [...document.querySelectorAll<HTMLButtonElement>('.txn-context-link')];
}

async function waitForViewer(): Promise<void> {
  await waitFor(() => expect(document.querySelector('.txn-app')).not.toBeNull());
}

/**
 * Wait until `read()` satisfies `check`. Every restoration failure carries one
 * searchable diagnostic naming the invariant, so a mutation of the production
 * restoration path fails with the state it actually produced.
 */
async function restoredState<T>(read: () => T, check: (value: T) => boolean, detail: string): Promise<void> {
  await waitFor(() => {
    const value = read();
    if (!check(value)) throw new Error(`mounted context navigation invariant failed: ${detail} (read ${JSON.stringify(value)})`);
  });
}

/** Mount the child session through the production route. */
function mountChild(c: ContextNavigationCase) {
  pathname = `/projects/${PROJECT_HASH}/${CHILD_ID}`;
  search = c.search ?? '';
  channelBySession.set(CHILD_ID, childWire(c));
  return render(<ProjectsRouter />);
}

beforeEach(() => {
  window.sessionStorage.clear();
  channelBySession.clear();
  pushed.length = 0;
  replaced.length = 0;
  fetchProjectResolution.mockReset();
  fetchProjectResolution.mockResolvedValue({ project: 'alpha-project', projectHash: PROJECT_HASH });
});

afterEach(() => {
  cleanup();
  window.sessionStorage.clear();
});

describe('mounted context and starter navigation on the production transcript route', () => {
  it('covers every named fixture case exactly', () => {
    expect(fixture.cases.map((entry) => entry.name).sort()).toEqual(
      fixture.cases.map((entry) => entry.name).slice().sort(),
    );
    expect(fixture.cases.length).toBe(8);
    expect(fixture.backCase.name).toBe('context-and-starter-distinct');
  });

  for (const c of fixture.cases) {
    it(c.name, async () => {
      const view = mountChild(c);
      await waitForViewer();
      await waitFor(() => expect(contextSources().length).toBe(c.expectedRows.length));

      expect(
        contextSources().map((source) => source.querySelector('.txn-context-row span:not(.txn-context-status)')?.textContent ?? null),
      ).toEqual(c.expectedRows.map((row) => row.label));
      expect(contextSources().map((source) => source.querySelector('.txn-context-note')?.textContent ?? null)).toEqual(
        c.expectedRows.map((row) => row.note),
      );

      const linkedRows = c.expectedRows.filter((row) => row.link !== null);
      const links = contextLinks();
      expect(links.map((link) => link.textContent)).toEqual(linkedRows.map(() => 'open current session'));
      expect(
        contextSources().map((source) => source.querySelector('.txn-context-status')?.textContent ?? null),
      ).toEqual(c.expectedRows.map((row) => row.status));

      // Every link the mounted viewer offers opens its own exact stored target
      // with a push, so the browser keeps the child route for Back.
      for (let index = 0; index < links.length; index += 1) {
        const expectedTarget = linkedRows[index].link as string;
        pushed.length = 0;
        fireEvent.click(links[index]);
        if (pushed.length !== 1 || pushed[0] !== transcriptHref(PROJECT_HASH, expectedTarget)) {
          throw new Error(`the current-parent link did not route to the exact stored target ${expectedTarget}: ${JSON.stringify(pushed)}`);
        }
        expect(replaced).toEqual([]);
      }
      if (links.length === 0) expect(pushed).toEqual([]);

      expect(view.container.querySelector('.txn-app')).not.toBeNull();
    });
  }

  it('opens the exact stored target session when a reader follows the context link', async () => {
    const c = caseNamed('context-and-starter-distinct');
    const view = mountChild(c);
    await waitForViewer();

    fireEvent.click(contextLinks()[0]);
    expect(pushed).toHaveLength(1);
    const url = new URL(pushed[0], 'http://peasant.invalid');
    if (decodeURIComponent(url.pathname) !== `/projects/${PROJECT_HASH}/sess_contextsource`) {
      throw new Error(`the current-parent link did not route to the exact stored target sess_contextsource: ${pushed[0]}`);
    }
    expect(url.search).toBe('');

    // Follow the pushed route for real: the target mounts through the same
    // production route with its own stored content.
    pathname = url.pathname;
    search = url.search;
    channelBySession.set('sess_contextsource', targetWire('sess_contextsource'));
    view.rerender(<ProjectsRouter />);

    await waitFor(() => expect(document.body.textContent).toContain('current target sess_contextsource'));
    expect(document.body.textContent).not.toContain('child request');
  });

  it('restores the child query, disclosure, scroll, and selection after Back from the target', async () => {
    const c = fixture.backCase;
    const view = mountChild(c);
    await waitForViewer();

    const earlierToggle = document.querySelector<HTMLButtonElement>('.txn-earlier-toggle');
    expect(earlierToggle).not.toBeNull();
    expect(earlierToggle?.getAttribute('aria-expanded')).toBe('false');
    fireEvent.click(earlierToggle as HTMLButtonElement);

    // The disclosure is READER state carried by the route: the host persists
    // exactly the disclosed section, so Back, a reload, and a copied link all
    // reopen it. The write must not touch the `turn` target, which is the
    // stream position the reader was at.
    await waitFor(() => expect(replaced.length).toBeGreaterThan(0));
    const disclosed = replaced.at(-1) as string;
    const disclosedRoute = new URL(disclosed, 'http://peasant.invalid');
    expect(disclosedRoute.searchParams.get('earlier')).toBe('earlier-0');
    expect(disclosedRoute.searchParams.get('origin')).toBe('Map');
    expect(disclosedRoute.searchParams.get('turn')).toBeNull();

    // The host's route write is what the browser applies, so a real Back lands
    // on the disclosed URL and the viewer reopens the section from it.
    search = disclosedRoute.search;
    view.rerender(<ProjectsRouter />);
    await waitFor(() => expect(document.querySelector<HTMLButtonElement>('.txn-earlier-toggle')?.getAttribute('aria-expanded')).toBe('true'));

    fireEvent.keyDown(window, { key: 'f', metaKey: true });
    const searchInput = await waitFor(() => {
      const input = document.querySelector<HTMLInputElement>('.txn-search-input');
      if (!input) throw new Error('search input did not open');
      return input;
    });
    fireEvent.change(searchInput, { target: { value: 'needle' } });
    if (searchInput.value !== 'needle') {
      throw new Error(
        `mounted context navigation invariant failed: the transcript search query was not applied through the host reading record (read ${JSON.stringify(searchInput.value)})`,
      );
    }

    const stream = document.querySelector<HTMLElement>('.txn-stream');
    if (!stream) throw new Error('transcript stream did not mount');
    stream.scrollTop = 120;
    fireEvent.scroll(stream);
    await waitFor(() =>
      expect(document.querySelector(".txn-turnwrap[data-turn='1'] .txn-turn.txn-active")).not.toBeNull(),
    );

    const childPathname = pathname;
    // Browser Back returns to the URL the host wrote when the reader disclosed
    // the retained history, so the disclosure comes back with the route.
    const childSearch = new URL(disclosed, 'http://peasant.invalid').search;
    fireEvent.click(contextLinks()[0]);
    if (pushed.length !== 1 || pushed[0] !== transcriptHref(PROJECT_HASH, 'sess_contextsource')) {
      throw new Error(`the current-parent link did not route to the exact stored target sess_contextsource: ${JSON.stringify(pushed)}`);
    }

    // Forward: the target's own route and content.
    const url = new URL(pushed[0], 'http://peasant.invalid');
    pathname = url.pathname;
    search = url.search;
    channelBySession.set('sess_contextsource', targetWire('sess_contextsource'));
    view.rerender(<ProjectsRouter />);
    await waitFor(() => expect(document.body.textContent).toContain('current target sess_contextsource'));

    // Back: the browser restores the child URL and re-mounts the child route.
    pathname = childPathname;
    search = childSearch;
    view.rerender(<ProjectsRouter />);
    await waitForViewer();
    await waitFor(() => expect(document.body.textContent).toContain('child request'));

    await restoredState(
      () => document.querySelector<HTMLButtonElement>('.txn-earlier-toggle')?.getAttribute('aria-expanded') ?? null,
      (value) => value === 'true',
      'the retained-history disclosure was not restored from the route on Back',
    );
    await restoredState(
      () => document.querySelector<HTMLElement>('.txn-stream')?.scrollTop ?? -1,
      (value) => value === 120,
      'the inner stream scroll offset was not restored from the child reading record on Back',
    );
    await restoredState(
      () => document.querySelector(".txn-turnwrap[data-turn='1'] .txn-turn.txn-active") !== null,
      (value) => value === true,
      'the selected turn was not restored from the child reading record on Back',
    );

    fireEvent.keyDown(window, { key: 'f', metaKey: true });
    const restoredInput = await waitFor(() => {
      const input = document.querySelector<HTMLInputElement>('.txn-search-input');
      if (!input) throw new Error('search input did not reopen');
      return input;
    });
    if (restoredInput.value !== 'needle') {
      throw new Error(
        `mounted context navigation invariant failed: the search query was not restored from the child reading record on Back (read ${JSON.stringify(restoredInput.value)})`,
      );
    }

    // The child's own route query survives the round trip: the origin crumb the
    // host derives from it is still mounted.
    expect(document.querySelector(`a[href="/map/${PROJECT_HASH}"]`)).not.toBeNull();
  });

  it('reopens the retained-history disclosure from a copied route in a fresh document', async () => {
    const c = caseNamed('context-and-starter-distinct');
    // A fresh document at the copied route has no in-memory state to rely on,
    // so the disclosure has to come from the URL the reader shared.
    pathname = `/projects/${PROJECT_HASH}/${CHILD_ID}`;
    search = '?origin=Map&earlier=earlier-0';
    channelBySession.set(CHILD_ID, childWire(c));
    render(<ProjectsRouter />);
    await waitForViewer();

    await restoredState(
      () => document.querySelector<HTMLButtonElement>('.txn-earlier-toggle')?.getAttribute('aria-expanded') ?? null,
      (value) => value === 'true',
      'the retained-history disclosure was not restored from the copied route',
    );
    expect(document.body.textContent).toContain(c.earlierTurnContent as string);
  });
});
