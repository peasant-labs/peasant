import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { DiscoveryRequestError } from '@/lib/api/errors';
import { parseStrictYAML, requireExactRequiredFields, requireRecord, requireUniqueNames } from '@/test/strictYaml';
import { ReviewRouter } from './ReviewRouter';

let pathname = '/review';
let search = '';
const replace = vi.fn();
const push = vi.fn();
const fetchProjectResolution = vi.fn();
const fetchReviewChanges = vi.fn();
const fetchChangeDetail = vi.fn();
const fetchChangeDiff = vi.fn();

vi.mock('next/navigation', () => ({ usePathname: () => pathname, useSearchParams: () => new URLSearchParams(search), useRouter: () => ({ replace, push }) }));
vi.mock('@/lib/api/map', () => ({
  fetchProjectResolution: (...args: unknown[]) => fetchProjectResolution(...args),
  fetchReviewChanges: (...args: unknown[]) => fetchReviewChanges(...args),
  fetchChangeDetail: (...args: unknown[]) => fetchChangeDetail(...args),
  fetchChangeDiff: (...args: unknown[]) => fetchChangeDiff(...args),
}));
vi.mock('@peasant-labs/fairtrade/graph', () => ({
  Changes: ({ projectLabel, payload }: { projectLabel: string; payload: { sessions: Array<{ sessionId: string; title: string }> } }) => (
    <div data-testid="changes">
      <span>{projectLabel}</span>
      {payload.sessions.map((session) => <button key={session.sessionId}>{session.title}</button>)}
    </div>
  ),
  ChangeDetail: ({ payload }: { payload: { branch: string } }) => (
    <div data-testid="change-detail"><span>{payload.branch}</span><button>open {payload.branch}</button></div>
  ),
}));

type FixtureCase = { name: string; scenario: string; pathname: string; search: string; resolution: string; expected: string };
const source = readFileSync(resolve(process.cwd(), 'src/app/review/[[...segments]]/testdata/mounted_review_router.yaml'), 'utf8');
const caseFields = ['name', 'scenario', 'pathname', 'search', 'resolution', 'expected'] as const;

function loadFixture(value: string): FixtureCase[] {
  const root = requireRecord(parseStrictYAML(value, 'mounted review router fixture'), 'mounted review router fixture');
  requireExactRequiredFields(root, ['expectedCaseCount', 'requiredNames', 'cases'], 'mounted review router fixture');
  if (!Number.isInteger(root.expectedCaseCount) || !Array.isArray(root.requiredNames) || !Array.isArray(root.cases)) throw new Error('mounted review router fixture requires a count, requiredNames, and cases');
  const names = root.requiredNames;
  if (names.length === 0 || names.some((name) => typeof name !== 'string' || name.length === 0)) throw new Error('mounted review router requiredNames must be nonempty strings');
  if (new Set(names).size !== names.length) throw new Error('mounted review router requiredNames must be unique');
  const cases = root.cases.map((row, index) => requireRecord(row, `mounted review router cases[${index}]`));
  requireUniqueNames(cases, 'mounted review router cases');
  cases.forEach((row, index) => requireExactRequiredFields(row, caseFields, `mounted review router cases[${index}]`));
  const caseNames = cases.map((row) => row.name);
  if (root.expectedCaseCount !== names.length || cases.length !== names.length || names.some((name) => !caseNames.includes(name)) || caseNames.some((name) => !names.includes(name))) throw new Error('mounted review router requiredNames and cases must have exact set equality and count');
  return cases as FixtureCase[];
}

const cases = loadFixture(source);
const hash = 'a'.repeat(64);
const reviewPayload = {
  repoFound: true,
  defaultBranch: 'main',
  changes: [],
  recentCommits: [],
  sessions: [] as Array<{ sessionId: string; title: string; harness: string; startMs: number }>,
};

type ReplacementCase = {
  name: string;
  kind: 'project' | 'branch';
  fromPath: string;
  fromSearch: string;
  toPath: string;
  toSearch: string;
  oldLabel: string;
  oldAction: string;
  newLabel: string;
  newAction: string;
};
const replacementSource = readFileSync(resolve(process.cwd(), 'src/app/review/[[...segments]]/testdata/mounted_review_replacements.yaml'), 'utf8');
const requiredReplacementNames = [
  'canonical project replacement clears the previous list',
  'canonical branch replacement clears the previous detail',
] as const;

function loadReplacementFixture(value: string): ReplacementCase[] {
  const root = requireRecord(parseStrictYAML(value, 'mounted review replacement fixture'), 'mounted review replacement fixture');
  requireExactRequiredFields(root, ['expectedCaseCount', 'requiredNames', 'cases'], 'mounted review replacement fixture');
  if (root.expectedCaseCount !== requiredReplacementNames.length || !Array.isArray(root.requiredNames) || !Array.isArray(root.cases)) throw new Error('mounted review replacement fixture has an invalid inventory');
  const requiredNames = root.requiredNames;
  if (requiredNames.length !== requiredReplacementNames.length || new Set(requiredNames).size !== requiredNames.length || requiredReplacementNames.some((name) => !requiredNames.includes(name))) throw new Error('mounted review replacement fixture requiredNames must have exact set equality');
  const cases = root.cases.map((row, index) => requireRecord(row, `mounted review replacement cases[${index}]`));
  requireUniqueNames(cases, 'mounted review replacement cases');
  cases.forEach((row, index) => {
    requireExactRequiredFields(row, ['name', 'kind', 'fromPath', 'fromSearch', 'toPath', 'toSearch', 'oldLabel', 'oldAction', 'newLabel', 'newAction'], `mounted review replacement cases[${index}]`);
    if (!requiredNames.includes(row.name) || !['project', 'branch'].includes(String(row.kind))) throw new Error(`mounted review replacement case ${index} is invalid`);
  });
  if (cases.length !== requiredNames.length || requiredNames.some((name) => !cases.some((testCase) => testCase.name === name))) throw new Error('mounted review replacement cases must have exact set equality');
  return cases as ReplacementCase[];
}

const replacementCases = loadReplacementFixture(replacementSource);

function deferred<T>() {
  let resolvePromise!: (value: T) => void;
  const promise = new Promise<T>((resolveValue) => { resolvePromise = resolveValue; });
  return { promise, resolve: resolvePromise };
}

const detailPayload = (branch: string) => ({
  branch, baseRef: 'abc', defaultBranch: 'main', files: [],
  slice: { nodes: [], structureEdges: [], activityEdges: [] },
  newEdges: [], removedEdges: [], newNodes: [], removedNodes: [], violations: [],
  work: [], unrecordedCommits: [], unusual: [], frictions: [], linesAdded: 0,
  linesRemoved: 0, outputTokens: 0, costUsd: null,
});

describe('mounted ReviewRouter fixture', () => {
  beforeEach(() => {
    replace.mockReset();
    push.mockReset();
    fetchProjectResolution.mockReset();
    fetchReviewChanges.mockReset();
    fetchChangeDetail.mockReset();
    fetchChangeDiff.mockReset();
    fetchReviewChanges.mockResolvedValue(reviewPayload);
  });
  afterEach(() => cleanup());

  it('rejects structural and semantic fixture mutations', () => {
    expect(() => loadFixture(source.replace('expectedCaseCount: 8', 'unknown: true\nexpectedCaseCount: 8'))).toThrow(/fields/);
    expect(() => loadFixture(`${source}\n---\n{}`)).toThrow();
    const firstRequired = source.match(/requiredNames:\n  - ([^\n]+)/)![1];
    expect(() => loadFixture(source.replace(`  - ${firstRequired}\n  - `, `  - ${firstRequired}\n  - ${firstRequired}\n  - `))).toThrow(/unique/);
    expect(() => loadFixture(source.replace(`  - ${firstRequired}\n`, ''))).toThrow(/exact set equality/);
    const caseNames = [...source.matchAll(/  - \{name: ([^,]+)/g)].map((match) => match[1]);
    expect(() => loadFixture(source.replace(`name: ${caseNames[1]}`, `name: ${caseNames[0]}`))).toThrow(/duplicate/);
    expect(() => loadFixture(source.replace(/  - \{name: transient legacy resolution can retry[^\n]+\n/, ''))).toThrow(/exact set equality/);
  });

  for (const fixture of cases) it(fixture.name, async () => {
    pathname = fixture.pathname;
    search = fixture.search;
    if (fixture.resolution === 'ready') fetchProjectResolution.mockResolvedValue({ project: fixture.scenario === 'canonical' ? fixture.expected : 'team alpha', projectHash: hash });
    if (fixture.resolution === 'missing') fetchProjectResolution.mockRejectedValue(new DiscoveryRequestError('/api/v1/projects/resolve', 404, fixture.expected));
    if (fixture.resolution === 'transient') fetchProjectResolution.mockRejectedValueOnce(new Error('temporary provider failure')).mockResolvedValueOnce({ project: 'team', projectHash: hash });
    render(<ReviewRouter />);

    switch (fixture.scenario) {
      case 'canonical':
        expect(await screen.findByTestId('changes')).toHaveTextContent(fixture.expected);
        expect(fetchReviewChanges).toHaveBeenCalledWith(hash);
        expect(screen.queryByText(hash)).not.toBeInTheDocument();
        break;
      case 'legacy':
        await waitFor(() => expect(replace).toHaveBeenCalledWith(fixture.expected));
        break;
      case 'malformed':
        expect(await screen.findByRole('alert')).toHaveTextContent(fixture.expected);
        expect(fetchProjectResolution).not.toHaveBeenCalled();
        expect(fetchReviewChanges).not.toHaveBeenCalled();
        expect(fetchChangeDetail).not.toHaveBeenCalled();
        expect(fetchChangeDiff).not.toHaveBeenCalled();
        break;
      case 'route-missing':
        expect(await screen.findByText(fixture.expected)).toBeInTheDocument();
        expect(fetchReviewChanges).not.toHaveBeenCalled();
        break;
      case 'missing':
        expect(await screen.findByRole('button', { name: /retry project resolution/i })).toHaveTextContent(fixture.expected);
        break;
      case 'retry':
        fireEvent.click(await screen.findByRole('button', { name: /retry project resolution/i }));
        await waitFor(() => expect(replace).toHaveBeenCalledWith(fixture.expected));
        expect(fetchProjectResolution).toHaveBeenCalledTimes(2);
        break;
      default: throw new Error(`unsupported fixture scenario ${fixture.scenario}`);
    }
  });
});

describe('mounted ReviewRouter canonical replacements', () => {
  beforeEach(() => {
    replace.mockReset();
    push.mockReset();
    fetchProjectResolution.mockReset();
    fetchReviewChanges.mockReset();
    fetchChangeDetail.mockReset();
    fetchChangeDiff.mockReset();
  });
  afterEach(() => cleanup());

  it('rejects replacement fixture drift', () => {
    expect(() => loadReplacementFixture(replacementSource.replace('expectedCaseCount: 2', 'unknown: true\nexpectedCaseCount: 2'))).toThrow();
    expect(() => loadReplacementFixture(`${replacementSource}\n---\n{}`)).toThrow();
    expect(() => loadReplacementFixture(replacementSource.replace('canonical branch replacement clears the previous detail', 'renamed branch behavior'))).toThrow(/exact set equality/);
    expect(() => loadReplacementFixture(replacementSource.replace(/  - name: canonical branch replacement clears the previous detail[\s\S]*$/, ''))).toThrow(/exact set equality/);
  });

  for (const fixture of replacementCases) it(fixture.name, async () => {
    const nextReview = deferred<typeof reviewPayload>();
    const nextDetail = deferred<ReturnType<typeof detailPayload>>();
    fetchProjectResolution.mockImplementation(async (identity: string) => ({
      project: identity.startsWith('b') ? fixture.newLabel : fixture.kind === 'project' ? fixture.oldLabel : 'project alpha',
      projectHash: identity,
    }));

    if (fixture.kind === 'project') {
      fetchReviewChanges
        .mockResolvedValueOnce({ ...reviewPayload, sessions: [{ sessionId: 'old', title: fixture.oldAction, harness: 'claude-code', startMs: 1 }] })
        .mockReturnValueOnce(nextReview.promise);
    } else {
      fetchReviewChanges.mockResolvedValueOnce(reviewPayload).mockReturnValueOnce(nextReview.promise);
      fetchChangeDetail.mockResolvedValueOnce(detailPayload(fixture.oldLabel)).mockReturnValueOnce(nextDetail.promise);
    }

    pathname = fixture.fromPath;
    search = fixture.fromSearch;
    const mounted = render(<ReviewRouter />);
    expect(await screen.findByText(fixture.oldLabel)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: fixture.oldAction })).toBeInTheDocument();

    pathname = fixture.toPath;
    search = fixture.toSearch;
    mounted.rerender(<ReviewRouter />);
    expect(screen.queryByText(fixture.oldLabel)).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: fixture.oldAction })).not.toBeInTheDocument();

    if (fixture.kind === 'project') {
      nextReview.resolve({ ...reviewPayload, sessions: [{ sessionId: 'new', title: fixture.newAction, harness: 'claude-code', startMs: 2 }] });
    } else {
      nextReview.resolve(reviewPayload);
      nextDetail.resolve(detailPayload(fixture.newLabel));
    }
    expect(await screen.findByText(fixture.newLabel)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: fixture.newAction })).toBeInTheDocument();
  });
});
