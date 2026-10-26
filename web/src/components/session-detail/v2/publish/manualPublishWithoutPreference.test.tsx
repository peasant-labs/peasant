import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import type { ReactNode } from 'react';
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { parseSessionDetailPayloadValue, type SessionDetailPayload } from '@peasant-labs/schema';
import { ProjectsRouter } from '@/app/projects/[[...segments]]/ProjectsRouter';
import { PublishProvider } from '@/contexts/PublishContext';
import { type ProjectHash } from '@/lib/navigation/projectRoutes';
import { createPublishWorld, loadPublishStates } from '@/app/share/testdata/publish-popup-states.mjs';

/**
 * The manual publish path must not depend on the optional automatic-setup
 * preference. A malformed `hooks.yaml` makes the server answer the settings
 * read with `500 settings_auto_publish_unreadable`; that must leave only the
 * automatic-setup offer unavailable, while a manual publish keeps its own
 * sign-in, audience and scan gates and creates no rule or hook.
 *
 * Mounted on the real transcript route (`ProjectsRouter` -> `SessionDetailV2`
 * -> fairtrade's publish parts) under the app-level publish provider. Only the
 * WebSocket channel, the project-identity read, the theme and the Local API are
 * replaced. The automatic-consent cases live in their own file; this one owns
 * the persistent preference failure.
 */

const FIXTURE_PATH = 'src/app/share/testdata/publish-popup-states.yaml';
const fixture = loadPublishStates(readFileSync(resolve(process.cwd(), FIXTURE_PATH), 'utf8'));

const PROJECT_HASH = 'b'.repeat(64) as ProjectHash;
const SESSION_ID = 'sess_publishstates';
const TITLE = 'Fix flaky ingest test';
/** A flow is several Local API round trips; give each wait room on a busy machine. */
const WAIT = { timeout: 3500 };
/** The server's refusal when the auto-publish rules cannot be read. */
const UNREADABLE_RULES = 'The settings could not be listed because the auto-publish rules could not be read in internal/api.handleGetSettings, and nothing was changed: the rules file is malformed.';

let pathname = `/projects/${PROJECT_HASH}/${SESSION_ID}`;
let search = '';
const fetchProjectResolution = vi.fn();

vi.mock('next/navigation', () => ({
  usePathname: () => pathname,
  useSearchParams: () => new URLSearchParams(search),
  useRouter: () => ({ push: vi.fn(), replace: vi.fn() }),
}));
vi.mock('@/contexts/WebSocketContext', () => ({
  useChannel: (sub: { id?: string; topic?: string }) => {
    if (sub?.id) return { data: sub.id === SESSION_ID ? sessionDetail : undefined, connected: true, error: null };
    return { data: { sessions: [QUALITY_SESSION] }, connected: true, error: null };
  },
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
vi.mock('@/app/share/ShareWizardClient', () => ({
  ShareWizardClient: () => <p>the multi-session wizard</p>,
}));

/** The quality row that carries the session's generated title. */
const QUALITY_SESSION = {
  id: SESSION_ID,
  title: TITLE,
  project: 'ingest-api',
  date: '2026-09-29',
  durationMinutes: 12,
  totalTokens: 1200,
  inputTokens: 700,
  outputTokens: 500,
  turnCount: 12,
  toolCalls: 0,
  filesTouched: 0,
  linesChanged: 0,
  retryLoops: 0,
  retryTokensWasted: 0,
  withinSessionReverts: 0,
  explorationRatio: 0,
  discoveryTurns: 0,
  scope: 'focused',
  scopeBreadth: 0,
  signalDensity: 0,
  specQualityScore: 0,
  outcome: 'resolved',
};

/** Twelve turns, one a minute: the publication fixture places publishedAt among them. */
const detail: SessionDetailPayload = (() => {
  const durable: SessionDetailPayload = {
    id: SESSION_ID,
    harness: 'claude-code',
    project: 'ingest-api',
    startTime: '2026-09-29T09:00:00.000Z',
    endTime: '2026-09-29T09:12:00.000Z',
    durationMins: 12,
    totalTokens: 1200,
    tokensIn: 700,
    tokensOut: 500,
    turnCount: 12,
    toolCallCount: 0,
    turns: Array.from({ length: 12 }, (_, index) => ({
      index,
      role: index % 2 === 0 ? 'user' : 'assistant',
      entryType: 'text',
      depth: 0,
      content: `turn ${index}`,
      timestamp: new Date(Date.parse('2026-09-29T09:00:00.000Z') + index * 60_000).toISOString(),
    })),
  };
  parseSessionDetailPayloadValue(durable);
  return durable;
})();

let sessionDetail: SessionDetailPayload | undefined = detail;

type World = ReturnType<typeof createPublishWorld>;

/**
 * The `ready-to-publish` world with one change: every settings read is refused
 * as if `hooks.yaml` were malformed, and every auto-publish rule save/install
 * is recorded. The refusal is persistent, so the retry cannot recover.
 */
function createUnreadablePreferenceWorld() {
  const entry = fixture.cases.find((candidate) => candidate.name === 'ready-to-publish');
  if (!entry) throw new Error('the publish states fixture no longer holds ready-to-publish');
  const world = createPublishWorld(fixture, entry, { sessionId: SESSION_ID, turns: detail.turns ?? [] });
  const ruleMutations: { method: string; path: string }[] = [];
  const respond = world.respond;
  world.respond = (request: { method: string; url: string; body?: unknown }) => {
    const path = new URL(request.url, 'http://peasant.local').pathname;
    if (path === '/api/v1/settings') {
      return { status: 500, json: { error: UNREADABLE_RULES, code: 'settings_auto_publish_unreadable' } };
    }
    if (path.startsWith('/api/v1/settings/auto-publish/')) {
      ruleMutations.push({ method: request.method, path });
      return { status: 200, json: {} };
    }
    return respond(request);
  };
  return { world, ruleMutations };
}

/** Serve the world's Local API through fetch; any other route fails the test. */
function installWorld(world: World) {
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    const answer = world.respond({
      method: (init?.method ?? 'GET').toUpperCase(),
      url,
      body: typeof init?.body === 'string' ? JSON.parse(init.body) : undefined,
    });
    // Answer after a macrotask, as a network does.
    await new Promise((settle) => setTimeout(settle, 5));
    if (answer === null) throw new Error(`unexpected fetch in the manual publish without preference test: ${init?.method ?? 'GET'} ${url}`);
    if ('pending' in answer) return new Promise<Response>(() => {});
    return 'json' in answer
      ? new Response(JSON.stringify(answer.json), { status: answer.status, headers: { 'Content-Type': 'application/json' } })
      : new Response(answer.text, { status: answer.status });
  });
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

function Page({ children }: { children?: ReactNode }) {
  return <PublishProvider>{children ?? <ProjectsRouter />}</PublishProvider>;
}

function bar(): HTMLElement {
  return screen.getByRole('group', { name: 'publish' });
}

function dialog(): HTMLElement {
  return screen.getByRole('dialog');
}

function primaryButton(): HTMLButtonElement | null {
  const foot = dialog().querySelector('.pub-foot');
  const buttons = [...(foot?.querySelectorAll<HTMLButtonElement>('.btn-primary') ?? [])];
  return buttons[buttons.length - 1] ?? null;
}

beforeEach(() => {
  sessionDetail = detail;
  pathname = `/projects/${PROJECT_HASH}/${SESSION_ID}`;
  search = '';
  fetchProjectResolution.mockReset();
  fetchProjectResolution.mockResolvedValue({ project: 'ingest-api', projectHash: PROJECT_HASH });
  if (typeof HTMLElement.prototype.scrollIntoView !== 'function') {
    Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', { configurable: true, value: () => undefined });
  }
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('manual publish with an unreadable automatic-publishing preference', () => {
  it('keeps publish available, shows the offer unavailable with its reason, and creates no rule', async () => {
    const { world, ruleMutations } = createUnreadablePreferenceWorld();
    installWorld(world);
    const user = userEvent.setup();

    render(<Page />);
    await screen.findByRole('group', { name: 'publish' }, WAIT);
    const action = await waitFor(() => within(bar()).getByRole('button', { name: /^(publish|update|manage)$/ }), WAIT);
    await user.click(action);
    await screen.findByRole('dialog', {}, WAIT);

    // The automatic-setup offer is unchecked and unavailable, and its reason is
    // discoverable in the popup alongside a retry.
    await waitFor(() => {
      const alert = within(dialog()).getByRole('alert');
      expect(alert).toHaveTextContent('automatic publishing is unavailable');
      expect(alert).toHaveTextContent('the auto-publish rules could not be read');
      expect(within(dialog()).getByRole('button', { name: 'retry' })).toBeInTheDocument();
    }, WAIT);
    const offer = within(dialog()).getByRole('checkbox', { name: 'publish this repo automatically on git push' }) as HTMLInputElement;
    expect(offer.checked).toBe(false);
    expect(ruleMutations).toEqual([]);

    // The manual action keeps its own gates and stays available.
    const primary = await waitFor(() => {
      const found = primaryButton();
      expect(found).not.toBeNull();
      expect(found).toBeEnabled();
      expect(found).not.toHaveAttribute('aria-disabled', 'true');
      return found as HTMLButtonElement;
    }, WAIT);
    await user.click(primary);

    await waitFor(() => expect(world.pushRequests).toHaveLength(1), WAIT);
    expect(world.pushRequests[0]).toMatchObject({ sessionIds: [SESSION_ID], redactionLevel: 'standard' });
    // Nothing about the fallback created a rule or installed a hook.
    expect(ruleMutations).toEqual([]);
    await waitFor(() => expect(bar().querySelector('.pub-state')).toHaveAttribute('data-state', 'published'), WAIT);
    expect(ruleMutations).toEqual([]);
  }, 15000);

  it('retries the preference read without blocking the manual action', async () => {
    const { world } = createUnreadablePreferenceWorld();
    installWorld(world);
    const user = userEvent.setup();

    render(<Page />);
    await screen.findByRole('group', { name: 'publish' }, WAIT);
    const action = await waitFor(() => within(bar()).getByRole('button', { name: /^(publish|update|manage)$/ }), WAIT);
    await user.click(action);
    await screen.findByRole('dialog', {}, WAIT);

    await waitFor(() => expect(within(dialog()).getByRole('alert')).toHaveTextContent('automatic publishing is unavailable'), WAIT);
    await user.click(within(dialog()).getByRole('button', { name: 'retry' }));
    // The refusal is persistent, so the offer stays unavailable and the manual
    // action stays enabled.
    await waitFor(() => expect(within(dialog()).getByRole('alert')).toHaveTextContent('automatic publishing is unavailable'), WAIT);
    const primary = primaryButton();
    expect(primary).not.toBeNull();
    expect(primary).toBeEnabled();
  }, 15000);
});
