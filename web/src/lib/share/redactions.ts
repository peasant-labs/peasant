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
import { parseRedactionCategoryLabel, redactionCategoryFromLabel, zSyncRedactionsResponse, type SyncRedactionsResponse } from '@peasant-labs/schema';

/**
 * The redaction policy, re-exported from the module generated out of
 * internal/config.
 *
 * Nothing here is written by hand any more. These policy values used to be typed
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
  SELECTABLE_REDACTION_LEVELS,
  DEFAULT_REDACTION_LEVEL,
} from '@/lib/share/redaction-policy.generated';
import type {
  RedactionLevel,
  SelectableRedactionLevel,
} from '@/lib/share/redaction-policy.generated';

export {
  SELECTABLE_REDACTION_LEVELS,
  DEFAULT_REDACTION_LEVEL,
};
export type { RedactionLevel, SelectableRedactionLevel };

/** Whether a level is one this version offers. */
export function isSelectableRedactionLevel(
  level: string,
): level is SelectableRedactionLevel {
  return (SELECTABLE_REDACTION_LEVELS as readonly string[]).includes(level);
}

function redactionCategory(value: unknown): RedactionCategory {
  try {
    const label = parseRedactionCategoryLabel(value);
    redactionCategoryFromLabel(label);
    return label;
  } catch {
  throw new Error([
    `what: redaction preview category ${JSON.stringify(value)} is not recognized`,
    'why: the local API and UI must use the same canonical redaction category rendering',
    'where: category field in the GET /api/v1/sync/redactions response',
    'when: validating the local scan response before rendering redaction findings',
    'means: the preview cannot safely classify this finding, so sharing remains blocked',
    'fix: update the producer and consumer to use CREDENTIAL, PII, PATH, or INTERNAL, then re-scan',
  ].join('\n'));
  }
}

function requireCount(valid: boolean): void {
  if (!valid) throw new Error('redaction scan returned invalid occurrence counts while validating the local preview; sharing remains blocked. Correct the scan producer and re-scan.');
}

/** Validate mock and cached domain results before either review owner sees success. */
export function validateRedactionPreview(preview: RedactionPreview): RedactionPreview {
  if (!preview || !Array.isArray(preview.redactions)) throw new Error('redaction preview is missing its findings while preparing review; sharing remains blocked. Re-scan using an updated producer.');
  requireCount(Number.isSafeInteger(preview.matchCount) && preview.matchCount >= preview.redactions.length
    && (preview.matchCount === 0) === (preview.redactions.length === 0));
  for (const item of preview.redactions) {
    if (!item || typeof item !== 'object') throw new Error('redaction preview has malformed findings while preparing review; sharing remains blocked. Correct the preview producer and re-scan.');
    redactionCategory(item.category);
    if (typeof item.id !== 'string' || typeof item.originalText !== 'string' || typeof item.redactedReplacement !== 'string'
      || !Number.isFinite(item.confidence) || item.confidence < 0 || item.confidence > 100
      || !Number.isSafeInteger(item.lineNumber) || item.lineNumber < 0
      || !Array.isArray(item.contextBefore) || !item.contextBefore.every((line) => typeof line === 'string')
      || !Array.isArray(item.contextAfter) || !item.contextAfter.every((line) => typeof line === 'string')) {
      throw new Error('redaction preview has malformed findings while preparing review; sharing remains blocked. Correct the preview producer and re-scan.');
    }
  }
  return preview;
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
export interface RedactionPreview {
  redactions: Redaction[];
  /** All occurrences, including matches omitted from the capped preview. */
  matchCount: number;
}

export async function fetchRedactionScan(
  sessionId: string,
  level: RedactionLevel,
): Promise<RedactionPreview> {
  const params = new URLSearchParams({ session_id: sessionId, level });
  const resp = await fetch(`${getApiBaseUrl()}/api/v1/sync/redactions?${params.toString()}`);
  if (!resp.ok) {
    const body = await resp.text().catch(() => '');
    throw new Error(`redaction scan failed (${resp.status}): ${body}`);
  }
  const parsed = zSyncRedactionsResponse.safeParse(await resp.json());
  if (!parsed.success) throw new Error('redaction scan returned a malformed response while reading GET /api/v1/sync/redactions; sharing remains blocked. Update the local scan producer and re-scan.');
  const data: SyncRedactionsResponse = parsed.data;
  const out: Redaction[] = [];
  let total = 0;
  for (const cat of data.categories) {
    const category = redactionCategory(cat.category);
    let categoryTotal = 0;
    for (const rule of cat.rules) {
      requireCount(Number.isSafeInteger(rule.count) && rule.count >= rule.items.length && (rule.count === 0) === (rule.items.length === 0));
      categoryTotal += rule.count;
      for (const it of rule.items) {
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
    requireCount(Number.isSafeInteger(categoryTotal) && categoryTotal === cat.totalCount);
    total += categoryTotal;
  }
  // The producer counts raw occurrences in total, but deduplicates each rule
  // by matched text before grouping and capping preview items.
  requireCount(Number.isSafeInteger(data.total) && Number.isSafeInteger(total) && total <= data.total);
  return validateRedactionPreview({ redactions: out, matchCount: data.total });
}

/** The sample-only adapter used by existing review consumers. */
export async function fetchRedactionPreview(sessionId: string, level: RedactionLevel): Promise<Redaction[]> {
  return (await fetchRedactionScan(sessionId, level)).redactions;
}
