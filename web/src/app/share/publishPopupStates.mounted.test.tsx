import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { createAutomaticConsentWorld, loadAutomaticConsent } from './testdata/publish-automatic-consent.mjs';
import { useEffect, type ComponentProps, type ReactNode } from 'react';
import { parseDocument } from 'yaml';
import { act, cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { parseSessionDetailPayloadValue, type SessionDetailPayload } from '@peasant-labs/schema';
import { ProjectsRouter } from '@/app/projects/[[...segments]]/ProjectsRouter';
import { SharePageClient } from '@/app/share/SharePageClient';
import { PublishProvider, usePublishState } from '@/contexts/PublishContext';
import * as reviewAdapter from '@/lib/share/reviewMatches';
import * as scans from '@/lib/share/redactions';
import { boundaryDomain, boundaryResponse, REDACTION_REVIEW_BOUNDARIES } from '@/test/fixtures/redaction-review-boundaries';
import type { PublishDialog } from '@/lib/ft-ui';
import { transcriptHref, type ProjectHash } from '@/lib/navigation/projectRoutes';
import {
  createPublishWorld,
  expectedPushBody,
  loadPublishStates,
} from './testdata/publish-popup-states.mjs';

/**
 * Every state of the publish bar and popup, mounted on the REAL transcript
 * route (`ProjectsRouter` -> `SessionDetailV2` -> fairtrade's `TranscriptViewer`
 * and publish parts) under the app-level publish provider. Only the WebSocket
 * channel, the project-identity read, the theme and the Local API are replaced;
 * the Local API answers come from the fixture's case, shaped to the contract
 * the page decodes them with.
 */

const FIXTURE_PATH = 'src/app/share/testdata/publish-popup-states.yaml';
const fixtureSource = readFileSync(resolve(process.cwd(), FIXTURE_PATH), 'utf8');
const fixture = loadPublishStates(fixtureSource);
const readyScanSummary = fixture.cases.find((entry) => entry.name === 'ready-to-publish')?.expect.popup?.texts.find((text) => text.includes('matches, all redacted'));
if (!readyScanSummary) throw new Error('ready-to-publish must pin the scan occurrence summary');

const PROJECT_HASH = 'b'.repeat(64) as ProjectHash;
const SESSION_ID = 'sess_publishstates';
const TITLE = 'Fix flaky ingest test';
/** A flow is several Local API round trips; give each wait room on a busy machine. */
const WAIT = { timeout: 3500 };

let pathname = `/projects/${PROJECT_HASH}/${SESSION_ID}`;
let search = '';
const replaced: string[] = [];
const pushed: string[] = [];
const fetchProjectResolution = vi.fn();
let latestDialogProps: ComponentProps<typeof PublishDialog>;
vi.mock('@/lib/ft-ui', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/lib/ft-ui')>();
  return { ...actual, PublishDialog: (props: ComponentProps<typeof PublishDialog>) => {
    latestDialogProps = props;
    return <actual.PublishDialog {...props} />;
  } };
});

vi.mock('next/navigation', () => ({
  usePathname: () => pathname,
  useSearchParams: () => new URLSearchParams(search),
  useRouter: () => ({
    push: (href: string) => { pushed.push(href); },
    replace: (href: string) => { replaced.push(href); },
  }),
}));
vi.mock('@/contexts/WebSocketContext', () => ({
  useChannel: (sub: { id?: string; topic?: string }) => {
    if (sub?.id) return { data: sub.id === SESSION_ID ? sessionDetail : undefined, connected: true, error: null, refresh: () => undefined };
    // The quality channel names the session's generated title.
    return { data: { sessions: [QUALITY_SESSION] }, connected: true, error: null, refresh: () => undefined };
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

describe('mounted category review boundary', () => {
  for (const entry of REDACTION_REVIEW_BOUNDARIES) {
    it(`${entry.name} guards publication and recovers through explicit re-scan`, async () => {
      const ready = fixture.cases.find((row) => row.name === 'ready-to-publish')!;
      const world = createPublishWorld(fixture, ready, { sessionId: SESSION_ID, turns: detail.turns ?? [] });
      const respond = world.respond;
      let response = boundaryResponse(entry.source === 'adapter' ? 'none' : entry.mutation);
      world.respond = (request) => {
        const result = respond(request);
        return request.url.includes('/sync/redactions') ? { status: 200, json: response } : result;
      };
      installWorld(world);
      const originalAdapter = reviewAdapter.reviewMatches;
      const adapter = entry.source === 'adapter' ? vi.spyOn(reviewAdapter, 'reviewMatches').mockImplementation((id, redactions) => {
        if (entry.mutation === 'none') throw new TypeError('injected conversion rejection');
        const matches = originalAdapter(id, redactions);
        if (matches[0]) Object.assign(matches[0], { category: 'LOCATION' });
        return matches;
      }) : null;
      const mock = entry.source === 'mock' ? vi.spyOn(scans, 'fetchRedactionScan').mockResolvedValue({ redactions: boundaryDomain(entry.mutation), matchCount: 4 }) : null;
      render(<Page><>{entry.source === 'cache' && <SeedScan mutation={entry.mutation} />}<ProjectsRouter /></></Page>);
      const user = userEvent.setup();
      await runStep('open', user);
      if (entry.error) {
        const expectedError = entry.error;
        await waitFor(() => expect(dialog()).toHaveTextContent(expectedError), WAIT);
        expect(primaryButton()).toBeDisabled();
        await act(async () => latestDialogProps.onPublish?.());
        await act(async () => latestDialogProps.onRetry?.());
        expect(world.pushRequests).toEqual([]);
        if (entry.source === 'cache') expect(world.scanRequests).toBe(0);
        response = boundaryResponse();
        adapter?.mockRestore();
        mock?.mockRestore();
        await runStep('rescan', user);
      }
      await waitFor(() => expect(primaryButton()).toBeEnabled(), WAIT);
      await user.click(within(dialog()).getByRole('button', { name: 'show matches' }));
      for (const item of boundaryDomain('none')) {
        const row = within(dialog()).getByText(item.originalText).closest('.rdx-card');
        expect(row).not.toBeNull();
        expect(row!.querySelector('.rdx-badge-cat')?.textContent).toBe(item.category);
      }
      expect(within(dialog()).queryByRole('button', { name: /^keep$/ })).not.toBeInTheDocument();
      const stalePublish = latestDialogProps.onPublish;
      response = boundaryResponse('unknown-item');
      // Invalidate before React has rendered the disabled state, then attempt
      // the formerly enabled callback rather than trusting the button alone.
      await act(async () => { latestDialogProps.onRescan?.(); stalePublish?.(); });
      await waitFor(() => expect(dialog()).toHaveTextContent('is not recognized'), WAIT);
      expect(primaryButton()).toBeDisabled();
      await act(async () => stalePublish?.());
      expect(world.pushRequests).toEqual([]);
      response = boundaryResponse();
      await runStep('rescan', user);
      await waitFor(() => expect(primaryButton()).toBeEnabled(), WAIT);
      await runStep('publish', user);
      await waitFor(() => expect(world.pushRequests).toHaveLength(1), WAIT);
    }, 15000);
  }
});

const scanFailures = parseDocument(readFileSync(resolve(process.cwd(), 'src/lib/share/testdata/scan-failures.yaml'), 'utf8')).toJS() as { cases: { name: string; detail: string }[] };

describe('stale local index', () => {
  const stale = scanFailures.cases.find((row) => row.name === 'stale-local-index')!;
  const envelope = JSON.parse(stale.detail.replace(/^redaction scan failed \(\d+\):\s*/, '')) as { error: string };
  const ready = fixture.cases.find((row) => row.name === 'ready-to-publish')!;

  /** A world whose scan refuses the stored index until the re-index answers `heals`. */
  function staleWorld(reindexAnswers: { status: number; json: unknown }[], heals: (answer: number) => boolean) {
    const world = createPublishWorld(fixture, ready, { sessionId: SESSION_ID, turns: detail.turns ?? [] });
    const respond = world.respond;
    const reindexRequests: string[] = [];
    let healed = false;
    world.respond = (request) => {
      if (request.url.includes('/sync/ingest/session')) {
        reindexRequests.push(`${request.method} ${new URL(request.url, 'http://local').searchParams.get('session_id')}`);
        const answer = reindexAnswers[Math.min(reindexRequests.length, reindexAnswers.length) - 1];
        healed = heals(reindexRequests.length);
        return answer;
      }
      // The popup never runs the whole-machine ingest.
      if (request.url.includes('/sync/ingest')) return null;
      if (request.url.includes('/sync/redactions') && !healed) return { status: 404, json: envelope };
      return respond(request);
    };
    return { world, reindexRequests };
  }

  it('re-indexes this one session when asked, names the step and what is slow, scans again and publishes', async () => {
    const { world, reindexRequests } = staleWorld([{ status: 200, json: { status: 'done', duration: '220ms', notes: [] } }], () => true);
    let release!: () => void;
    const held = new Promise<void>((settle) => { release = settle; });
    installWorld(world, { hold: (url) => (url.includes('/sync/ingest/session') ? held : undefined) });
    render(<Page />);
    const user = userEvent.setup();
    await runStep('open', user);
    await waitFor(() => expect(dialog()).toHaveTextContent('this session needs a quick re-index on this computer before it can be published.'), WAIT);
    // A failed scan stays cached: nothing re-indexes until the one action asks.
    expect(reindexRequests).toEqual([]);
    await user.click(within(dialog()).getByRole('button', { name: 're-index this session' }));
    await waitFor(() => expect(reindexRequests).toEqual([`POST ${SESSION_ID}`]), WAIT);
    // While it runs, the popup names the step and publish reads scanning….
    await waitFor(() => expect(latestDialogProps.checking).toEqual({ stage: 'updating this session’s index…' }), WAIT);
    expect(latestDialogProps.state).toBe('checking');
    expect(dialog()).toHaveTextContent('updating this session’s index…');
    expect(primaryButton()).toHaveTextContent('scanning…');
    expect(primaryButton()).toBeDisabled();
    // Ten seconds on one step: it says what is slow and that publish waits.
    const realNow = Date.now.bind(Date);
    const clock = vi.spyOn(Date, 'now').mockImplementation(() => realNow() + 12_000);
    await waitFor(() => expect(latestDialogProps.checking?.note).toMatch(/^taking longer than usual \(1[23]s\): peasant is reading this session’s recording again, on this computer\. publish waits for it\.$/), WAIT);
    expect(dialog()).toHaveTextContent('publish waits for it.');
    clock.mockRestore();
    release();
    await waitFor(() => expect(primaryButton()).toBeEnabled(), { timeout: 6000 });
    expect(latestDialogProps.checking).toBeUndefined();
    expect(primaryButton()).not.toHaveTextContent('scanning…');
    expect(dialog().textContent).not.toContain('retain unknown source evidence');
    expect(dialog().textContent).not.toContain('{"error"');
    await runStep('publish', user);
    await waitFor(() => expect(world.pushRequests).toHaveLength(1), WAIT);
    expect(reindexRequests).toHaveLength(1);
  }, 15000);

  it('says once that the copy stops before a line the re-index could not read, and still publishes', async () => {
    const reason = 'capture transcript source: capture JSONL record 50: malformed complete JSON; prior stored snapshot was retained';
    const { world, reindexRequests } = staleWorld([{ status: 200, json: { status: 'done', duration: '220ms', notes: [reason], unreadable: [{ line: 50, reason }] } }], () => true);
    installWorld(world);
    render(<Page />);
    const user = userEvent.setup();
    await runStep('open', user);
    await user.click(await within(dialog()).findByRole('button', { name: 're-index this session' }, WAIT));
    await waitFor(() => expect(primaryButton()).toBeEnabled(), { timeout: 6000 });
    expect(reindexRequests).toHaveLength(1);
    expect(latestDialogProps.scan?.note).toEqual({
      line: '1 line in this session couldn’t be read, so the copy being published stops before it.',
      detail: `line 50 of the recording: ${reason}`,
    });
    expect(dialog()).toHaveTextContent('1 line in this session couldn’t be read, so the copy being published stops before it.');
    await runStep('publish', user);
    await waitFor(() => expect(world.pushRequests).toHaveLength(1), WAIT);
  }, 15000);

  it('says in one line what stopped the re-index, keeps the reason in details, and offers to run it again', async () => {
    const busy = 'another index update is running on this computer; retry when it finishes';
    const { world, reindexRequests } = staleWorld([
      { status: 409, json: { error: busy, code: 'ingest_running' } },
      { status: 200, json: { status: 'done', duration: '220ms', notes: [] } },
    ], (answer) => answer >= 2);
    installWorld(world);
    render(<Page />);
    const user = userEvent.setup();
    await runStep('open', user);
    await user.click(await within(dialog()).findByRole('button', { name: 're-index this session' }, WAIT));
    await waitFor(() => expect(latestDialogProps.scan?.failure).toBe('the re-index did not finish, so publish is off. another index update is running on this computer; try again when it finishes.'), WAIT);
    expect(latestDialogProps.state).toBe('scan-failed');
    expect(latestDialogProps.scan?.failureDetail).toBe(`${busy}\n\n${envelope.error}`);
    expect(latestDialogProps.checking).toBeUndefined();
    expect(dialog().textContent).toContain('another index update is running on this computer; try again when it finishes.');
    expect(latestDialogProps.scan?.failureAction?.label).toBe('re-index again');
    expect(dialog().textContent).not.toContain('{"error"');
    expect(primaryButton()).toBeDisabled();
    await user.click(within(dialog()).getByRole('button', { name: 're-index again' }));
    await waitFor(() => expect(reindexRequests).toHaveLength(2), WAIT);
    await waitFor(() => expect(primaryButton()).toBeEnabled(), { timeout: 6000 });
  }, 15000);

  it('says the repair did not take when the scan still refuses the re-indexed session, with the run notes, and never retries on its own', async () => {
    const note = 'capture JSONL record 9: malformed complete JSON; prior stored snapshot was retained; repair this record and retry ingest';
    const { world, reindexRequests } = staleWorld([{ status: 200, json: { status: 'done', duration: '220ms', notes: [note] } }], () => false);
    installWorld(world);
    render(<Page />);
    const user = userEvent.setup();
    await runStep('open', user);
    await user.click(await within(dialog()).findByRole('button', { name: 're-index this session' }, WAIT));
    await waitFor(() => expect(latestDialogProps.scan?.failure).toBe('the re-index ran, but the scan still refuses this session, so publish is off. details say why.'), { timeout: 6000 });
    expect(latestDialogProps.scan?.failureAction).toBeUndefined();
    expect(latestDialogProps.state).toBe('scan-failed');
    expect(latestDialogProps.scan?.failureDetail).toBe(`${envelope.error}\n\n${note}`);
    expect(primaryButton()).toBeDisabled();
    await new Promise((settle) => setTimeout(settle, 200));
    expect(reindexRequests).toHaveLength(1);
  }, 15000);
});

describe('stopped publication retry', () => {
  it('guards a real stopped-write callback during re-scan, after refusal, and recovers', async () => {
    const entry = fixture.cases.find((row) => row.name === 'partial-publish-removes-reader')!;
    const world = createPublishWorld(fixture, entry, { sessionId: SESSION_ID, turns: detail.turns ?? [] });
    const respond = world.respond;
    let response = boundaryResponse();
    let release!: () => void;
    let held: Promise<void> | undefined;
    world.respond = (request) => request.url.includes('/sync/redactions') ? { status: 200, json: response } : respond(request);
    installWorld(world, { hold: (url) => url.includes('/sync/redactions') ? held : undefined });
    render(<Page />);
    const user = userEvent.setup();
    await runStep('open', user);
    await runStep('publish', user);
    await waitFor(() => expect(within(dialog()).getByRole('button', { name: 'retry' })).toBeEnabled(), WAIT);
    expect(world.pushRequests).toHaveLength(1);
    expect(latestDialogProps.onRecover).toBeUndefined();
    const staleRetry = latestDialogProps.onRetry;
    if (typeof staleRetry !== 'function') throw new Error('the real stopped-write retry callback was not installed');
    const rescan = latestDialogProps.onRescan;
    if (typeof rescan !== 'function') throw new Error('the stopped-write review has no rescan callback');
    held = new Promise<void>((done) => { release = done; });
    response = boundaryResponse('unknown-item');
    act(() => { rescan(); staleRetry(); });
    expect(world.pushRequests).toHaveLength(1);
    expect(primaryButton()).toBeDisabled();
    await act(async () => release());
    await waitFor(() => expect(dialog()).toHaveTextContent('is not recognized'), WAIT);
    expect(primaryButton()).toBeDisabled();
    await act(async () => staleRetry());
    expect(world.pushRequests).toHaveLength(1);
    response = boundaryResponse();
    held = undefined;
    await runStep('rescan', user);
    await waitFor(() => expect(within(dialog()).getByRole('button', { name: 'retry' })).toBeEnabled(), WAIT);
    expect(latestDialogProps.onRetry).toBeTypeOf('function');
    expect(latestDialogProps.onRecover).toBeUndefined();
    await user.click(within(dialog()).getByRole('button', { name: 'retry' }));
    await waitFor(() => expect(world.pushRequests).toHaveLength(2), WAIT);
    await waitFor(() => expect(within(dialog()).getByRole('button', { name: 'done' })).toBeEnabled(), WAIT);
  });
});

const recoveryFixture = parseDocument(readFileSync(resolve(process.cwd(), 'src/app/share/testdata/publish-review-recovery.yaml'), 'utf8')).toJS() as {
  requiredNames: string[];
  cases: { name: string; route: string; method: string; retry?: string; inline?: boolean }[];
};
describe('prerequisite recovery through the actual popup action', () => {
  it('retains every required recovery', () => {
    expect(new Set(recoveryFixture.cases.map((entry) => entry.name))).toEqual(new Set(recoveryFixture.requiredNames));
  });
  for (const recovery of recoveryFixture.cases) {
    it(`${recovery.name} restarts its intended operation without a push`, async () => {
      const entry = fixture.cases.find((row) => row.name === recovery.name)!;
      const world = createPublishWorld(fixture, entry, { sessionId: SESSION_ID, turns: detail.turns ?? [] });
      const fixed = createPublishWorld(fixture, {
        ...entry,
        signIn: recovery.name === 'login-start-failed' ? 'already-authenticated' : 'signed-in',
        village: fixture.collectives.map((collective) => collective.name),
        publication: { ...entry.publication, audienceRead: 'ok' },
      }, { sessionId: SESSION_ID, turns: detail.turns ?? [] });
      const respond = world.respond;
      let recovering = false;
      const operations: string[] = [];
      world.respond = (request) => {
        operations.push(`${request.method} ${new URL(request.url, 'http://peasant.local').pathname}`);
        return recovering ? fixed.respond(request) : respond(request);
      };
      installWorld(world);
      if (entry.steps.includes('arrive')) search = 'publish=open';
      render(<Page />);
      const user = userEvent.setup();
      for (const step of entry.steps) await runStep(step, user);
      const retry = await waitFor(() => {
        const button = within(dialog()).getByRole('button', { name: recovery.retry ?? 'retry' });
        expect(button).toBeEnabled();
        return button;
      }, WAIT);
      if (recovery.inline) {
        expect(latestDialogProps.accessFailure?.onRetry).toBeTypeOf('function');
        expect(latestDialogProps.state).not.toBe('stopped');
        expect(primaryButton()).toBeDisabled();
      } else {
        expect(latestDialogProps.onRecover).toBeTypeOf('function');
      }
      expect(latestDialogProps.onRetry).toBeUndefined();
      const operation = `${recovery.method} ${recovery.route}`;
      const before = operations.filter((value) => value === operation).length;
      expect(before).toBeGreaterThan(0);
      recovering = true;
      await user.click(retry);
      await waitFor(() => expect(operations.filter((value) => value === operation).length).toBeGreaterThan(before), WAIT);
      await waitFor(() => {
        expect(latestDialogProps.state).toBe('ready');
        expect(latestDialogProps.onRecover).toBeUndefined();
        expect(primaryButton()).toBeEnabled();
      }, WAIT);
      expect(world.pushRequests).toEqual([]);
      expect(fixed.pushRequests).toEqual([]);
    });
  }
});

type World = ReturnType<typeof createPublishWorld>;

interface WorldOptions {
  /** Answers a route the world does not serve; any other route fails the test. */
  extra?: (url: string) => Response | null;
  /** Holds a request until the returned promise settles. */
  hold?: (url: string) => Promise<void> | undefined;
}

/** Serve a case's Local API through fetch; any other route fails the test. */
function installWorld(world: World, options: WorldOptions = {}) {
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    const held = options.hold?.(url);
    const answer = world.respond({
      method: (init?.method ?? 'GET').toUpperCase(),
      url,
      body: typeof init?.body === 'string' ? JSON.parse(init.body) : undefined,
    });
    if (held) await held;
    // Answer after a macrotask, as a network does: a request answered within
    // the same turn would hide an effect that drops its own answer.
    await new Promise((settle) => setTimeout(settle, 5));
    if (answer === null) {
      const handled = options.extra?.(url);
      if (handled) return handled;
      throw new Error(`unexpected fetch in the mounted publish states test: ${init?.method ?? 'GET'} ${url}`);
    }
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

function SeedScan({ mutation }: { mutation: Parameters<typeof boundaryDomain>[0] }) {
  const { updateRedactionCache } = usePublishState();
  useEffect(() => updateRedactionCache((cache) => new Map(cache).set(`standard:${SESSION_ID}`, {
    status: 'success', version: String(detail.turns?.length), redactions: boundaryDomain(mutation),
  })), [mutation, updateRedactionCache]);
  return null;
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

async function runStep(step: string, user: ReturnType<typeof userEvent.setup>, keepRemoval = false) {
  if (step === 'arrive') return;
  if (step === 'open') {
    const action = await waitFor(() => within(bar()).getByRole('button', { name: /^(publish|update|manage)$/ }), WAIT);
    await user.click(action);
    await screen.findByRole('dialog', {}, WAIT);
    return;
  }
  if (step === 'connect') {
    await user.click(await within(dialog()).findByRole('button', { name: /continue with GitHub/ }, WAIT));
    return;
  }
  if (step === 'publish') {
    const button = await waitFor(() => {
      const found = primaryButton();
      expect(found).not.toBeNull();
      expect(found).toBeEnabled();
      expect(found).not.toHaveAttribute('aria-disabled', 'true');
      return found as HTMLButtonElement;
    }, WAIT);
    await user.click(button);
    return;
  }
  if (step === 'retry' || step === 'rescan') {
    await user.click(await within(dialog()).findByRole('button', { name: step === 'retry' ? 'retry' : /re-scan/ }, WAIT));
    return;
  }
  const [verb, ...rest] = step.split(' ');
  const button = await within(dialog()).findByRole('button', { name: `${verb} ${rest.join(' ')}` }, WAIT);
  const connectedBeforeClick = button.isConnected;
  await user.click(button);
  if (verb === 'remove') {
    await waitFor(() => {
      const evidence = JSON.stringify({ step, keepRemoval, connectedBeforeClick, connectedAfterClick: button.isConnected, popup: dialog().textContent });
      if (!keepRemoval) expect(within(dialog()).queryByRole('button', { name: `remove ${rest.join(' ')}` }), evidence).not.toBeInTheDocument();
      else expect(within(dialog()).queryByRole('button', { name: `keep ${rest.join(' ')}` }), evidence).toBeInTheDocument();
    }, WAIT);
  }
}

beforeEach(() => {
  sessionDetail = detail;
  pathname = `/projects/${PROJECT_HASH}/${SESSION_ID}`;
  search = '';
  replaced.length = 0;
  pushed.length = 0;
  fetchProjectResolution.mockReset();
  fetchProjectResolution.mockResolvedValue({ project: 'ingest-api', projectHash: PROJECT_HASH });
  if (typeof HTMLElement.prototype.scrollIntoView !== 'function') {
    Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', { configurable: true, value: () => undefined });
  }
});

afterEach(() => {
  vi.restoreAllMocks();
  cleanup();
  vi.unstubAllGlobals();
});

describe('publish states fixture', () => {
  it('is strict about its required states and its fields', () => {
    expect(() => loadPublishStates(fixtureSource.replace('  - waits-for-approval\n', ''))).toThrow(/is not a required state/);
    expect(() => loadPublishStates(fixtureSource.replace('- name: scan-failed', '- name: scan-broke'))).toThrow(/required state "scan-failed" has no case/);
    expect(() => loadPublishStates(fixtureSource.replace('    scan: pending\n', '    scan: pending\n    extra: true\n'))).toThrow(/unknown fields extra/);
    expect(() => loadPublishStates(fixtureSource.replace('steps: [open, add ML Reading Group, publish]', 'steps: [open, add Nobody, publish]'))).toThrow(/no collective named "Nobody"/);
  });
});

describe.each(fixture.cases)('publish state $name on /projects/[name]/[id]', (entry) => {
  it('mounts the bar and the popup the case expects', async () => {
    const world = createPublishWorld(fixture, entry, { sessionId: SESSION_ID, turns: detail.turns ?? [] });
    installWorld(world);
    const user = userEvent.setup();
    if (entry.steps.includes('arrive')) search = 'publish=open';
    render(<Page />);
    await screen.findByRole('group', { name: 'publish' }, WAIT);

    if ('alert' in entry.expect.bar) await within(bar()).findByRole('alert', {}, WAIT);
    else await waitFor(() => expect(bar().querySelector('.pub-state')).not.toBeNull(), WAIT);
    const actionSnapshots: { step: string; text: string }[] = [];
    for (const step of entry.steps) {
      await runStep(step, user, entry.expect.request?.remove.includes(step.replace(/^remove /, '')) ?? false);
      actionSnapshots.push({ step, text: screen.queryByRole('dialog')?.textContent ?? '' });
    }

    const expected = entry.expect;
    await waitFor(() => {
      if ('alert' in expected.bar) {
        expect(within(bar()).getByRole('alert')).toHaveTextContent(expected.bar.alert);
        expect(within(bar()).queryByRole('button', { name: 'retry' }) !== null).toBe(expected.bar.retry);
      }
      else {
        const label = bar().querySelector<HTMLElement>('.pub-state');
        expect(label?.dataset.state).toBe(expected.bar.state);
        expect(label?.textContent, JSON.stringify({ requests: world.pushRequests, actions: actionSnapshots })).toBe(expected.bar.text);
      }
    }, WAIT);
    if (!('alert' in expected.bar)) expect(within(bar()).getByRole('button', { name: expected.bar.action })).toBeInTheDocument();
    // The viewer's own tail and outcome chip are off: the bar is the header's only action.
    expect(screen.queryByRole('button', { name: /^share$/ })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: /search this transcript/ })).toBeInTheDocument();

    if (expected.popup === null) {
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
      expect(world.pushRequests).toEqual([]);
      return;
    }
    const popup = expected.popup;
    await waitFor(() => {
      const heading = document.getElementById(dialog().getAttribute('aria-labelledby') ?? '');
      expect(heading?.textContent).toBe(popup.heading.replace('{title}', TITLE));
      for (const text of popup.texts) expect(dialog().textContent).toContain(text);
      if (popup.link) expect(within(dialog()).getByRole('link', { name: popup.link.text })).toHaveAttribute('href', popup.link.href);
      const primary = primaryButton();
      if (popup.primary === null) {
        expect(primary).toBeNull();
      } else {
        expect(primary?.textContent?.trim()).toBe(popup.primary.label);
        expect(primary?.disabled === true || primary?.getAttribute('aria-disabled') === 'true').toBe(!popup.primary.enabled);
      }
    }, WAIT);
    const pushBody = expectedPushBody(fixture, entry, SESSION_ID);
    if (pushBody === null) expect(world.pushRequests).toEqual([]);
    else expect(world.pushRequests.at(-1)).toEqual(pushBody);
  }, 15000);
});

describe('the app-level scan cache under the transcript page', () => {
  function caseNamed(name: string) {
    const found = fixture.cases.find((entry) => entry.name === name);
    if (!found) throw new Error(`fixture case ${name} is missing`);
    return found;
  }

  it('scans an arrival link only after the session detail is loaded', async () => {
    const world = createPublishWorld(fixture, caseNamed('ready-to-publish'), { sessionId: SESSION_ID, turns: detail.turns ?? [] });
    const fetchMock = installWorld(world);
    sessionDetail = undefined;
    search = 'publish=open';
    const view = render(<Page />);
    await waitFor(() => expect(fetchMock).toHaveBeenCalled(), WAIT);
    expect(world.scanRequests).toBe(0);
    sessionDetail = detail;
    view.rerender(<Page />);
    await waitFor(() => expect(dialog().textContent).toContain(readyScanSummary), WAIT);
    expect(world.scanRequests).toBe(1);
  });

  it('starts no second scan when the reader leaves and returns during a scan, and shows its result', async () => {
    const world = createPublishWorld(fixture, caseNamed('ready-to-publish'), { sessionId: SESSION_ID, turns: detail.turns ?? [] });
    let releaseScan!: () => void;
    const scanGate = new Promise<void>((resolveGate) => { releaseScan = resolveGate; });
    installWorld(world, { hold: (url) => (url.includes('/api/v1/sync/redactions') ? scanGate : undefined) });
    const user = userEvent.setup();

    const view = render(<Page />);
    await runStep('open', user);
    expect(await within(dialog()).findByText('scanning 12 turns…', {}, WAIT)).toBeInTheDocument();
    await user.click(within(dialog()).getByRole('button', { name: 'cancel' }));

    // Leave the transcript (the provider stays, as the app shell does) and come back.
    view.rerender(<Page><p>another page</p></Page>);
    expect(screen.queryByRole('group', { name: 'publish' })).not.toBeInTheDocument();
    view.rerender(<Page />);
    await runStep('open', user);
    expect(await within(dialog()).findByText('scanning 12 turns…', {}, WAIT)).toBeInTheDocument();
    expect(world.scanRequests).toBe(1);

    await act(async () => { releaseScan(); });
    await waitFor(() => expect(dialog().textContent).toContain(readyScanSummary), WAIT);
    expect(world.scanRequests).toBe(1);
  });

  it('remembers a failed scan across closing the popup, keeps publish off, and scans again only on re-scan', async () => {
    const world = createPublishWorld(fixture, caseNamed('scan-failed'), { sessionId: SESSION_ID, turns: detail.turns ?? [] });
    installWorld(world);
    const user = userEvent.setup();
    render(<Page />);

    await runStep('open', user);
    await waitFor(() => expect(dialog().textContent).toContain('the scan failed, so publish is off.'), WAIT);
    expect(primaryButton()).toBeDisabled();
    await user.click(within(dialog()).getByRole('button', { name: 'cancel' }));

    await runStep('open', user);
    await waitFor(() => expect(dialog().textContent).toContain('the scan failed, so publish is off.'), WAIT);
    expect(primaryButton()).toBeDisabled();
    expect(world.scanRequests).toBe(1);

    await user.click(within(dialog()).getByRole('button', { name: /re-scan/ }));
    await waitFor(() => expect(world.scanRequests).toBe(2), WAIT);
    await waitFor(() => expect(dialog().textContent).toContain('the scan failed, so publish is off.'), WAIT);
    expect(primaryButton()).toBeDisabled();
  });
});

describe('/share?sessionId opens the publish popup on that transcript', () => {
  it('sends a one-session link to the transcript with its popup open, and closing drops the request', async () => {
    const world = createPublishWorld(fixture, fixture.cases.find((entry) => entry.name === 'ready-to-publish')!, { sessionId: SESSION_ID, turns: detail.turns ?? [] });
    installWorld(world, {
      extra: (url) => url.includes('/api/v1/session-summaries')
        ? new Response(JSON.stringify({ sessions: [{ id: SESSION_ID, harness: 'claude-code', startTime: '2026-09-29T09:00:00.000Z', durationMins: 12, totalTokens: 1200, turnCount: 12, toolCallCount: 0, project: 'ingest-api', projectHash: PROJECT_HASH }] }), { status: 200 })
        : null,
    });
    const user = userEvent.setup();

    pathname = '/share';
    search = `sessionId=${SESSION_ID}`;
    const share = render(<Page><SharePageClient /></Page>);
    const target = transcriptHref(PROJECT_HASH, SESSION_ID, { publish: true });
    await waitFor(() => expect(replaced).toEqual([target]), WAIT);
    expect(target).toBe(`/projects/${PROJECT_HASH}/${SESSION_ID}?publish=open`);
    share.unmount();

    pathname = `/projects/${PROJECT_HASH}/${SESSION_ID}`;
    search = 'publish=open';
    replaced.length = 0;
    render(<Page />);
    await waitFor(() => expect(dialog().textContent).toContain(readyScanSummary), WAIT);
    await user.click(within(dialog()).getByRole('button', { name: 'cancel' }));
    expect(replaced).toEqual([`/projects/${PROJECT_HASH}/${SESSION_ID}`]);
  });

  it('keeps the multi-session wizard for a link with a step or an evidence set', () => {
    pathname = '/share';
    expect(fixture.wizardLinks.length).toBeGreaterThan(0);
    for (const link of fixture.wizardLinks) {
      search = link.replace('{session}', SESSION_ID);
      const view = render(<Page><SharePageClient /></Page>);
      expect(screen.getByText('the multi-session wizard')).toBeInTheDocument();
      view.unmount();
    }
    expect(replaced).toEqual([]);
  });
});

// A direct draft-unit test cannot observe a real control click between the DOM
// readiness commit and the passive initializer. Hold that read, then dispatch
// the mounted button's native click at the actual picker commit. Each test owns
// and disconnects its observer and releases its held response even on failure.
interface AudienceReadinessCase {
  name: string;
  base: string;
  delayedRoute: string;
  readinessSelector: string;
  removeButton: string;
}
function audienceReadinessCases(): AudienceReadinessCase[] {
  const source = readFileSync(resolve(process.cwd(), 'src/app/share/testdata/publish-popup-readiness.yaml'), 'utf8');
  const doc = parseDocument(source, { strict: true, uniqueKeys: true });
  if (doc.errors.length || /^---\s*$/m.test(source)) throw new Error('audience readiness fixture requires one valid YAML document');
  const root = doc.toJS();
  const required = ['an-audience-removal-survives-collective-readiness'];
  if (!root || Object.keys(root).sort().join() !== 'cases,requiredNames' || JSON.stringify(root.requiredNames) !== JSON.stringify(required) || !Array.isArray(root.cases)) throw new Error('audience readiness fixture requires its named case');
  for (const row of root.cases) {
    if (!row || Object.keys(row).sort().join() !== 'base,delayedRoute,name,readinessSelector,removeButton'
      || Object.values(row).some((value) => typeof value !== 'string' || !value)
      || !fixture.cases.some((entry) => entry.name === row.base)
      || !row.delayedRoute.startsWith('/api/v1/') || !row.readinessSelector || !row.removeButton.startsWith('remove ')) throw new Error('invalid audience readiness fixture case');
  }
  if (JSON.stringify(root.cases.map((row: AudienceReadinessCase) => row.name)) !== JSON.stringify(required)) throw new Error('audience readiness case names differ');
  return root.cases;
}
for (const row of audienceReadinessCases()) {
  it(row.name, async () => {
    const entry = fixture.cases.find((candidate) => candidate.name === row.base)!;
    const world = createPublishWorld(fixture, entry, { sessionId: SESSION_ID, turns: detail.turns ?? [] });
    let release!: () => void;
    const held = new Promise<void>((resolve) => { release = resolve; });
    installWorld(world, { hold: (url) => url.includes(row.delayedRoute) ? held : undefined });
    const user = userEvent.setup();
    let observer: MutationObserver | undefined;
    let clicked = false;
    try {
      render(<Page />);
      await runStep('open', user);
      await within(dialog()).findByRole('button', { name: row.removeButton }, WAIT);
      observer = new MutationObserver(() => {
        const ready = document.querySelector(row.readinessSelector);
        if (clicked || !ready) return;
        const remove = within(dialog()).getByRole('button', { name: row.removeButton });
        clicked = true;
        remove.click();
      });
      observer.observe(document.body, { childList: true, subtree: true });
      release();
      await waitFor(() => expect(clicked).toBe(true), WAIT);
      await runStep('publish', user);
      await waitFor(() => expect(world.pushRequests.length).toBe(1), WAIT);
      expect(world.pushRequests).toEqual([expectedPushBody(fixture, entry, SESSION_ID)]);
      const expectedBarText = 'text' in entry.expect.bar ? entry.expect.bar.text : '';
      await waitFor(() => expect(bar().querySelector('.pub-state')).toHaveTextContent(expectedBarText), WAIT);
    } finally {
      observer?.disconnect();
      release();
    }
  });
}

// These cases reuse the mounted production route and its contract-decoded API.
// The app-level provider is essential: a unit call cannot observe navigation
// while hook installation is pending or the checkbox's explicit confirmation.
const automaticSource = readFileSync(resolve(process.cwd(), 'src/app/share/testdata/publish-automatic-consent.yaml'), 'utf8');
const automaticFixture = loadAutomaticConsent(automaticSource, fixture);

describe.each(automaticFixture.cases)('automatic publishing consent: $name', (entry) => {
  it('uses the actual checkbox, confirmed publish, and server repository', async () => {
    const { world, mutations, order, base } = createAutomaticConsentWorld(fixture, automaticFixture, entry, { sessionId: SESSION_ID, turns: detail.turns ?? [] });
    let releaseInstall!: () => void;
    const installation = new Promise<void>((release) => { releaseInstall = release; });
    installWorld(world, { hold: (url) => entry.leave && url.endsWith('/install') ? installation : undefined });
    const user = userEvent.setup();
    const view = render(<Page />);
    await runStep('open', user);
    if (entry.intent === 'error') {
      await waitFor(() => expect(within(dialog()).getByRole('alert')).toHaveTextContent('settings read unavailable'), WAIT);
      expect(world.pushRequests).toEqual([]);
      expect(mutations).toEqual([]);
      await runStep('retry', user);
    }
    if (entry.expect === 'existing') {
      await waitFor(() => expect(primaryButton()).toBeEnabled(), WAIT);
      expect(within(dialog()).queryByRole('checkbox')).not.toBeInTheDocument();
      await user.click(within(dialog()).getByRole('button', { name: 'cancel' }));
      await user.click(within(bar()).getByRole('button', { name: 'more' }));
      await user.click(screen.getByRole('menuitem', { name: 'automatic publishing settings' }));
      expect(pushed).toEqual(['/settings']);
      expect(mutations).toEqual([]);
      expect(world.pushRequests).toEqual([]);
      return;
    }
    const checkbox = await within(dialog()).findByRole('checkbox', { name: 'publish this repo automatically on git push' }, WAIT);
    expect((checkbox as HTMLInputElement).checked).toBe(entry.intent === true);
    if (entry.action === 'toggle') await user.click(checkbox);
    for (const name of entry.add ?? []) await runStep(`add ${name}`, user);
    expect(mutations).toEqual([]);
    expect(world.pushRequests).toEqual([]);
    await runStep('publish', user);
    if (entry.leave) {
      await waitFor(() => expect(mutations.filter((item) => item.method === 'POST')).toHaveLength(1), WAIT);
      view.rerender(<Page><p>another page</p></Page>);
      view.rerender(<Page />);
      await waitFor(() => expect(bar().querySelector('.pub-state')).toHaveAttribute('data-state', 'publishing'), WAIT);
      const busyAction = within(bar()).getByRole('button', { name: 'publish' });
      expect(busyAction).toHaveAttribute('aria-disabled', 'true');
      await user.click(busyAction);
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
      expect(world.pushRequests).toHaveLength(1);
      expect(mutations).toHaveLength(2);
      await act(async () => { releaseInstall(); });
    }
    if (entry.expect === 'manual' || entry.expect === 'automatic') {
      await waitFor(() => expect(bar().querySelector('.pub-state')).toHaveAttribute('data-state', entry.expect === 'automatic' ? 'auto-publish' : 'published'), WAIT);
    } else if (entry.expect === 'refused') {
      await waitFor(() => expect(within(dialog()).getByRole('alert')).toHaveTextContent(base.expect.popup!.texts[0]), WAIT);
      expect(mutations).toEqual([]);
    } else {
      await waitFor(() => expect(within(dialog()).getByRole('alert')).toHaveTextContent(entry.expect), WAIT);
      expect(within(dialog()).getByRole('alert')).toHaveTextContent('the transcript was published');
      expect(within(dialog()).queryByRole('button', { name: 'retry' })).not.toBeInTheDocument();
      await waitFor(() => expect(bar().querySelector('.pub-state')).toHaveAttribute('data-state', 'published'), WAIT);
      await user.click(within(dialog()).getByRole('button', { name: 'cancel' }));
      await user.click(within(bar()).getByRole('button', { name: 'more' }));
      await user.click(screen.getByRole('menuitem', { name: 'automatic publishing settings' }));
      expect(pushed).toEqual(['/settings']);
    }
    expect(world.pushRequests).toHaveLength(1);
    if (entry.expect === 'manual') expect(mutations).toEqual([]);
    else if (entry.expect !== 'refused') {
      expect(mutations[0]?.method).toBe('PUT');
      expect(mutations[0]?.body).toEqual({ sessionId: SESSION_ID, events: ['pre-push'], collectives: [...fixture.collectives.slice(0, 2), ...(entry.add ?? []).map((name) => fixture.collectives.find((item) => item.name === name)!)].map((item) => item.id) });
      if (entry.add?.length) expect(dialog().textContent).toContain(base.expect.popup!.texts[0]);
      expect(mutations[0]?.path.split('/').at(-1)).toMatch(/^[0-9a-f-]{36}$/);
      const failedSave = entry.setup === 'save-error' || entry.setup === 'malformed' || entry.setup.startsWith('wrong-');
      expect(mutations.map((item) => item.method)).toEqual(failedSave ? ['PUT'] : ['PUT', 'POST']);
      expect(order).toEqual(failedSave ? ['push', 'PUT'] : ['push', 'PUT', 'POST']);
      if (!failedSave) {
        expect(mutations[1].path).toBe(`${mutations[0].path}/install`);
        expect(mutations[1].body).toEqual({ path: automaticFixture.repository });
      }
    }
  }, 15000);
});
