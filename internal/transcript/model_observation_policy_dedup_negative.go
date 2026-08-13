//go:build evidence_negative_dedup

package transcript

func shouldSuppressEmptyTurn(hasContent, hasTools, hasObservation bool) bool {
	return !hasContent && !hasTools && !hasObservation
}

func observationsEquivalent(entryModelObservation, entryModelObservation) bool { return true }
