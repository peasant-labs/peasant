import { beforeEach, describe, expect, it, vi } from 'vitest';

const injectedFailure = vi.hoisted(() => ({ kind: null as 'normalization' | 'unrelated' | null }));

vi.mock('@peasant-labs/fairtrade/graph', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@peasant-labs/fairtrade/graph')>();
  return {
    ...actual,
    createCodeMapState(seed: Parameters<typeof actual.createCodeMapState>[0]) {
      if (injectedFailure.kind === 'normalization') {
        return actual.createCodeMapState({ ...(seed ?? {}), expandedIds: [''] });
      }
      if (injectedFailure.kind === 'unrelated') {
        throw new Error('unexpected application failure');
      }
      return actual.createCodeMapState(seed);
    },
  };
});

import { parseCodeMapLocation } from './codeMapRouteState';

const PROJECT_ROUTE = '/map/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa';

describe('code-map route normalization boundary', () => {
  beforeEach(() => {
    injectedFailure.kind = null;
  });

  it('translates a Fairtrade actionable normalization failure into route rejection', () => {
    injectedFailure.kind = 'normalization';
    expect(parseCodeMapLocation(PROJECT_ROUTE, '?mode=navigator&grain=package')).toBeNull();
  });

  it('does not swallow unrelated application failures', () => {
    injectedFailure.kind = 'unrelated';
    expect(() => parseCodeMapLocation(PROJECT_ROUTE, '?mode=navigator&grain=package'))
      .toThrow('unexpected application failure');
  });
});
