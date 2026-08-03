import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import {
  CODE_MAP_VIEWPORT_SCALE,
  isCodeMapViewportScale,
  type CodeMapState,
} from '@peasant-labs/fairtrade/graph';
import {
  parseStrictYAML,
  requireExactRequiredFields,
  requireRecord,
  requireUniqueNames,
} from '@/test/strictYaml';
import { formatCodeMapLocation, parseCodeMapLocation } from './codeMapRouteState';

const fixtureSource = readFileSync(
  resolve(process.cwd(), 'src/app/map/lib/testdata/code_map_route_state.yaml'),
  'utf8',
);
const manifestSource = readFileSync(
  resolve(process.cwd(), 'src/app/map/lib/testdata/code_map_route_state_manifest.yaml'),
  'utf8',
);

type RouteCase = {
  name: string;
  pathname: string;
  search: string;
  accepted: boolean;
  expectedHref: string;
  state: CodeMapState | null;
};

type RouteManifest = {
  expectedCaseCount: number;
  viewportEndpointCases: { minimum: string; maximum: string };
  requiredCases: Array<{ name: string; accepted: boolean }>;
  expectedMutationCount: number;
  mutations: Array<{ name: string; search: string; replacement: string; diagnostic: string }>;
};

const CASE_FIELDS = [
  'name',
  'pathname',
  'search',
  'accepted',
  'expectedHref',
  'state',
] as const;
const STATE_FIELDS = [
  'version',
  'presentation',
  'selectedId',
  'grain',
  'expandedIds',
  'navigatorFilter',
  'navigatorFocusedId',
  'viewport',
  'hoveredSessionId',
  'selectedSessionId',
  'expandedCommitSessions',
  'expandedGhostGroups',
  'rankMode',
  'scentFilter',
] as const;

function loadManifest(source: string): RouteManifest {
  const root = requireRecord(
    parseStrictYAML(source, 'code-map route manifest'),
    'code-map route manifest',
  );
  requireExactRequiredFields(
    root,
    [
      'expectedCaseCount',
      'viewportEndpointCases',
      'requiredCases',
      'expectedMutationCount',
      'mutations',
    ],
    'code-map route manifest',
  );
  if (!Array.isArray(root.requiredCases) || !Array.isArray(root.mutations)) {
    throw new Error('code-map route manifest cases and mutations must be arrays');
  }
  const requiredCases = root.requiredCases.map((value, index) => {
    const row = requireRecord(value, `code-map route manifest.requiredCases[${index}]`);
    requireExactRequiredFields(row, ['name', 'accepted'], `code-map route manifest.requiredCases[${index}]`);
    if (typeof row.name !== 'string' || row.name.length === 0 || typeof row.accepted !== 'boolean') {
      throw new Error('code-map route manifest cases require a non-empty name and boolean accepted');
    }
    return row as { name: string; accepted: boolean };
  });
  const viewportEndpointCases = requireRecord(
    root.viewportEndpointCases,
    'code-map route manifest.viewportEndpointCases',
  );
  requireExactRequiredFields(
    viewportEndpointCases,
    ['minimum', 'maximum'],
    'code-map route manifest.viewportEndpointCases',
  );
  if (typeof viewportEndpointCases.minimum !== 'string'
    || viewportEndpointCases.minimum.length === 0
    || typeof viewportEndpointCases.maximum !== 'string'
    || viewportEndpointCases.maximum.length === 0) {
    throw new Error('code-map route manifest viewport endpoint case names must be non-empty strings');
  }
  const mutations = root.mutations.map((value, index) => {
    const row = requireRecord(value, `code-map route manifest.mutations[${index}]`);
    requireExactRequiredFields(
      row,
      ['name', 'search', 'replacement', 'diagnostic'],
      `code-map route manifest.mutations[${index}]`,
    );
    if (Object.values(row).some((value) => typeof value !== 'string' || value.length === 0)) {
      throw new Error('code-map route manifest mutations require non-empty strings');
    }
    return row as RouteManifest['mutations'][number];
  });
  requireUniqueNames(requiredCases, 'code-map route manifest.requiredCases');
  requireUniqueNames(mutations, 'code-map route manifest.mutations');
  if (root.expectedCaseCount !== requiredCases.length) {
    throw new Error(`code-map route manifest requires exactly ${requiredCases.length} cases`);
  }
  if (root.expectedMutationCount !== mutations.length) {
    throw new Error(`code-map route manifest requires exactly ${mutations.length} mutations`);
  }
  return {
    expectedCaseCount: root.expectedCaseCount as number,
    viewportEndpointCases: viewportEndpointCases as RouteManifest['viewportEndpointCases'],
    requiredCases,
    expectedMutationCount: root.expectedMutationCount as number,
    mutations,
  };
}

function loadFixture(source: string, manifest: RouteManifest): RouteCase[] {
  const root = requireRecord(
    parseStrictYAML(source, 'code-map route fixture'),
    'code-map route fixture',
  );
  requireExactRequiredFields(root, ['expectedCaseCount', 'cases'], 'code-map route fixture');
  if (!Array.isArray(root.cases)) throw new Error('code-map route fixture.cases must be an array');
  const rows = root.cases.map((value, index) => {
    const row = requireRecord(value, `code-map route fixture.cases[${index}]`);
    requireExactRequiredFields(row, CASE_FIELDS, `code-map route fixture.cases[${index}]`);
    if (typeof row.name !== 'string' || typeof row.pathname !== 'string'
      || typeof row.search !== 'string' || typeof row.accepted !== 'boolean'
      || typeof row.expectedHref !== 'string') {
      throw new Error(`code-map route fixture.cases[${index}] has invalid scalar fields`);
    }
    if (row.accepted) {
      const state = requireRecord(row.state, `code-map route fixture.cases[${index}].state`);
      requireExactRequiredFields(state, STATE_FIELDS, `code-map route fixture.cases[${index}].state`);
      if (state.viewport !== null) {
        requireExactRequiredFields(
          requireRecord(state.viewport, `code-map route fixture.cases[${index}].state.viewport`),
          ['scale', 'panX', 'panY'],
          `code-map route fixture.cases[${index}].state.viewport`,
        );
      }
    } else if (row.state !== null) {
      throw new Error(`code-map route fixture.cases[${index}].state must be null when rejected`);
    }
    return row as unknown as RouteCase;
  });
  requireUniqueNames(rows, 'code-map route fixture.cases');
  if (root.expectedCaseCount !== manifest.expectedCaseCount || rows.length !== manifest.expectedCaseCount) {
    throw new Error(`code-map route fixture requires exactly ${manifest.expectedCaseCount} cases`);
  }
  for (const required of manifest.requiredCases) {
    const row = rows.find((candidate) => candidate.name === required.name);
    if (!row) throw new Error(`code-map route fixture is missing required case ${required.name}`);
    if (row.accepted !== required.accepted) {
      throw new Error(`code-map route fixture case ${required.name} has the wrong acceptance outcome`);
    }
  }
  return rows;
}

const manifest = loadManifest(manifestSource);
const routeCases = loadFixture(fixtureSource, manifest);

describe('code-map route fixture contract', () => {
  it.each(manifest.mutations)('rejects $name', (mutation) => {
    expect(fixtureSource.split(mutation.search)).toHaveLength(2);
    expect(() => loadFixture(
      fixtureSource.replace(mutation.search, mutation.replacement),
      manifest,
    )).toThrow(mutation.diagnostic);
  });

  it('rejects manifest drift and extra YAML documents', () => {
    expect(() => loadManifest(manifestSource.replace('expectedMutationCount: 5', 'expectedMutationCount: 4')))
      .toThrow(/exactly 5 mutations/);
    expect(() => loadManifest(`${manifestSource}\n---\n{}`)).toThrow(/one YAML document/);
  });

  it('binds endpoint cases to Fairtrade viewport policy', () => {
    const minimum = routeCases.find((row) => row.name === manifest.viewportEndpointCases.minimum);
    const maximum = routeCases.find((row) => row.name === manifest.viewportEndpointCases.maximum);
    expect(minimum?.state?.viewport?.scale).toBe(CODE_MAP_VIEWPORT_SCALE.min);
    expect(maximum?.state?.viewport?.scale).toBe(CODE_MAP_VIEWPORT_SCALE.max);
    expect(isCodeMapViewportScale(minimum?.state?.viewport?.scale)).toBe(true);
    expect(isCodeMapViewportScale(maximum?.state?.viewport?.scale)).toBe(true);
  });
});

describe.each(routeCases)('$name', (fixture) => {
  it('maps only valid Peasant URLs to the canonical Fairtrade state', () => {
    const parsed = parseCodeMapLocation(fixture.pathname, fixture.search);
    if (!fixture.accepted) {
      expect(parsed).toBeNull();
      return;
    }
    expect(parsed?.state).toEqual(fixture.state);
    if (parsed?.state.viewport) {
      expect(isCodeMapViewportScale(parsed.state.viewport.scale)).toBe(true);
    }
    expect(parsed && formatCodeMapLocation(parsed.projectHash, parsed.state)).toBe(fixture.expectedHref);
  });
});
