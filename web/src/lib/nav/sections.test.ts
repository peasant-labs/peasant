import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { LOCAL_APP_SECTIONS } from '@peasant-labs/fairtrade/graph';
import {
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
  const required = ['home-on-root', 'home-not-on-a-section-page', 'section-on-its-page-with-trailing-slash', 'section-below-its-page', 'section-not-on-a-lookalike-path', 'village-direct-active', 'village-not-lookalike'];
  const missing = required.filter((name) => !rows.some((row) => row.name === name));
  if (missing.length) throw new Error(`section active fixture is missing required cases: ${missing.join(', ')}`);
  return rows as Array<{ name: string; section: string; pathname: string; active: boolean }>;
})();

const NAV_EXPECTATIONS = (() => {
  const path = resolve(process.cwd(), 'src/lib/nav/testdata/village_adoption.yaml');
  const root = requireRecord(parseStrictYAML(readFileSync(path, 'utf8'), 'village adoption fixture'), 'village adoption fixture');
  requireExactRequiredFields(root, ['navRoutes', 'routeOnly'], 'village adoption fixture');
  const tuples = (rows: unknown, size: number, field: string) => {
    if (!Array.isArray(rows) || rows.some(row => !Array.isArray(row) || row.length !== size || row.some(cell => typeof cell !== 'string'))) throw new Error(`village adoption fixture.${field} requires named routes`);
    return rows as string[][];
  };
  const navRoutes = tuples(root.navRoutes, 2, 'navRoutes') as [string, string][];
  const routeOnly = tuples(root.routeOnly, 3, 'routeOnly') as [string, string, string][];
  if (!navRoutes.some(([id, href]) => id === 'home' && href === '/')) throw new Error('required home route missing from adoption fixture');
  if (!routeOnly.some(([id, label, href]) => id === 'village' && label === 'online' && href === '/village')) throw new Error('required route-only village page missing from adoption fixture');
  return { navRoutes, routeOnly };
})();

afterEach(() => {
  vi.doUnmock('@peasant-labs/fairtrade/graph');
  vi.resetModules();
});

describe('local app sections', () => {
  it('follow the fairtrade registry: order, labels and nav membership', () => {
    const registry = LOCAL_APP_SECTIONS.map(({ id, label, inNav }) => ({ id, label, inNav: inNav !== false }));
    expect(LOCAL_SECTIONS.map(({ id, label, inNav }) => ({ id, label, inNav }))).toEqual(
      registry,
    );
  });

  it('links every canonical navigation section to its mounted page', () => {
    expect(NAV_SECTIONS.map((section) => [section.id, section.href])).toEqual(NAV_EXPECTATIONS.navRoutes);
    expect(visibleNavSections(new Set(['code_map_navigation_v1'])).map((section) => section.id)).toEqual(NAV_EXPECTATIONS.navRoutes.map(([id]) => id));
  });

  it('keep online, analytics, changes and the code map on their routes, by URL only', () => {
    expect(ROUTE_ONLY_SECTIONS.map((section) => [section.id, section.label, section.href])).toEqual(NAV_EXPECTATIONS.routeOnly);
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
