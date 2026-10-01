import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import type { ReactNode } from 'react';
import { parseDocument } from 'yaml';
import { act, cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { parseSessionDetailPayloadValue, type SessionDetailPayload } from '@peasant-labs/schema';
import { ProjectsRouter } from '@/app/projects/[[...segments]]/ProjectsRouter';
import { SharePageClient } from '@/app/share/SharePageClient';
import { PublishProvider } from '@/contexts/PublishContext';
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
const readyScanSummary = fixture.cases.find((entry) => entry.name === 'ready-to-publish')?.expect.popup?.texts.find((text) => text.includes('matches · all redacted'));
if (!readyScanSummary) throw new Error('ready-to-publish must pin the scan occurrence summary');

const PROJECT_HASH = 'b'.repeat(64) as ProjectHash;
const SESSION_ID = 'sess_publishstates';
const TITLE = 'Fix flaky ingest test';
/** A flow is several Local API round trips; give each wait room on a busy machine. */
const WAIT = { timeout: 3500 };

let pathname = `/projects/${PROJECT_HASH}/${SESSION_ID}`;
let search = '';
const replaced: string[] = [];
const fetchProjectResolution = vi.fn();

vi.mock('next/navigation', () => ({
  usePathname: () => pathname,
  useSearchParams: () => new URLSearchParams(search),
  useRouter: () => ({
    push: vi.fn(),
    replace: (href: string) => { replaced.push(href); },
  }),
}));
vi.mock('@/contexts/WebSocketContext', () => ({
  useChannel: (sub: { id?: string; topic?: string }) => {
    if (sub?.id) return { data: sub.id === SESSION_ID ? sessionDetail : undefined, connected: true, error: null };
    // The quality channel names the session's generated title.
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

async function runStep(step: string, user: ReturnType<typeof userEvent.setup>) {
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
  await user.click(await within(dialog()).findByRole('button', { name: `${verb} ${rest.join(' ')}` }, WAIT));
}

beforeEach(() => {
  sessionDetail = detail;
  pathname = `/projects/${PROJECT_HASH}/${SESSION_ID}`;
  search = '';
  replaced.length = 0;
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
    for (const step of entry.steps) await runStep(step, user);

    const expected = entry.expect;
    await waitFor(() => {
      if ('alert' in expected.bar) {
        expect(within(bar()).getByRole('alert')).toHaveTextContent(expected.bar.alert);
        expect(within(bar()).queryByRole('button', { name: 'retry' }) !== null).toBe(expected.bar.retry);
      }
      else {
        const label = bar().querySelector<HTMLElement>('.pub-state');
        expect(label?.dataset.state).toBe(expected.bar.state);
        expect(label?.textContent).toBe(expected.bar.text);
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
    expect(await within(dialog()).findByText('checking for sensitive content', {}, WAIT)).toBeInTheDocument();
    await user.click(within(dialog()).getByRole('button', { name: 'cancel' }));

    // Leave the transcript (the provider stays, as the app shell does) and come back.
    view.rerender(<Page><p>another page</p></Page>);
    expect(screen.queryByRole('group', { name: 'publish' })).not.toBeInTheDocument();
    view.rerender(<Page />);
    await runStep('open', user);
    expect(await within(dialog()).findByText('checking for sensitive content', {}, WAIT)).toBeInTheDocument();
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
