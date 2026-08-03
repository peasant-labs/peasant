import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { act, renderHook, waitFor } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { DiscoveryRequestError } from '@/lib/api/errors';
import { parseStrictYAML, requireExactRequiredFields, requireRecord, requireUniqueNames } from '@/test/strictYaml';
import { useProjectIdentity } from './useProjectIdentity';

const { fetchProjectResolution } = vi.hoisted(() => ({ fetchProjectResolution: vi.fn() }));
vi.mock('@/lib/api/map', () => ({ fetchProjectResolution }));

const source = readFileSync(resolve(process.cwd(), 'src/lib/navigation/testdata/project_identity.yaml'), 'utf8');
const caseFields = ['name', 'scenario', 'requested', 'replacement', 'expectedPhase', 'expectedHash'] as const;
type FixtureCase = Record<(typeof caseFields)[number], string>;

function loadFixture(yaml: string): FixtureCase[] {
  const root = requireRecord(parseStrictYAML(yaml, 'project identity fixture'), 'project identity fixture');
  requireExactRequiredFields(root, ['expectedCaseCount', 'requiredNames', 'cases'], 'project identity fixture');
  if (!Array.isArray(root.requiredNames) || root.requiredNames.some((name) => typeof name !== 'string')) throw new Error('project identity fixture requiredNames must be strings');
  const requiredNames = root.requiredNames as string[];
	if (new Set(requiredNames).size !== requiredNames.length) throw new Error('project identity fixture requiredNames must be unique');
  if (root.expectedCaseCount !== requiredNames.length) throw new Error(`project identity fixture requires exactly ${requiredNames.length} cases`);
  if (!Array.isArray(root.cases) || root.cases.length !== requiredNames.length) throw new Error(`project identity fixture requires exactly ${requiredNames.length} cases`);
  const cases = root.cases.map((value, index) => requireRecord(value, `project identity fixture.cases[${index}]`));
  requireUniqueNames(cases, 'project identity fixture.cases');
  cases.forEach((value, index) => requireExactRequiredFields(value, caseFields, `project identity fixture.cases[${index}]`));
  for (const name of requiredNames) if (!cases.some((value) => value.name === name)) throw new Error(`project identity fixture is missing required semantic branch ${name}`);
	if (cases.some((value) => !requiredNames.includes(String(value.name)))) throw new Error('project identity fixture cases must exactly match requiredNames');
  return cases as FixtureCase[];
}

function deferred<T>() {
  let resolvePromise!: (value: T) => void;
  let rejectPromise!: (reason: unknown) => void;
  const promise = new Promise<T>((resolve, reject) => {
    resolvePromise = resolve;
    rejectPromise = reject;
  });
  return { promise, resolve: resolvePromise, reject: rejectPromise };
}

const cases = loadFixture(source);
const alphaHash = 'a'.repeat(64);
const betaHash = 'b'.repeat(64);

describe('project identity fixture contract', () => {
  it('is strict, complete, and non-vacuous', () => {
    expect(() => loadFixture(source.replace('expectedCaseCount: 7', 'expectedCaseCount: 6'))).toThrow(/exactly 7/);
    expect(() => loadFixture(source.replace('resolves a canonical project hash', 'renamed behavior'))).toThrow(/missing required semantic branch/);
    expect(() => loadFixture(source.replace('expectedCaseCount: 7', 'unknown: true\nexpectedCaseCount: 7'))).toThrow(/fields/);
    expect(() => loadFixture(source.replace('  - reports a missing project\n', '  - resolves a canonical project hash\n'))).toThrow(/unique/);
    expect(() => loadFixture(source.replace('  - name: reports a missing project\n', ''))).toThrow();
    expect(() => loadFixture(`${source}\n---\n{}`)).toThrow();
  });
});

describe.each(cases)('$name', (fixture) => {
  beforeEach(() => fetchProjectResolution.mockReset());

  it('keeps identity resolution fail-closed', async () => {
    if (fixture.scenario === 'ready') {
      fetchProjectResolution.mockResolvedValue({ project: '/repo/alpha', projectHash: alphaHash });
      const { result } = renderHook(() => useProjectIdentity(fixture.requested));
      await waitFor(() => expect(result.current.state.phase).toBe(fixture.expectedPhase));
      expect(result.current.state.phase === 'ready' ? result.current.state.projectHash : '').toBe(fixture.expectedHash);
      return;
    }
    if (fixture.scenario === 'missing') {
      const error = new DiscoveryRequestError('/api/v1/projects/resolve', 404, 'project was not found');
      fetchProjectResolution.mockResolvedValue({
        project: '/repo/absent',
        get projectHash() {
          throw error;
        },
      });
      const { result } = renderHook(() => useProjectIdentity(fixture.requested));
      await waitFor(() => expect(result.current.state.phase).toBe(fixture.expectedPhase));
      return;
    }
    if (fixture.scenario === 'malformed') {
      fetchProjectResolution.mockResolvedValue({ project: '/repo/alpha', projectHash: 'NOT-A-HASH' });
      const { result } = renderHook(() => useProjectIdentity(fixture.requested));
      await waitFor(() => expect(result.current.state.phase).toBe(fixture.expectedPhase));
      expect(result.current.state.phase === 'error' ? result.current.state.message : '').toMatch(/malformed identity/);
      return;
    }
    if (fixture.scenario === 'retry') {
      fetchProjectResolution
        .mockRejectedValueOnce(new Error('temporary provider failure'))
        .mockResolvedValueOnce({ project: '/repo/alpha', projectHash: alphaHash });
      const { result } = renderHook(() => useProjectIdentity(fixture.requested));
      await waitFor(() => expect(result.current.state.phase).toBe('error'));
      act(() => result.current.retry());
      await waitFor(() => expect(result.current.state.phase).toBe(fixture.expectedPhase));
      expect(result.current.state.phase === 'ready' ? result.current.state.projectHash : '').toBe(fixture.expectedHash);
      return;
    }
    if (fixture.scenario === 'stale') {
      const first = deferred<{ project: string; projectHash: string }>();
      const second = deferred<{ project: string; projectHash: string }>();
      fetchProjectResolution.mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise);
      const { result, rerender } = renderHook(({ identity }) => useProjectIdentity(identity), { initialProps: { identity: fixture.requested } });
      rerender({ identity: fixture.replacement });
      await act(async () => second.resolve({ project: '/repo/beta', projectHash: betaHash }));
      await waitFor(() => expect(result.current.state.phase).toBe(fixture.expectedPhase));
      await act(async () => first.resolve({ project: '/repo/alpha', projectHash: alphaHash }));
      expect(result.current.state.phase === 'ready' ? result.current.state.projectHash : '').toBe(fixture.expectedHash);
      return;
    }
    if (fixture.scenario === 'ready-then-replace') {
      const replacement = deferred<{ project: string; projectHash: string }>();
      fetchProjectResolution
        .mockResolvedValueOnce({ project: '/repo/alpha', projectHash: alphaHash })
        .mockReturnValueOnce(replacement.promise);
      const { result, rerender } = renderHook(({ identity }) => useProjectIdentity(identity), { initialProps: { identity: fixture.requested } });
      await waitFor(() => expect(result.current.state.phase).toBe('ready'));
      rerender({ identity: fixture.replacement });
      expect(result.current.state).toEqual({ phase: fixture.expectedPhase, requestedIdentity: fixture.replacement });
      expect(result.current.state.phase === 'ready' ? result.current.state.projectHash : '').toBe(fixture.expectedHash);
      return;
    }
    if (fixture.scenario === 'unmount') {
      const pending = deferred<{ project: string; projectHash: string }>();
      fetchProjectResolution.mockReturnValue(pending.promise);
      const { unmount } = renderHook(() => useProjectIdentity(fixture.requested));
      unmount();
      await act(async () => pending.resolve({ project: '/repo/alpha', projectHash: alphaHash }));
      expect(fetchProjectResolution).toHaveBeenCalledOnce();
      return;
    }
    throw new Error(`project identity fixture scenario ${fixture.scenario} is unsupported`);
  });
});
