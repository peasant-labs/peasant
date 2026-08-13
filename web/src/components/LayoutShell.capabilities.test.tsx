import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, cleanup, act, waitFor, within } from '@testing-library/react';
import {
  parseStrictYAML,
  requireExactRequiredFields,
  requireRecord,
  requireUniqueNames,
} from '@/test/strictYaml';
import { LayoutShell } from './LayoutShell';
import { OPEN_COMMAND_PALETTE_EVENT } from './command/CommandPalette';

// ---------------------------------------------------------------------------
// Only HTTP is mocked. The ServerCapabilitiesProvider under test is REAL; the
// real TopNavbar and CommandPalette read it. next/navigation and useTheme are
// framework/browser shims (not HTTP), mocked as in the sibling suites.
// ---------------------------------------------------------------------------

const push = vi.fn();
vi.mock('next/navigation', () => ({
  usePathname: () => '/',
  useRouter: () => ({ push }),
}));
vi.mock('@/hooks/useTheme', () => ({ useTheme: () => ({ theme: 'light', toggle: vi.fn() }) }));

// A valid 64-hex project hash so the palette surfaces per-project jumps.
const PROJECT_HASH = 'a'.repeat(64);
const PROJECT = {
  projectHash: PROJECT_HASH,
  project: '/work/demo-project',
  sessions: 1,
  recordedFiles: 1,
  totalFiles: 1,
  openChanges: 0,
};

// ---------------------------------------------------------------------------
// MockWebSocket — LayoutShell mounts the real WebSocketProvider, which opens a
// socket on mount. jsdom has no WebSocket; this inert stub never fires events,
// so the connection stays quietly "connecting" and drives no async state.
// ---------------------------------------------------------------------------
class MockWebSocket {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 3;
  onopen: ((ev: Event) => void) | null = null;
  onmessage: ((ev: MessageEvent) => void) | null = null;
  onclose: (() => void) | null = null;
  onerror: (() => void) | null = null;
  readyState = MockWebSocket.CONNECTING;
  send(): void {}
  close(): void {
    this.readyState = MockWebSocket.CLOSED;
  }
}

// ---------------------------------------------------------------------------
// Fixture: capability HTTP scenarios × expected code-map discoverability.
// ---------------------------------------------------------------------------
type CapabilityCase = {
  name: string;
  expectVisible: boolean;
  pending?: boolean;
  throw?: boolean;
  status?: number;
  body?: unknown;
};

const fixturePath = resolve(process.cwd(), 'src/components/testdata/ui_capabilities.yaml');
const fixture = requireRecord(
  parseStrictYAML(readFileSync(fixturePath, 'utf8'), 'ui capabilities fixture'),
  'ui capabilities fixture',
);
requireExactRequiredFields(fixture, ['cases'], 'ui capabilities fixture');
const cases = fixture.cases as CapabilityCase[];
if (!Array.isArray(cases) || cases.length !== 10) {
  throw new Error(`ui capabilities fixture must contain exactly 10 cases, got ${Array.isArray(cases) ? cases.length : 'non-array'}`);
}
requireUniqueNames(cases as unknown as Record<string, unknown>[], 'ui capabilities fixture.cases');
if (cases.filter((c) => c.expectVisible).length !== 1) {
  throw new Error('ui capabilities fixture must contain exactly one code-map-visible case');
}

let fetchMock: ReturnType<typeof vi.fn>;
let originalWebSocket: typeof globalThis.WebSocket;

beforeEach(() => {
  push.mockClear();
  originalWebSocket = globalThis.WebSocket;
  // @ts-expect-error — inert stand-in for the browser WebSocket in jsdom.
  globalThis.WebSocket = MockWebSocket;
  fetchMock = vi.fn();
  vi.stubGlobal('fetch', fetchMock);
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  globalThis.WebSocket = originalWebSocket;
  vi.resetAllMocks();
});

/** Wire the HTTP mock for one capability case (capabilities endpoint + the
 *  palette's own data endpoints). Everything else is an explicit test failure. */
function mockHttp(row: CapabilityCase): void {
  fetchMock.mockImplementation(async (input: string | URL) => {
    const url = new URL(String(input), 'http://localhost');
    if (url.pathname === '/api/v1/config/capabilities') {
      if (row.throw) throw new Error('network down');
      if (row.pending) return new Promise<Response>(() => {}); // never resolves
      const status = row.status ?? 200;
      if (row.body === undefined) return new Response(null, { status });
      return new Response(JSON.stringify(row.body), {
        status,
        headers: { 'Content-Type': 'application/json' },
      });
    }
    if (url.pathname === '/api/v1/projects/summary') return Response.json({ projects: [PROJECT] });
    if (url.pathname === '/api/v1/search') return Response.json({ query: url.searchParams.get('q'), results: [] });
    if (url.pathname === '/api/v1/web/discovery') return Response.json({ items: [] });
    throw new Error(`unexpected test request ${url.pathname}`);
  });
}

async function openPalette(): Promise<void> {
  await act(async () => {
    window.dispatchEvent(new Event(OPEN_COMMAND_PALETTE_EVENT));
  });
}

function navLabels(): string[] {
  const nav = screen.getByRole('navigation', { name: 'Main navigation' });
  return Array.from(nav.querySelectorAll('a')).map((a) => a.textContent);
}

describe('LayoutShell — capability-gated code-map discoverability', () => {
  it.each(cases.map((row) => [row.name, row] as const))(
    'keeps the code map %s',
    async (_name, row) => {
      mockHttp(row);
      render(
        <LayoutShell>
          <main>body</main>
        </LayoutShell>,
      );

      // Open the palette and wait until its own data has loaded, so the
      // capability fetch has had the same settling window: a fail-closed result
      // must hide the map even with the palette fully populated.
      await openPalette();
      const dialog = await screen.findByRole('dialog', { name: 'Command palette' });
      await within(dialog).findByText('demo-project · changes');

      if (row.expectVisible) {
        // Nav shows the canonical three sections in analytics-first order…
        await screen.findByRole('link', { name: 'code map' });
        expect(navLabels()).toEqual(['analytics', 'changes', 'code map']);
        // …and, in the SAME render, the palette exposes both the section jump
        // and the per-project map jump (simultaneity).
        expect(within(dialog).getByText('go to code map')).toBeInTheDocument();
        expect(within(dialog).getByText('demo-project · map')).toBeInTheDocument();
      } else {
        // Fail closed: no map tab, and no map commands in the palette.
        expect(navLabels()).toEqual(['analytics', 'changes']);
        expect(screen.queryByRole('link', { name: 'code map' })).not.toBeInTheDocument();
        expect(within(dialog).queryByText('go to code map')).not.toBeInTheDocument();
        expect(within(dialog).queryByText('demo-project · map')).not.toBeInTheDocument();
        // The palette still works — the changes jump is present.
        expect(within(dialog).getByText('go to changes')).toBeInTheDocument();
      }
    },
  );
});
