import type { AnalyticsSessionRecord } from '@peasant-labs/fairtrade/analytics';
import type { QualitySession } from '@/lib/quality/types';

/**
 * Adapter contract for the analytics surface.
 *
 * Converts the quality WS channel's QualitySession[] payload into the
 * AnalyticsSessionRecord[] rows that <ProjectOverview> from
 * @peasant-labs/fairtrade/analytics consumes through its cooked payload prop:
 *   quality WS payload -> adaptAnalytics -> { sessions } -> <ProjectOverview payload>
 *
 * The analytics surface does not lift or fork a dashboard component. It
 * consumes the real <ProjectOverview> from the design system and keeps this
 * file as the only transformation point.
 *
 * All ProjectOverview sections stay enabled. The quality stream already carries
 * most fields the dashboard needs; contributor and commit linkage are not on
 * that stream yet, so those three fields are supplied by contributorFields().
 * That helper is deliberately isolated: replacing its fixed temporary values
 * with schema-backed contributor/commit fields should not require changing the
 * component or the rest of this adapter.
 *
 * Wire type sourced from: @peasant-labs/schema (QualitySession)
 * Output type: AnalyticsSessionRecord from @peasant-labs/fairtrade/analytics
 *   (the design system owns the record shape; only the fields the dashboard
 *   actually reads are part of that contract)
 *
 * Field mapping (QualitySession -> AnalyticsSessionRecord):
 *   PRESENT:    id              -> id
 *   TRANSFORM:  date            -> startTime
 *   TRANSFORM:  project         -> projectKey
 *   TEMPORARY:  contributorFields(qs) -> contributorId/hasCommit/commitCount
 *   TRANSFORM:  durationMinutes -> durationMins
 *   PRESENT:    totalTokens     -> totalTokens
 *   PRESENT:    turnCount       -> turnCount
 *   TRANSFORM:  toolCalls       -> toolCallCount
 *   PRESENT:    outcome         -> outcome
 *   INTENTIONALLY NOT MAPPED (not part of the dashboard's record contract):
 *     title, scope, inputTokens, outputTokens, retryLoops, explorationRatio,
 *     scopeBreadth, discoveryTurns, rulesOutcome, effectiveAnnotations,
 *     signalDensity, specQualityScore, retryTokensWasted, withinSessionReverts,
 *     filesTouched, linesChanged
 */

type ContributorFields = Pick<
  AnalyticsSessionRecord,
  'contributorId' | 'hasCommit' | 'commitCount'
>;

export const ANALYTICS_LOCAL_USER_ID = 'local user';
export const ANALYTICS_PLACEHOLDER_COMMIT_COUNT = 0;

/**
 * Temporary contributor/commit projection.
 *
 * The quality stream carries no contributor identity yet, and peasant is a
 * single-operator local tool — every session is attributed to "local user"
 * (an honest description, not a fabricated name) until the stream exposes
 * real identity. Commit count is explicit and fixed at zero until the
 * stream exposes real linkage.
 */
export function contributorFields(_session: QualitySession): ContributorFields {
  return {
    contributorId: ANALYTICS_LOCAL_USER_ID,
    hasCommit: ANALYTICS_PLACEHOLDER_COMMIT_COUNT > 0,
    commitCount: ANALYTICS_PLACEHOLDER_COMMIT_COUNT,
  };
}

/** Map quality WS sessions to AnalyticsSessionRecord[] for the payload prop. */
export function adaptAnalytics(
  wire: QualitySession[] = [],
): AnalyticsSessionRecord[] {
  return wire.map((session) => ({
    id: session.id,
    startTime: startTimeFromQualityDate(session.date),
    projectKey: session.project,
    ...contributorFields(session),
    durationMins: session.durationMinutes,
    totalTokens: session.totalTokens,
    turnCount: session.turnCount,
    toolCallCount: session.toolCalls,
    outcome: session.outcome,
  }));
}

function startTimeFromQualityDate(date: string): string {
  const value = date.trim();
  if (value.includes('T')) return value;
  return `${value}T00:00:00Z`;
}
