package metrics

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// Annotator names — must match seed data in internal/store/schema.go seedAnnotators().
const (
	annotatorNameOutcome     = "outcome-classifier"
	annotatorNameFrustration = "frustration-classifier"
	annotatorNameScope       = "scope-classifier"

	// Entry-level classifier annotator names (V18 migration).
	annotatorNameFrustrationSignal  = "frustration-signal-classifier"
	annotatorNameResolutionEvidence = "resolution-evidence-classifier"
)

// typeAnnotatorNames maps each classifier's TypeID to its seeded annotator name.
// These names match the rows seeded by the V13/V18 migrations in store/schema.go.
var typeAnnotatorNames = map[string]string{
	typeIDSessionOutcome:     annotatorNameOutcome,
	typeIDUserFrustration:    annotatorNameFrustration,
	typeIDSessionScope:       annotatorNameScope,
	typeIDFrustrationSignal:  annotatorNameFrustrationSignal,
	typeIDResolutionEvidence: annotatorNameResolutionEvidence,
}

// ClassifierAnnotator implements ingest.SessionClassifier.
// It runs the ClassifierEngine over a session's entries and metrics, then
// persists non-nil results as session annotations via AnnotationStore.
//
// Annotate is best-effort: per-result errors are logged and skipped.
type ClassifierAnnotator struct {
	metricsStore    ingest.MetricsStore
	annotationStore ingest.AnnotationStore
	engine          *ClassifierEngine
}

// Compile-time guard: *ClassifierAnnotator must satisfy ingest.SessionClassifier.
var _ ingest.SessionClassifier = (*ClassifierAnnotator)(nil)

// NewClassifierAnnotator constructs a ClassifierAnnotator with the given stores.
func NewClassifierAnnotator(
	ms ingest.MetricsStore,
	as ingest.AnnotationStore,
) *ClassifierAnnotator {
	return &ClassifierAnnotator{
		metricsStore:    ms,
		annotationStore: as,
		engine:          NewClassifierEngine(),
	}
}

// Annotate runs all classifiers for sessionID and persists non-nil results.
// Entry/metrics load errors are returned; per-result errors are logged and skipped.
func (ca *ClassifierAnnotator) Annotate(ctx context.Context, sessionID ingest.SessionID) error {
	entries, err := ca.metricsStore.ListEntries(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("ClassifierAnnotator.Annotate: list entries for %s: %w", sessionID, err)
	}
	metrics, err := ca.metricsStore.GetMetrics(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("ClassifierAnnotator.Annotate: get metrics for %s: %w", sessionID, err)
	}

	results := ca.engine.Run(ctx, sessionID, entries, metrics)
	for _, r := range results {
		if err := ca.persistResult(ctx, sessionID, r); err != nil {
			slog.Warn("ClassifierAnnotator.Annotate: persist result",
				"session_id", sessionID,
				"type_id", r.TypeID,
				"error", err,
			)
		}
	}
	return nil
}

// persistResult resolves annotator/type IDs and writes one ClassifierResult to the store.
//
// Dedup logic (R9):
//  1. Compute content hash from the annotation fields.
//  2. Query for an existing non-superseded annotation with the same
//     (annotation_type_id, annotator_id, target) triple.
//  3. If found with same content_hash → skip (no-op).
//  4. If found with different content_hash → create new, supersede old.
//  5. If not found → create new.
//
// Dispatch: Target==nil → CreateSessionAnnotation, Target!=nil → CreateEntryAnnotation.
func (ca *ClassifierAnnotator) persistResult(
	ctx context.Context,
	sessionID ingest.SessionID,
	r *ClassifierResult,
) error {
	annotatorName, ok := typeAnnotatorNames[r.TypeID]
	if !ok {
		return fmt.Errorf("no seeded annotator for type %q", r.TypeID)
	}

	annotatorID, err := ca.annotationStore.GetAnnotatorIDByName(ctx, annotatorName)
	if err != nil {
		return fmt.Errorf("get annotator %q: %w", annotatorName, err)
	}
	if annotatorID == "" {
		return fmt.Errorf("annotator %q not found in DB (V13 migration required)", annotatorName)
	}

	annotationTypeID, err := ca.annotationStore.GetAnnotationTypeID(ctx, r.TypeID)
	if err != nil {
		return fmt.Errorf("get annotation type %q: %w", r.TypeID, err)
	}
	if annotationTypeID == "" {
		return fmt.Errorf("annotation type %q not found in DB (V13 migration required)", r.TypeID)
	}

	var confidence *float64
	if r.Confidence > 0 {
		confidence = &r.Confidence
	}
	var reason *string
	if r.Reason != "" {
		reason = &r.Reason
	}

	// Build FindAnnotationParams and compute content hash for dedup.
	sid := string(sessionID)
	findParams := ingest.FindAnnotationParams{
		AnnotationTypeID: annotationTypeID,
		AnnotatorID:      annotatorID,
		SessionID:        &sid,
	}

	var entryIndex *int
	var endIndex *int
	if r.Target != nil {
		findParams.EntryIndex = &r.Target.EntryIndex
		entryIndex = &r.Target.EntryIndex
		if r.Target.EndIndex != 0 {
			endIndex = &r.Target.EndIndex
		}
	}

	newHash := schema.ComputeAnnotationHash(
		annotationTypeID, annotatorID, r.Value,
		&sid, entryIndex, endIndex,
		confidence, reason, r.Provenance,
	)

	// Dedup check: look for existing annotation with same logical identity.
	existing, err := ca.annotationStore.FindExistingAnnotation(ctx, findParams)
	if err != nil {
		// Non-fatal: log and proceed to create (worst case: duplicate).
		slog.Warn("ClassifierAnnotator.persistResult: dedup check failed, proceeding with create",
			"type_id", r.TypeID, "error", err)
	}

	dedupResult := ca.decideDedupAction(existing, newHash)

	switch dedupResult {
	case ingest.DedupSkip:
		slog.Debug("ClassifierAnnotator.persistResult: skip (same content hash)",
			"type_id", r.TypeID, "session_id", sessionID, "hash", newHash)
		return nil

	case ingest.DedupSupersede:
		// Create the new annotation first, then supersede the old one.
		newID, createErr := ca.createAnnotation(ctx, sessionID, r, annotatorID, annotationTypeID, confidence, reason)
		if createErr != nil {
			return createErr
		}
		// Persist the content hash on the new annotation.
		if hashErr := ca.annotationStore.UpdateContentHash(ctx, newID, newHash); hashErr != nil {
			slog.Warn("ClassifierAnnotator.persistResult: update content hash",
				"annotation_id", newID, "error", hashErr)
		}
		// Supersede the old annotation.
		if supErr := ca.annotationStore.SupersedeAnnotation(ctx, existing.ID, newID); supErr != nil {
			slog.Warn("ClassifierAnnotator.persistResult: supersede old annotation",
				"old_id", existing.ID, "new_id", newID, "error", supErr)
		}
		return nil

	default: // DedupCreate
		newID, createErr := ca.createAnnotation(ctx, sessionID, r, annotatorID, annotationTypeID, confidence, reason)
		if createErr != nil {
			return createErr
		}
		// Persist the content hash on the new annotation.
		if hashErr := ca.annotationStore.UpdateContentHash(ctx, newID, newHash); hashErr != nil {
			slog.Warn("ClassifierAnnotator.persistResult: update content hash",
				"annotation_id", newID, "error", hashErr)
		}
		return nil
	}
}

// decideDedupAction determines whether to skip, supersede, or create a new annotation
// based on the existing annotation and the new content hash.
func (ca *ClassifierAnnotator) decideDedupAction(
	existing *ingest.ExistingAnnotation,
	newHash string,
) ingest.AnnotationDedupResult {
	if existing == nil {
		return ingest.DedupCreate
	}
	if existing.ContentHash == newHash {
		return ingest.DedupSkip
	}
	return ingest.DedupSupersede
}

// createAnnotation dispatches to CreateSessionAnnotation or CreateEntryAnnotation
// based on the ClassifierResult's Target field. Returns the new annotation's UUID.
func (ca *ClassifierAnnotator) createAnnotation(
	ctx context.Context,
	sessionID ingest.SessionID,
	r *ClassifierResult,
	annotatorID string,
	annotationTypeID string,
	confidence *float64,
	reason *string,
) (string, error) {
	if r.Target != nil {
		ep := ingest.EntryAnnotationParams{
			SessionID:        string(sessionID),
			EntryIndex:       r.Target.EntryIndex,
			EndIndex:         r.Target.EndIndex,
			AnnotatorID:      annotatorID,
			AnnotationTypeID: annotationTypeID,
			Value:            r.Value,
			Confidence:       confidence,
			Reason:           reason,
			Provenance:       r.Provenance,
		}
		newID, err := ca.annotationStore.CreateEntryAnnotation(ctx, ep)
		if err != nil {
			return "", fmt.Errorf("create entry annotation type=%q value=%q entry=%d: %w",
				r.TypeID, r.Value, r.Target.EntryIndex, err)
		}
		return newID, nil
	}

	sp := ingest.SessionAnnotationParams{
		SessionID:        string(sessionID),
		AnnotatorID:      annotatorID,
		AnnotationTypeID: annotationTypeID,
		Value:            r.Value,
		Confidence:       confidence,
		Reason:           reason,
		Provenance:       r.Provenance,
	}
	newID, err := ca.annotationStore.CreateSessionAnnotation(ctx, sp)
	if err != nil {
		return "", fmt.Errorf("create annotation type=%q value=%q: %w", r.TypeID, r.Value, err)
	}
	return newID, nil
}
