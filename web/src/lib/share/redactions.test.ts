import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  SELECTABLE_REDACTION_LEVELS,
  fetchRedactionPreview,
  isSelectableRedactionLevel,
} from '@/lib/share/redactions';
import { ALL_REDACTION_LEVELS } from '@/lib/share/redaction-policy.generated';
import { boundaryResponse, REDACTION_REVIEW_BOUNDARIES } from '@/test/fixtures/redaction-review-boundaries';
import {
  REDACTION_CATEGORY_FIXTURES,
  UNKNOWN_REDACTION_LEVEL_FIXTURES,
} from '@/test/fixtures/redaction-preview';

describe('redaction level policy contract', () => {
  it('accepts exactly the generated selectable subset', () => {
    expect(SELECTABLE_REDACTION_LEVELS.length).toBeGreaterThan(0);
    for (const level of SELECTABLE_REDACTION_LEVELS) {
      expect(isSelectableRedactionLevel(level)).toBe(true);
    }

    const unselectable = ALL_REDACTION_LEVELS.filter(
      (level) => !(SELECTABLE_REDACTION_LEVELS as readonly string[]).includes(level),
    );
    expect(unselectable.length).toBeGreaterThan(0);
    for (const level of unselectable) {
      expect(isSelectableRedactionLevel(level)).toBe(false);
    }

    for (const level of UNKNOWN_REDACTION_LEVEL_FIXTURES) {
      expect(ALL_REDACTION_LEVELS as readonly string[]).not.toContain(level);
      expect(isSelectableRedactionLevel(level)).toBe(false);
    }
  });
});

describe('fetchRedactionPreview category contract', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  for (const entry of REDACTION_REVIEW_BOUNDARIES.filter((row) => row.source === 'network')) {
    it(`validates the generated response and cross-field counts: ${entry.name}`, async () => {
      vi.stubGlobal('fetch', vi.fn(async () => Response.json(boundaryResponse(entry.mutation))));
      const result = fetchRedactionPreview('fixture-session', 'standard');
      if (entry.error) await expect(result).rejects.toThrow(entry.error);
      else await expect(result).resolves.toHaveLength(4);
    });
  }

  for (const fixture of REDACTION_CATEGORY_FIXTURES) {
    it(fixture.name, async () => {
      vi.stubGlobal('fetch', vi.fn(async () => ({
        ok: true,
        json: async () => ({
          total: 1,
          categories: [{
            category: fixture.groupCategory,
            totalCount: 1,
            rules: [{
              ruleId: 'fixture_rule',
              displayName: 'Fixture rule',
              count: 1,
              items: [{
                category: fixture.itemCategory,
                ruleId: 'fixture_rule',
                ruleDisplayName: 'Fixture rule',
                originalText: 'fixture-sensitive-value',
                redactedReplacement: '<FIXTURE>',
                description: 'Fixture category contract',
                lineNumber: 1,
                contextBefore: [],
                contextAfter: [],
              }],
            }],
          }],
        }),
      })));

      const preview = fetchRedactionPreview('fixture-session', 'standard');
      if (fixture.expectedError) {
        await expect(preview).rejects.toThrow(fixture.expectedError);
        return;
      }
      await expect(preview).resolves.toEqual([
        expect.objectContaining({ category: fixture.expectedCategory }),
      ]);
    });
  }
});
