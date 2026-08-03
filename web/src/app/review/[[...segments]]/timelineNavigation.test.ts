import { describe, expect, it } from 'vitest';
import YAML from 'yaml';
import { parseProjectHash } from '@/lib/navigation/projectRoutes';
import { resolveTimelineNavigation } from './timelineNavigation';
import {
  loadTimelineNavigationFixture,
  loadTimelineNavigationManifest,
  timelineNavigationFixture,
  timelineNavigationManifestSource,
  timelineNavigationSource,
  type TimelineNavigationMutation,
} from './timelineNavigation.fixture';

const projectHash = parseProjectHash(
  'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
);
if (!projectHash) throw new Error('timeline navigation test project hash fixture is invalid');

describe('timeline navigation route boundary', () => {
  for (const testCase of timelineNavigationFixture.cases) {
    it(`resolves ${testCase.name} through the canonical route codec`, () => {
      const resolveCase = () =>
        resolveTimelineNavigation(testCase.action, {
          projectHash,
          ...testCase.context,
        });
      if (testCase.expected.kind === 'error') {
        expect(resolveCase).toThrowError(testCase.expected.message);
      } else {
        expect(resolveCase()).toEqual(testCase.expected);
      }
    });
  }

  it('rejects removal or renaming of every independently manifested behavior case', () => {
    const manifest = loadTimelineNavigationManifest(timelineNavigationManifestSource);
    const parsed = YAML.parse(timelineNavigationSource);
    for (const manifested of manifest.cases) {
      const mutated = structuredClone(parsed);
      const testCase = mutated.cases.find((candidate: { name: string }) => candidate.name === manifested.name);
      testCase.name = `renamed_${manifested.name}`;
      expect(() => loadTimelineNavigationFixture(YAML.stringify(mutated), manifest)).toThrow(
        `missing behavior case ${manifested.name}`,
      );
    }
  });

  const manifest = loadTimelineNavigationManifest(timelineNavigationManifestSource);
  it.each(manifest.mutations)('rejects router-write mutation $name', (mutation) => {
    const mutated = applyRouterWriteMutation(YAML.parse(timelineNavigationSource), mutation);
    expect(() => loadTimelineNavigationFixture(YAML.stringify(mutated), manifest)).toThrow(
      mutation.diagnostic,
    );
  });

  it('fails closed and actionably for an unknown runtime action', () => {
    expect(() =>
      resolveTimelineNavigation(
        { type: 'open-neighboring-project', projectHash: 'unknown' },
        {
          projectHash,
          defaultBranch: 'develop',
          returnLocation: null,
          pagination: {
            cursorAvailable: false,
            handlerAvailable: false,
          },
        },
      ),
    ).toThrow(/Timeline navigation action rejected[\s\S]*How to fix:/);
  });
});

function applyRouterWriteMutation(
  parsed: { cases: Array<{ name: string; expectedRouterCalls: string[][] }> },
  mutation: TimelineNavigationMutation,
): typeof parsed {
  const mutated = structuredClone(parsed);
  const target = mutated.cases.find((candidate) => candidate.name === mutation.caseName);
  if (!target) {
    throw new Error(`timeline navigation mutation target ${mutation.caseName} is missing`);
  }
  switch (mutation.operation) {
    case 'append-router-call':
      target.expectedRouterCalls.push([mutation.value]);
      break;
    case 'replace-router-href':
      target.expectedRouterCalls[0][0] = mutation.value;
      break;
  }
  return mutated;
}
