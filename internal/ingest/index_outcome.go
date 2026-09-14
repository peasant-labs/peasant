package ingest

// IndexOutcomeCompleted reports whether an outcome means the session came out
// of the INDEX stage with its entries.
//
// Only indexed and reindexed complete indexing. Fallback is an EXTRACT+WRITE
// routing diagnostic emitted before indexing (see reindexFallbackLog): the
// original source was missing, so the run fell back to the existing saved
// transcript without refreshing metadata. It records that routing decision,
// not that entries were written, so it neither counts as a completion nor
// clears an earlier error. Skipped ends without entries and without an error,
// so it is neither a completion nor a failure.
func IndexOutcomeCompleted(outcome IndexOutcome) bool {
	switch outcome {
	case IndexOutcomeIndexed, IndexOutcomeReindexed:
		return true
	}
	return false
}

// FailedIndexSessions returns the distinct sessions whose index attempt ended
// in error with no completion, in first-seen order.
//
// One session can produce several log rows in one run: the drain loop and the
// stale-index sweep each record an attempt. Counting rows over-reports, so
// callers must count sessions. A session that fails one attempt and completes
// another is not a failure at all, regardless of row order: the sweep can
// resolve what the drain loop lost, and the log order reflects scheduling
// rather than precedence.
func FailedIndexSessions(log []IndexLogEntry) []SessionID {
	var failed []SessionID
	failedSeen := map[SessionID]bool{}
	completed := map[SessionID]bool{}
	for _, entry := range log {
		switch {
		case entry.Outcome == IndexOutcomeError:
			if !failedSeen[entry.SessionID] {
				failedSeen[entry.SessionID] = true
				failed = append(failed, entry.SessionID)
			}
		case IndexOutcomeCompleted(entry.Outcome):
			completed[entry.SessionID] = true
		}
	}
	kept := failed[:0]
	for _, session := range failed {
		if !completed[session] {
			kept = append(kept, session)
		}
	}
	return kept
}
