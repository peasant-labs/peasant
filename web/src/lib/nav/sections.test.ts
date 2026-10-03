import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
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
import {
  parseStrictYAML,
  requireExactRequiredFields,
  requireRecord,
  requireUniqueNames,
} from '@/test/strictYaml';

const ACTIVE_CASES = (() => {
  const path = resolve(process.cwd(), 'src/lib/nav/testdata/section_active.yaml');
  const root = requireRecord(parseStrictYAML(readFileSync(path, 'utf8'), 'section active fixture'), 'section active fixture');
  requireExactRequiredFields(root, ['cases'], 'section active fixture');
  if (!Array.isArray(root.cases)) throw new Error('section active fixture.cases must be a list');
  const rows = root.cases.map((row, index) => requireRecord(row, `section active fixture.cases[${index}]`));
  rows.forEach((row, index) => requireExactRequiredFields(row, ['name', 'section', 'pathname', 'active'], `section active fixture.cases[${index}]`));
  requireUniqueNames(rows, 'section active fixture.cases');
  const required = ['home-on-root', 'home-not-on-a-section-page', 'section-on-its-page-with-trailing-slash', 'section-below-its-page', 'section-not-on-a-lookalike-path'];
  const missing = required.filter((name) => !rows.some((row) => row.name === name));
  if (missing.length) throw new Error(`section active fixture is missing required cases: ${missing.join(', ')}`);
  return rows as Array<{ name: string; section: string; pathname: string; active: boolean }>;
})();

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

  it.each(ACTIVE_CASES.map((row) => [row.name, row] as const))('match the active section: %s', (_name, row) => {
    const section = LOCAL_SECTIONS.find((candidate) => candidate.id === row.section);
    if (!section) throw new Error(`section active fixture names unrouted section ${row.section}`);
    expect(isSectionActive(section, row.pathname)).toBe(row.active);
  });
});
