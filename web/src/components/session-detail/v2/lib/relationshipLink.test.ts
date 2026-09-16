import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import { AllRelationshipNavigationStatuses, type SessionRelationshipNavigation } from '@peasant-labs/schema';
import { transcriptHref, type ProjectHash } from '@/lib/navigation/projectRoutes';
import { relationshipLinkHref } from './relationshipLink';
import {
  loadContextNavigationFixture,
  replaceExactlyOnce,
} from '@/test/contextNavigationFixture';

const CASES_PATH = 'src/components/session-detail/v2/testdata/context_navigation.yaml';
const MANIFEST_PATH = 'src/components/session-detail/v2/testdata/context_navigation.manifest.yaml';
const PROJECT_HASH = 'a'.repeat(64) as ProjectHash;

const casesSource = readFileSync(resolve(process.cwd(), CASES_PATH), 'utf8');
const manifestSource = readFileSync(resolve(process.cwd(), MANIFEST_PATH), 'utf8');
const fixture = loadContextNavigationFixture(casesSource, manifestSource);

describe('relationship link routing', () => {
  it('covers the whole published navigation status set exactly once per status', () => {
    const declared = new Set(fixture.linkCases.map((entry) => entry.status));
    expect(declared).toEqual(new Set(AllRelationshipNavigationStatuses));
    expect(fixture.requiredStatuses.slice().sort()).toEqual([...AllRelationshipNavigationStatuses].slice().sort());
  });

  it('rejects every strict loader mutation from the fixture manifest', () => {
    for (const mutation of fixture.mutations) {
      const mutated = replaceExactlyOnce(
        mutation.target === 'cases' ? casesSource : manifestSource,
        mutation.find,
        mutation.replace,
        mutation.name,
      );
      expect(
        () =>
          loadContextNavigationFixture(
            mutation.target === 'cases' ? mutated : casesSource,
            mutation.target === 'manifest' ? mutated : manifestSource,
          ),
        mutation.name,
      ).toThrow(new RegExp(mutation.expectedError));
    }
  });

  for (const linkCase of fixture.linkCases) {
    it(linkCase.name, () => {
      const navigation = {
        kind: linkCase.kind,
        status: linkCase.status,
        ...(linkCase.localId ? { localId: linkCase.localId } : {}),
      } as unknown as SessionRelationshipNavigation;
      const href = relationshipLinkHref(PROJECT_HASH, navigation);
      expect(href).toBe(linkCase.expectedTarget ? transcriptHref(PROJECT_HASH, linkCase.expectedTarget) : null);
    });
  }

  it('routes to the exact stored target, never to a nearby identifier', () => {
    const href = relationshipLinkHref(PROJECT_HASH, {
      kind: 'context_from',
      status: 'general_link_only',
      localId: 'sess_contextsource',
    } as unknown as SessionRelationshipNavigation);
    expect(href).toBe(`/projects/${PROJECT_HASH}/sess_contextsource`);
    expect(href).not.toContain('sess_contextchild');
  });
});
