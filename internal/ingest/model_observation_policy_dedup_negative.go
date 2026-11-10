//go:build evidence_negative_dedup && !evidence_negative_suppression

package ingest

func shouldSuppressEmptyTurn(hasContent, hasTools, hasObservation bool) bool {
	return !hasContent && !hasTools && !hasObservation
}

func observationsEquivalent(entryModelObservation, entryModelObservation) bool { return true }
