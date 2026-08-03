import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import { parseStrictYAML, requireExactRequiredFields, requireRecord, requireUniqueNames } from '@/test/strictYaml';
import {
  RouteOrigin,
  TranscriptScope,
  formatMapRouteState,
  mapHref,
  parseMapRoute,
  parseMapRouteState,
  parseProjectDetailRoute,
  parseProjectHash,
  parseReviewRoute,
  parseTranscriptRoute,
  parseTranscriptRouteQuery,
  parseReturnLocation,
  reviewHref,
  transcriptHref,
} from './projectRoutes';

const source = readFileSync(resolve(process.cwd(), 'src/lib/navigation/testdata/project_routes.yaml'), 'utf8');
const fields = ['name', 'operation', 'pathname', 'search', 'projectHash', 'sessionId', 'branch', 'rawReturn', 'expected'];

type FixtureCase = Record<(typeof fields)[number], string>;

function loadFixture(yaml: string): FixtureCase[] {
  const root = requireRecord(parseStrictYAML(yaml, 'project route fixture'), 'project route fixture');
  requireExactRequiredFields(root, ['expectedCaseCount', 'requiredNames', 'cases'], 'project route fixture');
  if (!Array.isArray(root.requiredNames) || root.requiredNames.some((name) => typeof name !== 'string')) throw new Error('project route fixture requiredNames must be strings');
  const requiredNames = root.requiredNames as string[];
  if (new Set(requiredNames).size !== requiredNames.length) throw new Error('project route fixture requiredNames must be unique');
  if (root.expectedCaseCount !== requiredNames.length) throw new Error(`project route fixture requires exactly ${requiredNames.length} cases`);
  if (!Array.isArray(root.cases) || root.cases.length !== requiredNames.length) throw new Error(`project route fixture requires exactly ${requiredNames.length} cases`);
  const cases = root.cases.map((value, index) => requireRecord(value, `project route fixture.cases[${index}]`));
  requireUniqueNames(cases, 'project route fixture.cases');
  cases.forEach((value, index) => requireExactRequiredFields(value, fields, `project route fixture.cases[${index}]`));
  for (const name of requiredNames) if (!cases.some((value) => value.name === name)) throw new Error(`project route fixture is missing required semantic branch ${name}`);
  return cases as FixtureCase[];
}

const cases = loadFixture(source);

describe('project route fixture contract', () => {
  it('is strict, complete, and non-vacuous', () => {
    expect(() => loadFixture(source.replace('expectedCaseCount: 28', 'expectedCaseCount: 27'))).toThrow(/exactly 28/);
    expect(() => loadFixture(source.replace('canonical map round trip', 'renamed map behavior'))).toThrow(/missing required semantic branch/);
    expect(() => loadFixture(source.replace('expectedCaseCount: 28', 'unknown: true\nexpectedCaseCount: 28'))).toThrow(/fields/);
    expect(() => loadFixture(`${source}\n---\n{}`)).toThrow();
  });
});

describe.each(cases)('$name', (fixture) => {
  it('matches the canonical codec behavior', () => {
    const hash = parseProjectHash(fixture.projectHash);
    switch (fixture.operation) {
      case 'map':
        expect(hash && mapHref(hash, { node: 'internal/ingest', mode: 'navigator', grain: 'file' })).toBe(fixture.expected);
        break;
      case 'review':
        expect(hash && reviewHref(hash, { branch: fixture.branch })).toBe(fixture.expected);
        break;
      case 'transcript':
        expect(hash && transcriptHref(hash, fixture.sessionId, { turn: 12, origin: RouteOrigin.Review })).toBe(fixture.expected);
        break;
      case 'transcript-scoped': {
        const query = parseTranscriptRouteQuery(fixture.search);
        expect(query).toEqual({
          turn: 42,
          scope: TranscriptScope.File,
          scopeVal: 'src/api.ts',
          origin: RouteOrigin.Map,
          originNode: 'src/api.ts',
          originBranch: null,
          returnLocation: null,
        });
        expect(hash && query && transcriptHref(hash, fixture.sessionId, {
          turn: query.turn ?? undefined,
          scope: query.scope ?? undefined,
          scopeVal: query.scopeVal,
          origin: query.origin ?? undefined,
          originNode: query.originNode ?? undefined,
        })).toBe(fixture.expected);
        break;
      }
      case 'legacy': {
        const route = fixture.pathname.startsWith('/review')
          ? parseReviewRoute(fixture.pathname)
          : fixture.pathname.startsWith('/projects')
            ? parseTranscriptRoute(fixture.pathname)
            : parseMapRoute(fixture.pathname);
        expect(route.kind === 'legacy' ? route.projectLabel : '').toBe(fixture.expected);
        break;
      }
      case 'return-reject':
        expect(parseReturnLocation(fixture.rawReturn)).toBeNull();
        break;
      case 'return-accept':
        expect(parseReturnLocation(fixture.rawReturn)?.href).toBe(fixture.expected);
        break;
      case 'popstate': {
        const state = parseMapRouteState(fixture.pathname, fixture.search);
        expect(state && formatMapRouteState(state)).toBe(fixture.expected);
        break;
      }
      case 'route-reject': {
        const route = fixture.pathname.startsWith('/review')
          ? parseReviewRoute(fixture.pathname)
          : fixture.pathname.startsWith('/projects')
            ? parseTranscriptRoute(fixture.pathname)
            : parseMapRoute(fixture.pathname);
        expect(route.kind === 'malformed' ? route.code : route.kind).toBe(fixture.expected);
        break;
      }
      case 'project-detail': {
        const route = parseProjectDetailRoute(fixture.pathname);
        expect(route.kind === 'canonical' ? route.projectHash : route.kind === 'legacy' ? route.projectLabel : route.kind).toBe(fixture.expected);
        break;
      }
      default:
        throw new Error(`project route fixture operation ${fixture.operation} is unsupported`);
    }
  });
});
