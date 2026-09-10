package store

import "github.com/peasant-labs/peasant/internal/ingest"

// PublishableWithOmissions is THE rule for whether a stored content capture may
// be read whole, exported and published.
//
// A complete capture always may. So may a capture that is incomplete for exactly
// ONE reason: oversized source records were omitted at ingest. That capture holds
// every entry the source had, with a placeholder standing in each omitted
// record's place, and the session's own metadata diagnostics say so; refusing to
// publish it would hide a session for a record its owner can already see is
// missing. Every other incomplete or failed capture is refused, unchanged:
//   - a strict-parser refusal, whose entries are the tolerant projection rather
//     than the source's own,
//   - a legacy preview-only capture, which no build has ever certified,
//   - a failed capture, which proves nothing about what is stored,
//   - and an incomplete capture whose stored format is a bounded preview rather
//     than the full text, whatever its code says.
//
// It is one predicate on one typed value so the readiness check, the
// complete-content readers and the detail loader cannot drift apart. Callers pass
// the capture the store read; nothing here reads the database.
func PublishableWithOmissions(capture ingest.SessionContentCapture) bool {
	return publishableCaptureState(capture.Status, capture.FailureCode, capture.CaptureFormat)
}

// FullCaptureWritable is the same rule on the way IN: which capture states the
// full-content writer may certify. A full capture is written for a complete
// session, and for the one incompleteness that still holds every entry — the
// omitted-record case, whose placeholders are entries like any other. Anything
// else must be stored as the bounded preview it is.
func FullCaptureWritable(capture ingest.SessionContentCaptureWrite) bool {
	return publishableCaptureState(capture.Status, capture.FailureCode, capture.CaptureFormat)
}

// publishableCaptureState is the single statement of the rule. Both sides read
// it, so what the writer may certify and what the readers may serve cannot drift.
func publishableCaptureState(status ingest.ContentCaptureStatus, code ingest.ContentCaptureFailureCode, format ingest.ContentCaptureFormat) bool {
	switch status {
	case ingest.ContentCaptureComplete:
		// Unchanged: a complete capture is certified by its status and the
		// absence of a failure. The format is not questioned here, because it
		// never was, and a reader that reports only the state it knows must not
		// start being refused by this rule.
		return code == ingest.ContentCaptureNoFailure
	case ingest.ContentCaptureIncomplete:
		// The exception, and it is where the format matters: the allowed code
		// over a BOUNDED PREVIEW is still a preview, and a preview may never be
		// published. Only a capture that stores the full text qualifies.
		return code == ingest.ContentCaptureSourceRecordsOmitted &&
			format == ingest.ContentCaptureFormatFull
	}
	return false
}

// IncompleteByOmittedRecordsOnly reports the exception itself: a capture that is
// publishable although it is NOT complete. It is the state whose published
// metadata carries partial diagnostics and the omission warning.
func IncompleteByOmittedRecordsOnly(capture ingest.SessionContentCapture) bool {
	return capture.Status != ingest.ContentCaptureComplete && PublishableWithOmissions(capture)
}
