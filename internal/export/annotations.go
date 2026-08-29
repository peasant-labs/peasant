package export

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/store"
)

// ExportAnnotations retrieves all non-superseded annotations for a session
// (both session-level and entry-level) and maps them to the export schema.
//
// Returns an empty slice (not nil) when no annotations exist for the session.
func ExportAnnotations(ctx context.Context, db *store.Store, sessionID string) ([]ExportedAnnotation, error) {
	unresolved, err := db.ListUnresolvedAnnotationTargetAnchors(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("ExportAnnotations: failed to check unresolved annotation targets for session %s: %w\nWhat went wrong: the export trust-boundary check could not read annotation_target_anchors.\nWhere: internal/export/annotations.go, ExportAnnotations.\nFix: verify the database is readable and re-run the export.", sessionID, err)
	}
	if len(unresolved) > 0 {
		first := unresolved[0]
		return nil, fmt.Errorf("ExportAnnotations: refused to export %d unresolved annotation target(s) for session %s\nWhat went wrong: annotation %s has no safely resolved transcript entry target.\nWhy it happened: re-index repair could not prove the old target maps to one unique current entry.\nWhere: annotation_target_anchors unresolved state before session annotation export.\nWhat it means: Peasant did not export annotations because doing so could attach a user label to the wrong transcript entry.\nFix: inspect the session with 'peasant annotate list %s', recreate or remove the affected annotation from annotator %q, then re-run the export.", len(unresolved), sessionID, first.AnnotationID, sessionID, first.AnnotatorName)
	}
	// Collect session-level annotations.
	rows, err := db.GetAnnotationsForSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf(
			"ExportAnnotations: failed to retrieve session annotations for session %s: %w\n"+
				"What went wrong: the store query for session annotations returned an error.\n"+
				"Where: internal/export/annotations.go, ExportAnnotations.\n"+
				"Fix: verify the session ID exists in the store and the database is accessible.",
			sessionID, err,
		)
	}
	// Collect entry-level annotations (per-turn labels, ingest-generated rule labels).
	entryRows, err := db.GetEntryAnnotationsForSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf(
			"ExportAnnotations: failed to retrieve entry annotations for session %s: %w\n"+
				"What went wrong: the store query for entry-level annotations returned an error.\n"+
				"Where: internal/export/annotations.go, ExportAnnotations.\n"+
				"Fix: verify the session ID exists in the store and the database is accessible.",
			sessionID, err,
		)
	}
	rows = append(rows, entryRows...)

	out := make([]ExportedAnnotation, 0, len(rows))
	for _, row := range rows {
		ea := ExportedAnnotation{
			SessionID:     sessionID,
			TypeID:        row.TypeID,
			Value:         row.Value,
			Annotator:     row.AnnotatorName,
			AnnotatorKind: row.AnnotatorKind.String(),
			Confidence:    row.Confidence,
			CreatedAt:     row.CreatedAt,
			StartEntry:    row.TargetEntryIndex,
			EndEntry:      row.TargetEntryEndIndex,
		}

		// Dereference optional *string Reason field.
		if row.Reason != nil {
			ea.Reason = *row.Reason
		}

		out = append(out, ea)
	}
	return out, nil
}
