//go:build !evidence_negative_suppression && !evidence_negative_dedup

package transcript

func shouldSuppressEmptyTurn(hasContent, hasTools, hasObservation bool) bool {
	return !hasContent && !hasTools && !hasObservation
}

func observationsEquivalent(previous, current entryModelObservation) bool {
	return previous.present == current.present && previous.value == current.value
}
