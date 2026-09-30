import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, cleanup } from '@testing-library/react';
import { headerFailures, loadShellHeaderManifest, shippedItems } from '../../scripts/visual/shell-header-manifest.mjs';
import { TopNavbar } from './TopNavbar';

// A registry in which the settings page has shipped: the header must then carry
// its link, derived from the section list rather than written into the header.
vi.mock('@/lib/nav/sections', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/lib/nav/sections')>();
  return {
    ...actual,
    visibleNavSections: () => [
      { id: 'home', href: '/', label: 'home' },
      { id: 'settings', href: '/settings', label: 'settings', title: 'Every peasant setting.' },
    ],
  };
});

let currentPathname = '/';
vi.mock('next/navigation', () => ({ usePathname: () => currentPathname }));
vi.mock('@/hooks/useTheme', () => ({ useTheme: () => ({ theme: 'dark', toggle: vi.fn() }) }));
vi.mock('@/contexts/ServerCapabilitiesContext', () => ({
  useServerCapabilities: () => ({ status: 'ready', capabilities: new Set<string>() }),
}));

const manifest = loadShellHeaderManifest();

afterEach(() => {
  cleanup();
  currentPathname = '/';
});

describe('TopNavbar — nav sections from the registry', () => {
  it('links every nav section but home, which the brand already is', () => {
    render(<TopNavbar />);
    const settings = screen.getByRole('link', { name: 'settings' });
    expect(settings).toHaveAttribute('href', '/settings');
    expect(settings).toHaveAttribute('title', 'Every peasant setting.');
    expect(settings).not.toHaveAttribute('aria-current');
    expect(screen.queryByRole('link', { name: 'home' })).not.toBeInTheDocument();
    // With the page shipped, the manifest now requires the link.
    expect(headerFailures(manifest, { theme: 'dark', shipped: { ...shippedItems(manifest), settings: true } })).toEqual([]);
  });

  it('marks the section link current on its own page', () => {
    currentPathname = '/settings/';
    render(<TopNavbar />);
    expect(screen.getByRole('link', { name: 'settings' })).toHaveAttribute('aria-current', 'page');
  });
});
