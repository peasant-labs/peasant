/**
 * Minimal REST client for entry-level (per-turn) annotation labeling in the
 * live v2 transcript viewer.
 *
 * Three calls only:
 *   - fetchAnnotationTypes()    GET  /api/v1/annotation-types
 *   - fetchSessionAnnotations() GET  /api/v1/annotations?session_id=X
 *   - batchCreateAnnotations()  POST /api/v1/annotations/batch
 *
 * The single-create endpoint (POST /api/v1/annotations) does NOT support entry
 * targeting, so saving a turn label always goes through the batch endpoint with
 * a single-item array.
 */

import { getApiBaseUrl } from './base';
import {
  AnnotationStatus,
  TargetKind,
  ValueDomainKind,
  type AnnotationStatus as AnnotationStatusValue,
  type AnnotationSummary,
  type AnnotationTypeSummary,
  type BatchCreateAnnotationsResponse,
  type CreateAnnotationRequest,
  type ValueDomain,
} from '@peasant-labs/schema';

export type {
  AnnotationSummary,
  BatchCreateAnnotationsResponse,
  CreateAnnotationRequest,
  ValueDomain,
  ValueDomainKind,
} from '@peasant-labs/schema';

// ---------------------------------------------------------------------------
// Generated wire types (AnnotationTypeSummary / CreateAnnotationRequest)
// ---------------------------------------------------------------------------

/**
 * Generated wire format for an annotation type from GET /api/v1/annotation-types.
 * `allowedTargetKinds` is the V16 junction projection — the set of target
 * kinds this type may annotate. Omitted/empty means "no restriction".
 */
export type AnnotationType = AnnotationTypeSummary;

/**
 * POST /api/v1/annotations/batch request item — mirrors Go
 * schema.CreateAnnotationRequest. For an entry (turn) target, set both
 * `sessionId` and `targetEntryIndex`; `targetEntryEndIndex` defaults to
 * index+1 server-side but we send it explicitly (half-open [start, end)).
 */
// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

/** Target kind that marks a type as applicable to a single transcript entry. */
export const TARGET_KIND_ENTRY = TargetKind.Entry;

/** Annotator name for human annotations created via the web dashboard. */
export const ANNOTATOR_NAME_HUMAN_WEB = 'human-web' as const;

/**
 * Entry-level annotation type IDs backing the restored per-turn labeling
 * modal: a human outcome verdict (good/neutral/bad) and an
 * optional friction flag. Seeded by migration V39 (internal/store/schema_v39.go).
 */
export const TURN_OUTCOME_TYPE_ID = 'quality.turn_outcome' as const;
export const TURN_FLAG_TYPE_ID = 'quality.turn_flag' as const;

// ---------------------------------------------------------------------------
// Pure helpers (testable without a network)
// ---------------------------------------------------------------------------

/**
 * Filter annotation types to those a user may apply to a single turn.
 *
 * A type is entry-applicable when its `allowedTargetKinds` includes "entry".
 * Types with no `allowedTargetKinds` are treated as unrestricted-by-registry
 * but are NOT offered for entry targeting here: the system's entry-applicable
 * types (e.g. quality.frustration_signal, quality.resolution_evidence) all
 * carry an explicit "entry" kind, and offering session-only types on a turn
 * is exactly the correctness bug the old UI had.
 */
export function entryApplicableTypes(types: AnnotationType[]): AnnotationType[] {
  return types.filter((t) =>
    (t.allowedTargetKinds ?? []).includes(TARGET_KIND_ENTRY),
  );
}

/** Enumerated permissible values for a type, or [] for described/free-text. */
export function permissibleValues(type: AnnotationType): string[] {
  if (type.valueDomain.kind !== ValueDomainKind.Enumerated) return [];
  return type.valueDomain.permissibleValues ?? [];
}

// ---------------------------------------------------------------------------
// API client
/** GET /api/v1/annotation-types. Optionally filter by status (e.g. "active"). */
export async function fetchAnnotationTypes(
  opts: { status?: AnnotationStatusValue } = { status: AnnotationStatus.Active },
): Promise<AnnotationType[]> {
  const params = new URLSearchParams();
  if (opts.status) params.set('status', opts.status);
  const qs = params.toString();
  const url = `${getApiBaseUrl()}/api/v1/annotation-types${qs ? `?${qs}` : ''}`;

  const resp = await fetch(url);
  if (!resp.ok) {
    const body = await resp.text().catch(() => '');
    throw new Error(`GET /api/v1/annotation-types failed (${resp.status}): ${body}`);
  }
  return (await resp.json()) as AnnotationType[];
}

/** GET /api/v1/annotations?session_id=X — all non-superseded annotations. */
export async function fetchSessionAnnotations(
  sessionId: string,
): Promise<AnnotationSummary[]> {
  const url = `${getApiBaseUrl()}/api/v1/annotations?session_id=${encodeURIComponent(sessionId)}`;
  const resp = await fetch(url);
  if (!resp.ok) {
    const body = await resp.text().catch(() => '');
    throw new Error(`GET /api/v1/annotations failed (${resp.status}): ${body}`);
  }
  // Handler encodes nil as `null` rather than `[]`; normalize.
  return ((await resp.json()) as AnnotationSummary[] | null) ?? [];
}

/**
 * POST /api/v1/annotations/batch. Commits all items atomically; returns the
 * created annotation IDs in request order. Throws on validation/HTTP error.
 */
export async function batchCreateAnnotations(
  annotations: CreateAnnotationRequest[],
): Promise<BatchCreateAnnotationsResponse> {
  const resp = await fetch(`${getApiBaseUrl()}/api/v1/annotations/batch`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ annotations }),
  });
  if (!resp.ok) {
    const body = await resp.text().catch(() => '');
    throw new Error(`POST /api/v1/annotations/batch failed (${resp.status}): ${body}`);
  }
  return (await resp.json()) as BatchCreateAnnotationsResponse;
}

/**
 * Save a single entry-level label on a turn. Convenience wrapper that builds
 * the single-item batch with entry targeting (`targetEntryIndex` = entryIndex,
 * `targetEntryEndIndex` = entryIndex + 1) and the web annotator.
 */
export async function saveTurnLabel(args: {
  sessionId: string;
  entryIndex: number;
  typeId: string;
  value: string;
}): Promise<string> {
  const req: CreateAnnotationRequest = {
    sessionId: args.sessionId,
    typeId: args.typeId,
    value: args.value,
    isPrimary: false,
    annotatorName: ANNOTATOR_NAME_HUMAN_WEB,
    targetEntryIndex: args.entryIndex,
    targetEntryEndIndex: args.entryIndex + 1,
  };
  const res = await batchCreateAnnotations([req]);
  const id = res.ids?.[0];
  if (!id) {
    throw new Error('The annotation batch succeeded without returning its created ID because the server response omitted ids[0] in saveTurnLabel after the POST completed. The label cannot be reconciled with later updates; retry after updating the Peasant server to return one ID per created annotation.');
  }
  return id;
}

/**
 * Save one or more entry-level labels on a turn in a single atomic batch —
 * used by the outcome+flag labeling modal, which always writes both the
 * `quality.turn_outcome` and `quality.turn_flag` values together. Convenience
 * wrapper over `batchCreateAnnotations` with entry targeting
 * (`targetEntryIndex` = entryIndex, `targetEntryEndIndex` = entryIndex + 1)
 * and the web annotator, mirroring `saveTurnLabel`'s single-item shape.
 *
 * Returns one created annotation ID per input item, in the same order.
 */
export async function saveTurnLabels(args: {
  sessionId: string;
  entryIndex: number;
  items: Array<{ typeId: string; value: string }>;
}): Promise<string[]> {
  const reqs: CreateAnnotationRequest[] = args.items.map((item) => ({
    sessionId: args.sessionId,
    typeId: item.typeId,
    value: item.value,
    isPrimary: false,
    annotatorName: ANNOTATOR_NAME_HUMAN_WEB,
    targetEntryIndex: args.entryIndex,
    targetEntryEndIndex: args.entryIndex + 1,
  }));
  const res = await batchCreateAnnotations(reqs);
  const ids = res.ids ?? [];
  if (ids.length !== reqs.length) {
    throw new Error(
      `The annotation batch for entry ${args.entryIndex} returned ${ids.length} id(s) but ${reqs.length} item(s) were submitted in saveTurnLabels after the POST /api/v1/annotations/batch completed. The saved labels cannot be reliably reconciled with their created IDs; retry after confirming the Peasant server returns one ID per submitted annotation, in request order.`,
    );
  }
  return ids;
}
