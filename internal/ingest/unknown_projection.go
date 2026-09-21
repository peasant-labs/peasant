package ingest

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/peasant-labs/schema"
)

// ProjectRetainedUnknown validates stored evidence and copies the retained JSON
// text verbatim. Capture owns coordinates: entry indices, line numbers and native
// IDs cannot reconstruct traversal positions after known blocks have been folded.
func ProjectRetainedUnknown(entries []schema.SessionEntry, harness Harness) ([]schema.RetainedUnknownRecord, error) {
	var projected []schema.RetainedUnknownRecord
	for _, entry := range entries {
		records, err := RetainedUnknownOf(entry)
		if err != nil {
			return nil, err
		}
		for _, record := range records {
			if (harness != "" && record.Harness != harness) || (entry.Harness != "" && record.Harness != entry.Harness) {
				return nil, fmt.Errorf("project retained unknown evidence: stored harness disagrees with its owner; no detail was emitted; re-index the original source")
			}
			position := record.Position.Public
			if position == nil {
				return nil, fmt.Errorf("project retained unknown evidence: stored capture lacks complete source traversal coordinates; line numbers and native IDs cannot reconstruct block positions; no detail was emitted; re-index with a position-aware adapter")
			}
			projected = append(projected, schema.RetainedUnknownRecord{
				SourceRef: position.SourceRef, RecordIndex: position.RecordIndex, Position: position.Position,
				Pointer: record.Position.JSONPointer, Namespace: record.Namespace, Kind: record.Kind, Payload: string(record.Payload),
			})
		}
	}
	if len(projected) != 0 {
		// Entry folding and partition layout are not source traversal order.
		// Sort by captured coordinates, never payload identity; duplicates remain
		// present for schema validation to reject rather than silently deduping.
		slices.SortStableFunc(projected, func(a, b schema.RetainedUnknownRecord) int {
			if source := cmp.Compare(a.SourceRef, b.SourceRef); source != 0 {
				return source
			}
			return cmp.Compare(a.Position, b.Position)
		})
		if err := schema.ValidateRetainedUnknown(schema.SessionDetailPayload{RetainedUnknown: projected, Diagnostics: &schema.InterpretationDiagnostics{Partial: true}}); err != nil {
			return nil, fmt.Errorf("project retained unknown evidence: stored payload or source ordering is invalid: %w; nothing was emitted; re-index the intact source", err)
		}
	}
	return projected, nil
}
