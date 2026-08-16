import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, cleanup } from '@testing-library/react';
import { TopNavbar } from './TopNavbar';

// TopNavbar reads usePathname from next/navigation.
let currentPathname = '/';
vi.mock('next/navigation', () => ({
  usePathname: () => currentPathname,
}));

// The code map section is gated on the server-advertised capability set
// (ServerCapabilitiesContext). Tests drive the ReadonlySet<string> of advertised
// tokens directly — the same shape the real provider exposes — so the mock reads
// as the token set the shell reacts to, not a boolean proxy.
let capabilities: ReadonlySet<string> = new Set();
vi.mock('@/contexts/ServerCapabilitiesContext', () => ({
  useServerCapabilities: () => ({
    status: 'ready',
    capabilities,
  }),
}));

describe('TopNavbar — graph shell nav', () => {
  afterEach(() => {
    cleanup();
    currentPathname = '/';
    capabilities = new Set();
  });

  it('renders exactly the three graph-shell links in analytics-first order on an experimental server', () => {
    capabilities = new Set(['code_map_navigation_v1']);
    render(<TopNavbar connected />);
    const nav = screen.getByRole('navigation', { name: 'Main navigation' });

    // The changes section is labelled "projects" (LABEL_OVERRIDES in
    // lib/nav/sections.ts) — it owns `/`, which is the project picker.
    const projects = screen.getByRole('link', { name: 'projects' });
    const map = screen.getByRole('link', { name: 'code map' });
    const analytics = screen.getByRole('link', { name: 'analytics' });

    expect(projects).toHaveAttribute('href', '/');
    expect(map).toHaveAttribute('href', '/map');
    expect(analytics).toHaveAttribute('href', '/analytics');

    // Order still comes from fairtrade's GRAPH_APP_SECTIONS: analytics first.
    const labels = Array.from(nav.querySelectorAll('a')).map((a) => a.textContent);
    expect(labels).toEqual(['analytics', 'projects', 'code map']);

    // Shared fairtrade chrome uses lowercase labels, not title-cased app labels.
    expect(screen.queryByText('Analytics')).not.toBeInTheDocument();
    expect(screen.queryByText('Code Map')).not.toBeInTheDocument();
    expect(screen.queryByText('Projects')).not.toBeInTheDocument();
    expect(screen.queryByText('changes')).not.toBeInTheDocument();

    // The other retired labels stay gone from persistent chrome.
    expect(screen.queryByText('Overview')).not.toBeInTheDocument();
    expect(screen.queryByText('Review')).not.toBeInTheDocument();
    expect(screen.queryByText('Share')).not.toBeInTheDocument();
    expect(screen.queryByText('Contribute')).not.toBeInTheDocument();
  });

  it('shelves the code map link on a default (non-experimental) server', () => {
    render(<TopNavbar connected />);
    const nav = screen.getByRole('navigation', { name: 'Main navigation' });

    expect(screen.queryByRole('link', { name: 'code map' })).not.toBeInTheDocument();
    const labels = Array.from(nav.querySelectorAll('a')).map((a) => a.textContent);
    expect(labels).toEqual(['analytics', 'projects']);
  });

  it('mounts the persistent share action on the production /share route', () => {
    render(<TopNavbar connected />);

    const share = screen.getByRole('link', { name: 'share' });
    expect(share).toHaveAttribute('href', '/share');
    expect(share.className).toContain('btn-primary');
  });

  it('marks the share action current on the production /share/ pathname', () => {
    currentPathname = '/share/';
    render(<TopNavbar connected />);
    expect(screen.getByRole('link', { name: 'share' })).toHaveAttribute('aria-current', 'page');
  });

  // The active section is a FILLED AMBER PILL — bg-amber + on-amber text + amber border —
  // matching the fairtrade in-use demo's `.iu-subnav-item.active`, not an underline marker.
  function expectActivePill(link: HTMLElement) {
    expect(link.className).toContain('bg-amber');
    expect(link.className).toContain('text-on-amber');
    expect(link.className).toContain('border-amber');
    expect(link).toHaveAttribute('aria-current', 'page');
    // No leftover underline-marker child element.
    expect(link.querySelector('span[aria-hidden]')).toBeNull();
  }

  function expectInactivePill(link: HTMLElement) {
    expect(link.className).not.toContain('bg-amber');
    expect(link.className).not.toContain('text-on-amber');
    expect(link).not.toHaveAttribute('aria-current');
  }

  it('marks Changes active on / with aria-current', () => {
    capabilities = new Set(['code_map_navigation_v1']);
    currentPathname = '/';
    render(<TopNavbar connected />);
    expectActivePill(screen.getByRole('link', { name: 'projects' }));
    expectInactivePill(screen.getByRole('link', { name: 'code map' }));
  });

  it('marks Changes active on /review/* paths', () => {
    capabilities = new Set(['code_map_navigation_v1']);
    currentPathname = '/review/peasant';
    render(<TopNavbar connected />);
    expectActivePill(screen.getByRole('link', { name: 'projects' }));
    expectInactivePill(screen.getByRole('link', { name: 'code map' }));
  });

  it('marks Map active on /map/* paths', () => {
    capabilities = new Set(['code_map_navigation_v1']);
    currentPathname = '/map/peasant';
    render(<TopNavbar connected />);
    expectActivePill(screen.getByRole('link', { name: 'code map' }));
    expectInactivePill(screen.getByRole('link', { name: 'projects' }));
  });

  it('marks Map active on /projects/{name}/{id} viewer routes', () => {
    capabilities = new Set(['code_map_navigation_v1']);
    currentPathname = '/projects/peasant/sess-0001';
    render(<TopNavbar connected />);
    expectActivePill(screen.getByRole('link', { name: 'code map' }));
    expectInactivePill(screen.getByRole('link', { name: 'projects' }));
  });

  it('marks Analytics active on /analytics', () => {
    currentPathname = '/analytics';
    render(<TopNavbar connected />);
    expectActivePill(screen.getByRole('link', { name: 'analytics' }));
  });

  it('keeps the connection badge (plain-language, local-only)', () => {
    render(<TopNavbar connected={false} />);
    expect(screen.getByText('connecting…')).toBeInTheDocument();
    render(<TopNavbar connected />);
    expect(screen.getByText('live · local')).toBeInTheDocument();
  });

  it('shows the word "Search" beside the ⌘K shortcut on the palette button', () => {
    render(<TopNavbar connected />);
    const button = screen.getByRole('button', {
      name: 'Open the command palette (Command or Control + K)',
    });
    // Reads as a search affordance, not a bare icon: word plus shortcut.
    expect(button).toHaveTextContent('search');
    expect(button.querySelector('kbd')?.textContent).toBe('⌘K');
  });

  it('de-emphasizes the connected badge: no box border in the steady state', () => {
    render(<TopNavbar connected />);
    const badge = screen.getByText('live · local');
    // Connected = quiet glance: no border / box, just text-ink-3.
    expect(badge.className).not.toMatch(/\bborder\b/);
    expect(badge.className).toContain('text-ink-3');
  });

  it('keeps the disconnected badge bordered and colored for attention', () => {
    render(<TopNavbar connected={false} />);
    const badge = screen.getByText('connecting…');
    expect(badge.className).toMatch(/\bborder\b/);
    expect(badge.className).toContain('text-danger');
  });

  it('orders the connection badge before the Search control in the right cluster', () => {
    render(<TopNavbar connected />);
    const badge = screen.getByText('live · local');
    const search = screen.getByRole('button', {
      name: 'Open the command palette (Command or Control + K)',
    });
    // The de-emphasized status leads; the interactive Search control anchors
    // the right edge — DOCUMENT_POSITION_FOLLOWING means search comes after.
    expect(badge.compareDocumentPosition(search) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });
});
