import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, cleanup, fireEvent } from '@testing-library/react';
import {
  headerFailures,
  loadShellHeaderManifest,
  routePageFile,
  shippedItems,
} from '../../scripts/visual/shell-header-manifest.mjs';
import { ROUTE_ONLY_SECTIONS } from '@/lib/nav/sections';
import { OPEN_COMMAND_PALETTE_EVENT } from '@/components/command/CommandPalette';
import { TopNavbar } from './TopNavbar';

let currentPathname = '/';
vi.mock('next/navigation', () => ({
  usePathname: () => currentPathname,
}));

let theme: 'light' | 'dark' = 'light';
const toggle = vi.fn();
vi.mock('@/hooks/useTheme', () => ({ useTheme: () => ({ theme, toggle }) }));

// The header reads the advertised capability set. The code-map token is
// advertised here on purpose: an experimental server must not bring a
// route-only section back into the header.
vi.mock('@/contexts/ServerCapabilitiesContext', () => ({
  useServerCapabilities: () => ({ status: 'ready', capabilities: new Set(['code_map_navigation_v1']) }),
}));

const manifest = loadShellHeaderManifest();
const shipped = shippedItems(manifest);

afterEach(() => {
  cleanup();
  currentPathname = '/';
  theme = 'light';
  toggle.mockClear();
});

describe('TopNavbar — the local shell header manifest', () => {
  it.each(['light', 'dark'] as const)('holds every show, hide and route rule in the %s theme', (mode) => {
    theme = mode;
    render(<TopNavbar />);
    expect(headerFailures(manifest, { theme: mode, shipped })).toEqual([]);
  });

  it.each(['/', '/analytics', '/review/peasant', '/map/peasant', '/projects/peasant/sess-0001', '/share/'])(
    'holds on %s, a route-only page included',
    (pathname) => {
      currentPathname = pathname;
      render(<TopNavbar />);
      expect(headerFailures(manifest, { theme: 'light', shipped })).toEqual([]);
    },
  );

  it('leaves settings out until the settings page exists, so the link is never dead', () => {
    // Written against the current tree: flips to "must show" the moment the page file lands.
    render(<TopNavbar />);
    const link = document.querySelector(manifest.show.settings.selector);
    expect(link !== null).toBe(shipped.settings);
  });

  it('switches the theme from its icon-only button', () => {
    render(<TopNavbar />);
    const button = screen.getByRole('button', { name: 'Switch to dark mode' });
    expect(button).toHaveAttribute('title', 'Switch to dark mode');
    expect(button.textContent).toBe('');
    fireEvent.click(button);
    expect(toggle).toHaveBeenCalledTimes(1);
  });

  it('opens the command palette from the search button', () => {
    const opened = vi.fn();
    window.addEventListener(OPEN_COMMAND_PALETTE_EVENT, opened);
    render(<TopNavbar />);
    fireEvent.click(screen.getByRole('button', { name: 'Open the command palette (Command or Control + K)' }));
    window.removeEventListener(OPEN_COMMAND_PALETTE_EVENT, opened);
    expect(opened).toHaveBeenCalledTimes(1);
  });
});

describe('route-only sections', () => {
  it('are exactly the manifest routes, and each still has its page', () => {
    expect(ROUTE_ONLY_SECTIONS.map((section) => section.href).sort()).toEqual(Object.keys(manifest.routes).sort());
    for (const path of Object.keys(manifest.routes)) {
      expect(routePageFile(path), `${path} must keep its page`).not.toBeNull();
    }
  });
});

describe('shell header manifest loader', () => {
  it('rejects a manifest that drops a required name', async () => {
    const { readFileSync } = await import('node:fs');
    const { SHELL_HEADER_FIXTURE } = await import('../../scripts/visual/shell-header-manifest.mjs');
    const source = readFileSync(SHELL_HEADER_FIXTURE, 'utf8');
    const withoutShare = source.replace(/  share-button:\n    selector: [^\n]+\n/, '');
    expect(withoutShare).not.toBe(source);
    expect(() => loadShellHeaderManifest(withoutShare)).toThrow(/hide is missing required names: share-button/);
  });
});
