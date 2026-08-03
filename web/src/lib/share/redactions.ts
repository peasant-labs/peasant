/**
 * Real redaction-preview client. Calls the LOCAL scan endpoint
 * (GET /api/v1/sync/redactions?session_id=&level=) — a genuine `redact.Detect`
 * over the session's transcript on this machine — and flattens the server's
 * category→rule→item grouping into the flat `Redaction[]` the RedactionDiffView
 * consumes. This replaces the old mock preview so the review shows what was
 * actually detected in the user's own content before anything is shared.
 *
 * Note: the endpoint caps items per rule for display, so the preview shows a
 * representative sample of each rule's matches, not every single occurrence.
 */

import type { Redaction, RedactionCategory } from '@/types/messages';
import { getApiBaseUrl } from '@/lib/api/base';

/**
 * The redaction policy, re-exported from the module generated out of
 * internal/config.
 *
 * Nothing here is written by hand any more. These four values used to be typed
 * out on this side under a comment saying they mirrored the Go ones, and the
 * mirror was one-way: widening the offered set turned three Go packages red and
 * left every web test green, with the wizard still presenting the old menu and a
 * test asserting the newly-offered level must stay hidden. A second hand-written
 * copy that currently agrees is the same defect with a fresh timestamp.
 *
 * They are re-exported rather than imported directly by each consumer so the
 * generated file stays an implementation detail of this module.
 */
import {
  ALL_REDACTION_LEVELS,
  SELECTABLE_REDACTION_LEVELS,
  DEFAULT_REDACTION_LEVEL,
  UNSELECTABLE_REDACTION_LEVEL_REASONS,
  REDACTION_SCOPE_SENTENCE,
} from '@/lib/share/redaction-policy.generated';
import type {
  RedactionLevel,
  SelectableRedactionLevel,
} from '@/lib/share/redaction-policy.generated';

export {
  ALL_REDACTION_LEVELS,
  SELECTABLE_REDACTION_LEVELS,
  DEFAULT_REDACTION_LEVEL,
  UNSELECTABLE_REDACTION_LEVEL_REASONS,
  REDACTION_SCOPE_SENTENCE,
};
export type { RedactionLevel, SelectableRedactionLevel };

/** Whether a level is one this version offers. */
export function isSelectableRedactionLevel(
  level: string,
): level is SelectableRedactionLevel {
  return (SELECTABLE_REDACTION_LEVELS as readonly string[]).includes(level);
}

/**
 * Why a level cannot be chosen, in the project's what/why/where/means/fix shape.
 *
 * The `why` line is the GENERATED sentence, which is the same one the CLI and the
 * local API give for that level - it used to be a third phrasing of the same
 * fact, pinned by its own tests, so all three could be improved separately and
 * nothing compared them. What stays here is the wizard-specific frame: where the
 * refusal happened, what it means for the review below, and what to do, phrased
 * for a user rather than an operator because this text is rendered in the step.
 *
 * The redaction-scope hedge in the `fix:` line is generated for the same reason,
 * and it had already drifted before it was: it read "shared as recorded" here
 * against "published as recorded" in Go, while config.RedactionScopeSentence
 * claimed the hedge lived in exactly one place and named this file as a consumer.
 */
export function unselectableRedactionLevelReason(level: string): string {
  const why =
    UNSELECTABLE_REDACTION_LEVEL_REASONS[level] ??
    `the ${level} redaction level is not one this version has any defined behaviour for`;
  return [
    `what: the ${level} redaction level is not one this version offers`,
    `why: ${why}`,
    'where: the redaction level selector on this step',
    'when: before any scan was started, so nothing was classified and nothing was shared',
    `means: the level stayed at ${DEFAULT_REDACTION_LEVEL}; your review below is the one this version will actually apply`,
    `fix: continue at ${SELECTABLE_REDACTION_LEVELS.join(', ')}. ${REDACTION_SCOPE_SENTENCE}`,
  ].join('\n');
}

interface WireItem {
  category: unknown;
  ruleId: string;
  ruleDisplayName: string;
  originalText: string;
  redactedReplacement: string;
  description: string;
  lineNumber: number;
  contextBefore: string[];
  contextAfter: string[];
}
interface WireRule {
  ruleId: string;
  displayName: string;
  count: number;
  items: WireItem[];
}
interface WireCategory {
  category: unknown;
  totalCount: number;
  rules: WireRule[];
}
interface WireResponse {
  total: number;
  categories: WireCategory[];
}

const VALID_CATEGORIES: ReadonlySet<string> = new Set([
  'CREDENTIAL',
  'PII',
  'PATH',
  'INTERNAL',
]);

function redactionCategory(value: unknown): RedactionCategory {
  if (typeof value === 'string' && VALID_CATEGORIES.has(value)) {
    return value as RedactionCategory;
  }
  throw new Error([
    `what: redaction preview category ${JSON.stringify(value)} is not recognized`,
    'why: the local API and UI must use the same canonical redaction category rendering',
    'where: category field in the GET /api/v1/sync/redactions response',
    'when: validating the local scan response before rendering redaction findings',
    'means: the preview cannot safely classify this finding, so sharing remains blocked',
    'fix: update the producer and consumer to use CREDENTIAL, PII, PATH, or INTERNAL, then re-scan',
  ].join('\n'));
}

function requireMatchingCategory(
  groupCategory: RedactionCategory,
  itemCategory: RedactionCategory,
): void {
  if (itemCategory === groupCategory) return;
  throw new Error([
    `what: redaction preview item category ${JSON.stringify(itemCategory)} does not match enclosing category ${JSON.stringify(groupCategory)}`,
    'why: every redaction item must use the same canonical category as its enclosing group',
    'where: category fields in the GET /api/v1/sync/redactions response',
    'when: validating the local scan response before rendering redaction findings',
    'means: the preview cannot safely classify this finding, so sharing remains blocked',
    'fix: correct the response producer so each item category matches its enclosing group, then re-scan',
  ].join('\n'));
}

/**
 * Fetch + flatten the real redaction scan for one session at the given level.
 * The level filtering is server-side (the redactor only detects what that level
 * strips), so the client does not re-filter by category.
 */
export async function fetchRedactionPreview(
  sessionId: string,
  level: RedactionLevel,
): Promise<Redaction[]> {
  const params = new URLSearchParams({ session_id: sessionId, level });
  const resp = await fetch(`${getApiBaseUrl()}/api/v1/sync/redactions?${params.toString()}`);
  if (!resp.ok) {
    const body = await resp.text().catch(() => '');
    throw new Error(`redaction scan failed (${resp.status}): ${body}`);
  }
  const data = (await resp.json()) as WireResponse;
  const out: Redaction[] = [];
  for (const cat of data.categories ?? []) {
    const category = redactionCategory(cat.category);
    for (const rule of cat.rules ?? []) {
      for (const it of rule.items ?? []) {
        const itemCategory = redactionCategory(it.category);
        requireMatchingCategory(category, itemCategory);
        out.push({
          id: `${it.category}:${it.ruleId}:${it.lineNumber}:${it.originalText}`,
          category,
          // The rule-based detector doesn't emit a per-match score; detected
          // matches are high-confidence, so they show without a low-confidence
          // warning. (Confidence scoring is a future server-side addition.)
          confidence: 100,
          lineNumber: it.lineNumber,
          contextBefore: it.contextBefore ?? [],
          contextAfter: it.contextAfter ?? [],
          originalText: it.originalText,
          redactedReplacement: it.redactedReplacement,
          description: it.description || it.ruleDisplayName,
          status: 'pending',
        });
      }
    }
  }
  return out;
}
