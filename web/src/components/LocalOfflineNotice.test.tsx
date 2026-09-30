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
import { startCommandFor } from './LocalOfflineNotice';

vi.mock('next/navigation', () => ({ usePathname: () => '/', useRouter: () => ({ push: vi.fn() }) }));
vi.mock('@/hooks/useTheme', () => ({ useTheme: () => ({ theme: 'dark', toggle: vi.fn() }) }));
// The tour stays in the tree but is not mounted: a mount of its provider would render this marker.
vi.mock('@/components/tour/TourProvider', () => ({
  TourProvider: ({ children }: { children: ReactNode }) => <div data-testid="tour-provider">{children}</div>,
}));

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

type Step =
  | { health: 'up' | 'down' }
  | { socket: 'open' | 'close' }
  | { wait: number }
  | { retry: true }
  | { expect: 'shown' | 'hidden' }
  | { sockets: number };

const STEP_KEYS = ['health', 'socket', 'wait', 'retry', 'expect', 'sockets'] as const;
const REQUIRED_CASES = [
  'reachable-app-shows-nothing',
  'first-connect-within-grace-shows-nothing',
  'stopped-app-shows-at-once',
  'socket-blip-reconnects-without-notice',
  'socket-down-past-grace-shows',
  'notice-clears-when-app-returns',
  'try-again-reconnects-at-once',
];

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

let healthUp = true;
let originalWebSocket: typeof globalThis.WebSocket;

beforeEach(() => {
  vi.useFakeTimers();
  MockWebSocket.instances = [];
  healthUp = true;
  originalWebSocket = globalThis.WebSocket;
  // @ts-expect-error — a scripted stand-in for the browser WebSocket.
  globalThis.WebSocket = MockWebSocket;
  vi.stubGlobal('fetch', vi.fn(async (input: string | URL) => {
    const url = new URL(String(input), 'http://localhost');
    if (url.pathname === '/api/v1/health') {
      if (!healthUp) throw new TypeError('Failed to fetch');
      return Response.json({ status: 'ok' });
    }
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

function expectShown() {
  const region = notice();
  expect(region, 'the offline notice must show').not.toBeNull();
  // It sits in the fixed top chrome, directly under the header.
  const chrome = document.querySelector('header')?.parentElement;
  expect(chrome?.contains(region)).toBe(true);
  expect(chrome?.firstElementChild?.tagName).toBe('HEADER');
  // It names this computer, never the internet, and offers the way back.
  expect(within(region!).getByRole('status')).toHaveTextContent(
    "peasant isn't running on this computer. your internet is fine: this page talks to the peasant app on your machine.",
  );
  // The start command is for the port this page came from.
  expect(within(region!).getByText(startCommandFor(window.location.port))).toBeInTheDocument();
  expect(within(region!).getByRole('button', { name: 'try again' })).toBeInTheDocument();
  // The fixed chrome grows by the notice while it shows.
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
    render(
      <LayoutShell>
        <main>body</main>
      </LayoutShell>,
    );
    await advance(0);
    for (const step of steps) {
      if ('health' in step) healthUp = step.health === 'up';
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
      else if ('sockets' in step) expect(MockWebSocket.instances).toHaveLength(step.sockets);
      else if (step.expect === 'shown') expectShown();
      else expectHidden();
    }
    // The first-run tour is never mounted, online or off.
    expect(screen.queryByTestId('tour-provider')).toBeNull();
    expect(screen.queryByRole('dialog', { name: /^Product tour/ })).toBeNull();
  });

  it.each(fixture.startCommands.map((row) => [row.port || '(none)', row] as const))(
    'offers the start command for port %s',
    (_port, row) => {
      expect(startCommandFor(row.port)).toBe(row.command);
    },
  );
});
