import { isRedactionCategory, redactionCategoryFromLabel, type RedactionCategory } from '@peasant-labs/schema';
import type { Redaction } from '@/types/messages';

export interface ReviewMatch {
  id: string;
  category: RedactionCategory;
  confidence: number;
  before: string;
  after: string;
  kept: boolean;
}

export function reviewMatches(sessionId: string, redactions: readonly Redaction[]): ReviewMatch[] {
  return redactions.map((redaction) => ({
    id: `${sessionId}::${redaction.id}`,
    category: redactionCategoryFromLabel(redaction.category),
    confidence: redaction.confidence / 100,
    before: redaction.originalText,
    after: redaction.redactedReplacement,
    kept: false,
  }));
}

/** Conversion failure belongs to owner readiness, not an exception from render. */
export function guardReviewMatches(adapt: () => ReviewMatch[]): { matches: ReviewMatch[]; error: string | null } {
  try {
    const matches = adapt();
    if (!matches.every((match) => isRedactionCategory(match.category))) throw new TypeError('unsupported review category');
    return { matches, error: null };
  } catch {
    return { matches: [], error: 'redaction review could not classify the scan while preparing publication; sharing remains blocked. Update the review packages and re-scan.' };
  }
}
