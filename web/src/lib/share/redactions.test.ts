import { afterEach, describe, expect, it, vi } from 'vitest';
import { fetchRedactionPreview } from '@/lib/share/redactions';
import { REDACTION_CATEGORY_FIXTURES } from '@/test/fixtures/redaction-preview';

describe('fetchRedactionPreview category contract', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

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
