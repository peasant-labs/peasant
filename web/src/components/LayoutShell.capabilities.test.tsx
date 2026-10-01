import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, cleanup, act, within } from '@testing-library/react';
import { capabilitiesResponse, UI_CAPABILITY_CASES, type CapabilityCase } from '@/test/fixtures/uiCapabilities';
import {
  headerFailures,
  loadShellHeaderManifest,
  paletteFailures,
  shippedItems,
} from '../../scripts/visual/shell-header-manifest.mjs';
import { usePublishState } from '@/contexts/PublishContext';
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

const manifest = loadShellHeaderManifest();
const shipped = shippedItems(manifest);

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

/** Wire the HTTP mock for one capability case. Everything else the shell may
 *  ask is answered as a running app would; any other request fails the test. */
function mockHttp(row: CapabilityCase): void {
  fetchMock.mockImplementation(async (input: string | URL) => {
    const url = new URL(String(input), 'http://localhost');
    if (url.pathname === '/api/v1/config/capabilities') return capabilitiesResponse(row);
    if (url.pathname === '/api/v1/health') return Response.json({ status: 'ok' });
    throw new Error(`unexpected test request ${url.pathname}`);
  });
}

describe('LayoutShell — the code map stays route-only whatever the server advertises', () => {
  it.each(UI_CAPABILITY_CASES.map((row) => [row.name, row] as const))(
    'keeps the code map out of the header and the palette: %s',
    async (_name, row) => {
      mockHttp(row);
      render(
        <LayoutShell>
          <main>body</main>
        </LayoutShell>,
      );
      // Let the capability request settle (or stay pending) before reading.
      await act(async () => {
        await new Promise((r) => setTimeout(r, 0));
      });

      expect(headerFailures(manifest, { theme: 'light', shipped })).toEqual([]);
      expect(screen.queryByRole('link', { name: 'code map' })).not.toBeInTheDocument();

      await act(async () => {
        window.dispatchEvent(new Event(OPEN_COMMAND_PALETTE_EVENT));
      });
      const dialog = await screen.findByRole('dialog', { name: 'Command palette' });
      expect(paletteFailures(manifest)).toEqual([]);
      expect(within(dialog).getByText('go to home')).toBeInTheDocument();
      expect(within(dialog).queryByText('go to code map')).not.toBeInTheDocument();
    },
  );
});

function CacheWriter() {
  const store = usePublishState();
  return <button onClick={() => store.updateRedactionCache((cache) => new Map(cache).set('standard:session', { status: 'failure', error: 'scan failed' }))}>save scan failure</button>;
}
function CacheReader() {
  const store = usePublishState();
  const entry = store.redactionCache.get('standard:session');
  return <p>{entry?.status === 'failure' ? entry.error : 'no cached scan'}</p>;
}
it('shares the publish cache between consumers through the production shell', async () => {
  mockHttp(UI_CAPABILITY_CASES[0]);
  const view = render(<LayoutShell><CacheWriter /><CacheReader /></LayoutShell>);
  await act(async () => { screen.getByRole('button', { name: 'save scan failure' }).click(); });
  expect(screen.getByText('scan failed')).toBeInTheDocument();
  view.rerender(<LayoutShell><CacheReader /></LayoutShell>);
  expect(screen.getByText('scan failed')).toBeInTheDocument();
});
