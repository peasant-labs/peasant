import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import type { ReactNode } from 'react';
import {
  parseStrictYAML,
  requireExactFields,
  requireExactRequiredFields,
  requireRecord,
  requireUniqueNames,
} from '@/test/strictYaml';
import { LayoutShell } from './LayoutShell';
import { OFFLINE_ANNOUNCEMENTS, startCommandFor } from './LocalOfflineNotice';
import { loadShellHeaderManifest } from '../../scripts/visual/shell-header-manifest.mjs';
import YAML from 'yaml';

vi.mock('next/navigation', () => ({ usePathname: () => '/', useRouter: () => ({ push: vi.fn() }) }));
vi.mock('@/hooks/useTheme', () => ({ useTheme: () => ({ theme: 'dark', toggle: vi.fn() }) }));
// The tour stays in the tree but is not mounted: a mount of its provider would render this marker.
vi.mock('@/components/tour/TourProvider', () => ({
  TourProvider: ({ children }: { children: ReactNode }) => <div data-testid="tour-provider">{children}</div>,
}));

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

type HealthMode = 'up' | 'down' | 'error' | 'hang';
type Step =
  | { health: HealthMode }
  | { socket: 'open' | 'close' }
  | { wait: number }
  | { retry: true }
  | { focus: 'retry' | 'away' }
  | { click: 'page' }
  | { expect: 'shown' | 'hidden' }
  | { checkedAt: number }
  | { sockets: number }
  | { healthChecks: number }
  | { announce: 'stopped' | 'still' | 'back' | 'none' }
  | { focused: 'main' | 'elsewhere' }
  | { mainFocusable: boolean };

const STEP_KEYS = ['health', 'socket', 'wait', 'retry', 'focus', 'click', 'expect', 'checkedAt', 'sockets', 'healthChecks', 'announce', 'focused', 'mainFocusable'] as const;
const REQUIRED_CASES = [
  'reachable-app-shows-nothing',
  'first-connect-within-grace-shows-nothing',
  'stopped-app-shows-at-once',
  'error-status-counts-as-stopped',
  'unanswered-check-times-out',
  'socket-blip-reconnects-without-notice',
  'socket-down-past-grace-shows',
  'recheck-reconnects-at-once',
  'returning-socket-clears-a-failed-check',
  'late-failure-loses-to-reconnected-socket',
  'try-again-reconnects-at-once',
  'failed-try-again-stays-usable',
  'second-outage-keeps-grace',
  'unanswered-retry-does-not-reconnect',
  'stale-check-loses-to-newer-answer',
  'try-again-within-grace-trusts-the-answer',
  'moot-try-again-stays-quiet',
  'try-again-superseded-by-an-answer-stays-quiet',
  'return-during-the-repeat-clear-stays-back',
  'click-after-try-again-leaves-focus-alone',
];
/** The ports the start-command rows must cover: none, the default, and at least one other. */
const REQUIRED_PORTS = ['', '8690'];

function loadFixture() {
  const path = resolve(process.cwd(), 'src/components/testdata/local_offline_notice.yaml');
  const root = requireRecord(parseStrictYAML(readFileSync(path, 'utf8'), 'local offline notice fixture'), 'local offline notice fixture');
  requireExactRequiredFields(root, ['startCommands', 'cases'], 'local offline notice fixture');
  if (!Array.isArray(root.startCommands) || !Array.isArray(root.cases)) throw new Error('local offline notice fixture needs startCommands and cases lists');
  const startCommands = root.startCommands.map((value, index) => {
    const row = requireRecord(value, `startCommands[${index}]`);
    requireExactRequiredFields(row, ['port', 'command'], `startCommands[${index}]`);
    if (typeof row.port !== 'string' || typeof row.command !== 'string') throw new Error(`startCommands[${index}] needs string port and command`);
    return { port: row.port, command: row.command };
  });
  const ports = startCommands.map((row) => row.port);
  const missingPorts = REQUIRED_PORTS.filter((port) => !ports.includes(port));
  if (missingPorts.length || !ports.some((port) => !REQUIRED_PORTS.includes(port))) {
    throw new Error('local offline notice fixture.startCommands must cover no port, 8690, and a non-default port');
  }
  const cases = root.cases.map((value, index) => requireRecord(value, `cases[${index}]`));
  requireUniqueNames(cases, 'local offline notice fixture.cases');
  const names = new Set(cases.map((row) => row.name));
  const missing = REQUIRED_CASES.filter((name) => !names.has(name));
  if (missing.length) throw new Error(`local offline notice fixture is missing required cases: ${missing.join(', ')}`);
  return {
    startCommands,
    cases: cases.map((row, index) => {
      requireExactRequiredFields(row, ['name', 'steps'], `cases[${index}]`);
      if (!Array.isArray(row.steps) || row.steps.length === 0) throw new Error(`cases[${index}].steps must be a non-empty list`);
      const steps = row.steps.map((value, stepIndex) => {
        const step = requireRecord(value, `cases[${index}].steps[${stepIndex}]`);
        requireExactFields(step, STEP_KEYS, `cases[${index}].steps[${stepIndex}]`);
        if (Object.keys(step).length !== 1) throw new Error(`cases[${index}].steps[${stepIndex}] must hold exactly one action`);
        return step as Step;
      });
      if (!steps.some((step) => 'expect' in step)) throw new Error(`cases[${index}] asserts nothing`);
      return { name: row.name as string, steps };
    }),
  };
}

const fixture = loadFixture();
/** The pinned-notice class, as the shell geometry fixture records it. */
const NOTICE_PINNED_CLASS = (
  YAML.parse(readFileSync(resolve(process.cwd(), 'src/components/testdata/app-shell-geometry.yaml'), 'utf8')) as { noticePinnedClass: string }
).noticePinnedClass;

// ---------------------------------------------------------------------------
// Doubles: the browser WebSocket and the health route
// ---------------------------------------------------------------------------

class MockWebSocket {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSING = 2;
  static readonly CLOSED = 3;
  static instances: MockWebSocket[] = [];
  onopen: ((ev: Event) => void) | null = null;
  onmessage: ((ev: MessageEvent) => void) | null = null;
  onclose: (() => void) | null = null;
  onerror: (() => void) | null = null;
  readyState = MockWebSocket.CONNECTING;
  constructor() {
    MockWebSocket.instances.push(this);
  }
  send(): void {}
  close(): void {
    this.readyState = MockWebSocket.CLOSED;
  }
}

let health: HealthMode = 'up';
let healthChecks = 0;
let originalWebSocket: typeof globalThis.WebSocket;

function healthResponse(signal: AbortSignal | null | undefined): Promise<Response> {
  healthChecks += 1;
  if (health === 'up') return Promise.resolve(Response.json({ status: 'ok' }));
  if (health === 'error') return Promise.resolve(new Response('unavailable', { status: 503 }));
  if (health === 'down') return Promise.reject(new TypeError('Failed to fetch'));
  // hang: never answers; only the page's own timeout ends it.
  return new Promise<Response>((_resolve, reject) => {
    signal?.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')));
  });
}

beforeEach(() => {
  vi.useFakeTimers();
  MockWebSocket.instances = [];
  health = 'up';
  healthChecks = 0;
  originalWebSocket = globalThis.WebSocket;
  // @ts-expect-error — a scripted stand-in for the browser WebSocket.
  globalThis.WebSocket = MockWebSocket;
  vi.stubGlobal('fetch', vi.fn(async (input: string | URL, init?: RequestInit) => {
    const url = new URL(String(input), 'http://localhost');
    if (url.pathname === '/api/v1/health') return healthResponse(init?.signal);
    if (url.pathname === '/api/v1/config/capabilities') return Response.json({ uiCapabilities: [] });
    throw new Error(`unexpected test request ${url.pathname}`);
  }));
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  globalThis.WebSocket = originalWebSocket;
});

const newestSocket = () => {
  const socket = MockWebSocket.instances.at(-1);
  if (!socket) throw new Error('the provider has opened no socket');
  return socket;
};

async function advance(ms: number) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
    for (let i = 0; i < 10; i += 1) await Promise.resolve();
  });
}

const notice = () => screen.queryByRole('region', { name: 'peasant is not running' });
const escapeRegExp = (text: string) => text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
const liveRegion = () => {
  const region = [...document.querySelectorAll('p[role="status"]')].find((element) => element.classList.contains('sr-only'));
  if (!region) throw new Error('the always-mounted offline live region is missing');
  return region;
};

function expectShown() {
  const region = notice();
  expect(region, 'the offline notice must show').not.toBeNull();
  // Under the fixed header: absolute at the top of the page by default, pinned (fixed) only
  // through the notice-pinned variant on a screen with room for it.
  const header = document.querySelector('header');
  expect(header?.className).toContain('fixed');
  expect(header?.contains(region)).toBe(false);
  const wrapper = region!.parentElement!;
  expect(wrapper.classList.contains('absolute')).toBe(true);
  expect(wrapper.classList.contains('top-[var(--nav-h)]')).toBe(true);
  expect(wrapper.classList.contains('fixed')).toBe(false);
  expect(wrapper.classList.contains(NOTICE_PINNED_CLASS)).toBe(true);
  // It names this computer, never the internet, and offers the way back.
  expect(within(region!).getByRole('status')).toHaveTextContent(
    "peasant isn't running on this computer. your internet is fine: this page talks to the peasant app on your machine.",
  );
  // The start command is for the port this page came from.
  expect(within(region!).getByText(startCommandFor(window.location.port))).toBeInTheDocument();
  expect(within(region!).getByRole('button', { name: 'try again' })).toBeInTheDocument();
  // Page content starts below it while it shows.
  expect(document.documentElement.style.getPropertyValue('--app-notice-height')).toMatch(/^\d+(\.\d+)?px$/);
}

function expectHidden() {
  expect(notice(), 'the offline notice must not show').toBeNull();
  expect(document.documentElement.style.getPropertyValue('--app-notice-height')).toBe('');
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

describe('LocalOfflineNotice', () => {
  it.each(fixture.cases.map((row) => [row.name, row.steps] as const))('%s', async (_name, steps) => {
    const mountedAt = Date.now();
    render(
      <LayoutShell>
        {/* As app/layout.tsx mounts it: not focusable at rest. */}
        <main>body</main>
      </LayoutShell>,
    );
    await advance(0);
    for (const step of steps) {
      if ('health' in step) health = step.health;
      else if ('socket' in step) {
        const socket = newestSocket();
        await act(async () => {
          if (step.socket === 'open') {
            socket.readyState = MockWebSocket.OPEN;
            socket.onopen?.(new Event('open'));
          } else {
            socket.readyState = MockWebSocket.CLOSED;
            socket.onclose?.();
          }
        });
      } else if ('wait' in step) await advance(step.wait);
      else if ('retry' in step) fireEvent.click(screen.getByRole('button', { name: 'try again' }));
      else if ('focus' in step) {
        if (step.focus === 'retry') act(() => screen.getByRole('button', { name: 'try again' }).focus());
        else act(() => (document.activeElement as HTMLElement | null)?.blur());
      } else if ('click' in step) {
        const main = document.querySelector('main');
        if (!main) throw new Error('no <main> to click');
        fireEvent.pointerDown(main);
        act(() => (document.activeElement as HTMLElement | null)?.blur());
      } else if ('mainFocusable' in step) expect(document.querySelector('main')?.hasAttribute('tabindex')).toBe(step.mainFocusable);
      else if ('checkedAt' in step) {
        expect(notice()?.querySelector('time')?.getAttribute('datetime')).toBe(new Date(mountedAt + step.checkedAt).toISOString());
      }
      else if ('sockets' in step) expect(MockWebSocket.instances).toHaveLength(step.sockets);
      else if ('healthChecks' in step) expect(healthChecks).toBe(step.healthChecks);
      else if ('announce' in step) {
        const text = liveRegion().textContent ?? '';
        if (step.announce === 'none') expect(text).toBe('');
        else if (step.announce === 'still') expect(text).toMatch(new RegExp(`^${escapeRegExp(OFFLINE_ANNOUNCEMENTS.stillStopped)} \\d{2}:\\d{2}:\\d{2}\\.$`));
        else expect(text).toBe(OFFLINE_ANNOUNCEMENTS[step.announce]);
      } else if ('focused' in step) {
        if (step.focused === 'main') expect(document.activeElement?.tagName).toBe('MAIN');
        else expect(document.activeElement?.tagName).not.toBe('MAIN');
      }
      else if (step.expect === 'shown') expectShown();
      else expectHidden();
    }
    // The first-run tour is never mounted, online or off.
    expect(screen.queryByTestId('tour-provider')).toBeNull();
    expect(screen.queryByRole('dialog', { name: /^Product tour/ })).toBeNull();
  });

  it('announces exactly what the shell manifest says', () => {
    expect(OFFLINE_ANNOUNCEMENTS).toEqual(loadShellHeaderManifest().announcements);
  });

  it.each(fixture.startCommands.map((row) => [row.port || '(none)', row] as const))(
    'offers the start command for port %s',
    (_port, row) => {
      expect(startCommandFor(row.port)).toBe(row.command);
    },
  );
});
