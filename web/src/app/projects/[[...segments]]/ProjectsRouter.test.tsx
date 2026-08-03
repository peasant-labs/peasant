import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { DiscoveryRequestError } from '@/lib/api/errors';
import { parseStrictYAML, requireExactRequiredFields, requireRecord, requireUniqueNames } from '@/test/strictYaml';
import { ProjectsRouter } from './ProjectsRouter';

let pathname = '/projects';
let search = '';
const replace = vi.fn();
const fetchProjectResolution = vi.fn();

vi.mock('next/navigation', () => ({ usePathname: () => pathname, useSearchParams: () => new URLSearchParams(search), useRouter: () => ({ replace }) }));
vi.mock('@/lib/api/map', () => ({ fetchProjectResolution: (...args: unknown[]) => fetchProjectResolution(...args) }));
vi.mock('@/components/session-detail/v2/SessionDetailV2', () => ({
  SessionDetailV2: ({ projectHash, projectName, routeQuery }: { projectHash: string; projectName: string; routeQuery: { turn: number | null; scope: string | null; scopeVal: string } }) => <div data-testid="viewer" data-project-hash={projectHash} data-route-turn={routeQuery.turn ?? -1} data-route-scope={routeQuery.scope ?? 'none'} data-route-scope-value={routeQuery.scopeVal}>{projectName}</div>,
}));

type FixtureCase = { name: string; scenario: string; pathname: string; search: string; resolution: string; expected: string };
const source = readFileSync(resolve(process.cwd(), 'src/app/projects/[[...segments]]/testdata/mounted_projects_router.yaml'), 'utf8');
const caseFields = ['name', 'scenario', 'pathname', 'search', 'resolution', 'expected'] as const;

function loadFixture(value: string): FixtureCase[] {
  const root = requireRecord(parseStrictYAML(value, 'mounted projects router fixture'), 'mounted projects router fixture');
  requireExactRequiredFields(root, ['expectedCaseCount', 'requiredNames', 'cases'], 'mounted projects router fixture');
  if (!Number.isInteger(root.expectedCaseCount) || !Array.isArray(root.requiredNames) || !Array.isArray(root.cases)) throw new Error('mounted projects router fixture requires a count, requiredNames, and cases');
  const names = root.requiredNames;
  if (names.length === 0 || names.some((name) => typeof name !== 'string' || name.length === 0)) throw new Error('mounted projects router requiredNames must be nonempty strings');
  if (new Set(names).size !== names.length) throw new Error('mounted projects router requiredNames must be unique');
  const cases = root.cases.map((row, index) => requireRecord(row, `mounted projects router cases[${index}]`));
  requireUniqueNames(cases, 'mounted projects router cases');
  cases.forEach((row, index) => requireExactRequiredFields(row, caseFields, `mounted projects router cases[${index}]`));
  const caseNames = cases.map((row) => row.name);
  if (root.expectedCaseCount !== names.length || cases.length !== names.length || names.some((name) => !caseNames.includes(name)) || caseNames.some((name) => !names.includes(name))) throw new Error('mounted projects router requiredNames and cases must have exact set equality and count');
  return cases as FixtureCase[];
}

const cases = loadFixture(source);
const hash = 'a'.repeat(64);

describe('mounted ProjectsRouter fixture', () => {
  beforeEach(() => { replace.mockReset(); fetchProjectResolution.mockReset(); });
  afterEach(() => cleanup());

  it('rejects structural and semantic fixture mutations', () => {
    expect(() => loadFixture(source.replace('expectedCaseCount: 11', 'unknown: true\nexpectedCaseCount: 11'))).toThrow(/fields/);
    expect(() => loadFixture(`${source}\n---\n{}`)).toThrow();
    const firstRequired = source.match(/requiredNames:\n  - ([^\n]+)/)![1];
    expect(() => loadFixture(source.replace(`  - ${firstRequired}\n  - `, `  - ${firstRequired}\n  - ${firstRequired}\n  - `))).toThrow(/unique/);
    expect(() => loadFixture(source.replace(`  - ${firstRequired}\n`, ''))).toThrow(/exact set equality/);
    const caseNames = [...source.matchAll(/  - \{name: ([^,]+)/g)].map((match) => match[1]);
    expect(() => loadFixture(source.replace(`name: ${caseNames[1]}`, `name: ${caseNames[0]}`))).toThrow(/duplicate/);
    expect(() => loadFixture(source.replace(/  - \{name: legacy project detail canonicalizes identity[^\n]+\n/, ''))).toThrow(/exact set equality/);
  });

  for (const fixture of cases) it(fixture.name, async () => {
    pathname = fixture.pathname;
    search = fixture.search;
    if (fixture.resolution === 'ready') fetchProjectResolution.mockResolvedValue({ project: fixture.scenario === 'canonical-viewer' ? fixture.expected : 'team alpha', projectHash: hash });
    if (fixture.resolution === 'missing') fetchProjectResolution.mockRejectedValue(new DiscoveryRequestError('/api/v1/projects/resolve', 404, fixture.expected));
    if (fixture.resolution === 'transient') fetchProjectResolution.mockRejectedValueOnce(new Error('temporary provider failure')).mockResolvedValueOnce({ project: 'team', projectHash: hash });
    render(<ProjectsRouter />);

    switch (fixture.scenario) {
      case 'canonical-viewer': {
        const viewer = await screen.findByTestId('viewer');
        expect(viewer).toHaveTextContent(fixture.expected);
        expect(viewer).toHaveAttribute('data-project-hash', hash);
        expect(viewer).toHaveAttribute('data-route-turn', '42');
        expect(viewer).toHaveAttribute('data-route-scope', 'file');
        expect(viewer).toHaveAttribute('data-route-scope-value', 'src/api.ts');
        expect(screen.queryByText(hash)).not.toBeInTheDocument();
        break;
      }
      case 'legacy-viewer':
      case 'project-detail-legacy':
        await waitFor(() => expect(replace).toHaveBeenCalledWith(fixture.expected));
        break;
      case 'project-detail':
        await waitFor(() => expect(replace).toHaveBeenCalledWith(fixture.expected));
        expect(fetchProjectResolution).not.toHaveBeenCalled();
        break;
      case 'malformed':
        expect(await screen.findByRole('alert')).toHaveTextContent(fixture.expected);
        expect(fetchProjectResolution).not.toHaveBeenCalled();
        break;
      case 'missing':
        expect(await screen.findByRole('button', { name: /retry project resolution/i })).toHaveTextContent(fixture.expected);
        break;
      case 'retry': {
        fireEvent.click(await screen.findByRole('button', { name: /retry project resolution/i }));
        await waitFor(() => expect(replace).toHaveBeenCalledWith(fixture.expected));
        expect(fetchProjectResolution).toHaveBeenCalledTimes(2);
        break;
      }
      default: throw new Error(`unsupported fixture scenario ${fixture.scenario}`);
    }
  });
});
