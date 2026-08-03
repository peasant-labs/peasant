import {
  AnnotatorKind,
  SessionOutcome,
  isSessionOutcome,
  type AnnotationSummary,
  type QualitySession as SchemaQualitySession,
} from '@peasant-labs/schema';

export type { SessionOutcome } from '@peasant-labs/schema';

/** Legacy annotation value that is intentionally distinct from SessionOutcome. */
const NOT_RESOLVED = 'not_resolved' as const;

/** Binary label used everywhere in the UI: positive (green) or negative (red). */
export type SessionLabel = "positive" | "negative";

export type QualitySession = Omit<
  SchemaQualitySession,
  'outcome' | 'effectiveAnnotations'
> & {
  // Optional, not SessionOutcome: a session whose quality metrics have not
  // been computed yet (or were computed before an outcome could be inferred)
  // legitimately carries an EMPTY wire outcome — real local stores have
  // thousands of these. That is a normal "not yet determined" state, not a
  // decode failure, and matches the same empty-means-uncomputed contract
  // internal/store already documents for SessionSummary.outcome (see
  // web/src/lib/share/types.ts). undefined here flows straight through
  // adaptAnalytics into @peasant-labs/fairtrade/analytics's own
  // AnalyticsSessionRecord.outcome (also optional there), which already
  // buckets an absent outcome under "unknown" in its distribution — the
  // downstream design system was already built to handle this; only this
  // adapter was rejecting it.
  outcome?: SessionOutcome;
  // Label annotations (v4) — raw annotation objects from the backend.
  // Derive humanLabel / agentLabel via deriveLabels() rather than reading fields directly.
  rulesOutcome?: SessionOutcome;
  effectiveAnnotations?: AnnotationSummary[];
};

/**
 * Validate the generated wire's open string before analytics consumes it.
 *
 * An EMPTY outcome ("") means "not yet computed" — a normal, expected state
 * for real sessions (quality metrics run asynchronously and don't always
 * reach a resolution verdict) — and is passed through as `undefined`, not an
 * error. A NON-EMPTY but unrecognized value (e.g. a future schema adding an
 * outcome this build doesn't know about yet) is still treated as a genuine
 * decode failure and fails closed: analytics must not silently misrepresent
 * an outcome value it can't classify.
 */
export function adaptQualitySessions(
  sessions: readonly SchemaQualitySession[],
): QualitySession[] {
  return sessions.map((session) => {
    if (session.outcome === '') {
      return { ...session, outcome: undefined };
    }
    if (!isSessionOutcome(session.outcome)) {
      throw new Error(
        `Quality data could not be analyzed because session ${JSON.stringify(session.id)} has unknown outcome ${JSON.stringify(session.outcome)} in adaptQualitySessions while decoding the quality channel; analytics have stopped to avoid presenting a false result. Regenerate @peasant-labs/schema and update the quality adapter for the new outcome.`,
      );
    }
    return { ...session, outcome: session.outcome };
  });
}

/** Map an annotation outcome value string to a binary SessionLabel. */
export function outcomeValueToLabel(value: string): SessionLabel | undefined {
  if (value === SessionOutcome.Resolved) return "positive";
  if (value === NOT_RESOLVED) return "negative";
  return undefined;
}

export interface DerivedLabels {
  humanLabel?: SessionLabel;
  agentLabel?: SessionLabel;
  rulesLabel?: SessionLabel;
}

/**
 * Derive humanLabel, agentLabel, and rulesLabel from a session's effectiveAnnotations array.
 *
 * Only annotations with typeId === "quality.session_outcome" are considered.
 * The first matching annotation per annotatorKind wins.
 * Returns an empty object when there are no relevant annotations.
 */
export function deriveLabels(annotations?: AnnotationSummary[]): DerivedLabels {
  if (!annotations?.length) return {};

  const result: DerivedLabels = {};

  for (const ann of annotations) {
    if (ann.typeId !== "quality.session_outcome") continue;
    const label = outcomeValueToLabel(ann.value);
    if (!label) continue;

    if (ann.annotatorKind === AnnotatorKind.Human && !result.humanLabel) {
      result.humanLabel = label;
    } else if (ann.annotatorKind === AnnotatorKind.Agent && !result.agentLabel) {
      result.agentLabel = label;
    } else if (ann.annotatorKind === AnnotatorKind.Rule && !result.rulesLabel) {
      result.rulesLabel = label;
    }
  }

  return result;
}
