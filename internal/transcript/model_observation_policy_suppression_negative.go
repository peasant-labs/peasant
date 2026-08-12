//go:build evidence_negative_suppression

package transcript

func shouldSuppressEmptyTurn(hasContent, hasTools, _ bool) bool {
	return !hasContent && !hasTools
}

func observationsEquivalent(previous, current entryModelObservation) bool {
	return previous.present == current.present && previous.value == current.value
}
