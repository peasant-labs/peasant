import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { parse } from 'yaml';
import { AllRedactionCategoryLabels, RedactionCategoryLabel, type SyncRedactionsResponse } from '@peasant-labs/schema';
import type { Redaction } from '@/types/messages';
import type { RedactionLevel } from '@/lib/share/redactions';

interface BoundaryCase {
  name: string;
  source: 'network' | 'cache' | 'mock' | 'adapter';
  mutation: 'none' | 'unknown-empty-group' | 'unknown-item' | 'group-mismatch' | 'malformed' | 'wrong-total' | 'empty-positive' | 'wrong-group-count' | 'wrong-rule-count';
  error?: string;
  minimumLevel?: RedactionLevel;
}
const fixture = parse(readFileSync(resolve(process.cwd(), 'src/test/testdata/redaction-review-boundaries.yaml'), 'utf8')) as { requiredNames: string[]; cases: BoundaryCase[] };
if (new Set(fixture.cases.map((entry) => entry.name)).size !== fixture.cases.length
  || fixture.requiredNames.some((name) => !fixture.cases.some((entry) => entry.name === name))
  || fixture.cases.some((entry) => !fixture.requiredNames.includes(entry.name))) throw new Error('redaction boundary fixture must retain every required scenario exactly once');
export const REDACTION_REVIEW_BOUNDARIES = fixture.cases;

/** Wire responses stay label-valued; no consumer semantic map in the fixture. */
export function boundaryResponse(mutation: BoundaryCase['mutation'] = 'none'): unknown {
  const response: SyncRedactionsResponse = {
    total: AllRedactionCategoryLabels.length,
    categories: AllRedactionCategoryLabels.map((category) => ({
      category, totalCount: 1, rules: [{ ruleId: category, displayName: category, count: 1,
        items: [{ category, ruleId: category, ruleDisplayName: category, originalText: `fixture ${category}`, redactedReplacement: '<REDACTED>', description: category, lineNumber: 1, contextBefore: [], contextAfter: [] }],
      }],
    })),
  };
  const group = response.categories[0];
  switch (mutation) {
    case 'none': break;
    case 'unknown-empty-group': response.categories.push({ category: 'LOCATION', rules: [], totalCount: 0 }); break;
    case 'unknown-item': group.rules[0].items[0].category = 'LOCATION'; break;
    case 'group-mismatch': group.rules[0].items[0].category = RedactionCategoryLabel.PII; break;
    case 'malformed': return { total: 0 };
    case 'wrong-total': response.total = 0; break;
    case 'empty-positive': response.categories = []; break;
    case 'wrong-group-count': group.totalCount++; break;
    case 'wrong-rule-count': group.rules[0].count = 0; break;
  }
  return response;
}

/** Deliberately untrusted domain input models a corrupt cache or mock producer. */
export function boundaryDomain(mutation: BoundaryCase['mutation']): Redaction[] {
  const items: Redaction[] = AllRedactionCategoryLabels.map((category) => ({
    id: category, category, confidence: 100, lineNumber: 1, contextBefore: [], contextAfter: [], originalText: `fixture ${category}`, redactedReplacement: '<REDACTED>', description: category, status: 'pending',
  }));
  if (mutation === 'unknown-item') Object.assign(items[0], { category: 'LOCATION' });
  if (mutation === 'malformed') Object.assign(items[0], { originalText: null });
  return items;
}
