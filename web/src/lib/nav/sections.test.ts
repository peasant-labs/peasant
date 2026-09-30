import { afterEach, describe, expect, it, vi } from 'vitest';
import { LOCAL_APP_SECTIONS } from '@peasant-labs/fairtrade/graph';
import {
  headerNavSections,
  isSectionActive,
  LOCAL_SECTIONS,
  NAV_SECTIONS,
  ROUTE_ONLY_SECTIONS,
  visibleNavSections,
} from './sections';

afterEach(() => {
  vi.doUnmock('@peasant-labs/fairtrade/graph');
  vi.resetModules();
});

describe('local app sections', () => {
  it('follow the fairtrade registry: order, labels and nav membership', () => {
    const registry = LOCAL_APP_SECTIONS.map(({ id, label, inNav }) => ({ id, label, inNav: inNav !== false }));
    // Settings is the one registry section this app has no page for yet.
    expect(LOCAL_SECTIONS.map(({ id, label, inNav }) => ({ id, label, inNav }))).toEqual(
      registry.filter((section) => section.id !== 'settings'),
    );
  });

  it('link the header only to home until the settings page ships', () => {
    expect(NAV_SECTIONS.map((section) => [section.id, section.href])).toEqual([['home', '/']]);
    expect(visibleNavSections(new Set(['code_map_navigation_v1'])).map((section) => section.id)).toEqual(['home']);
    // Home is the brand link, so the header carries no section link beside it yet.
    expect(headerNavSections(new Set(['code_map_navigation_v1']))).toEqual([]);
  });

  it('keep analytics, changes and the code map on their routes, by URL only', () => {
    expect(ROUTE_ONLY_SECTIONS.map((section) => [section.id, section.label, section.href])).toEqual([
      ['analytics', 'analytics', '/analytics'],
      ['changes', 'changes', '/review'],
      ['map', 'code map', '/map'],
    ]);
  });

  it('fail loudly on a registry id this app does not map', async () => {
    vi.doMock('@peasant-labs/fairtrade/graph', () => ({
      LOCAL_APP_SECTIONS: [...LOCAL_APP_SECTIONS, { id: 'insights', label: 'insights', inNav: true }],
    }));
    vi.resetModules();
    await expect(import('./sections')).rejects.toThrow(/Unknown local app section "insights"/);
  });

  it('match a section on its own page and below it, and home on / only', () => {
    const [home] = NAV_SECTIONS;
    const [analytics] = ROUTE_ONLY_SECTIONS;
    expect(isSectionActive(home, '/')).toBe(true);
    expect(isSectionActive(home, '/review/')).toBe(false);
    expect(isSectionActive(analytics, '/analytics/')).toBe(true);
    expect(isSectionActive(analytics, '/analytics/x')).toBe(true);
    expect(isSectionActive(analytics, '/analyticsx')).toBe(false);
  });
});
