package store

import (
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// A status flag alone is not an evidence certificate. Verify the exact stored
// payload and coordinates on both sides of the full-content trust boundary.
func validateUnknownCapture(entries []schema.SessionEntry, status ingest.ContentCaptureStatus, code ingest.ContentCaptureFailureCode) error {
	records, err := ingest.ProjectRetainedUnknown(entries, "")
	if err != nil {
		return fmt.Errorf("store full content evidence validation: %w; prior capture remains authoritative", err)
	}
	if (len(records) == 0 && code == ingest.ContentCaptureUnknownDataRetained) || (len(records) > 0 && (status != ingest.ContentCaptureIncomplete || (code != ingest.ContentCaptureUnknownDataRetained && code != ingest.ContentCaptureSourceRecordsOmitted))) {
		return fmt.Errorf("store full content evidence validation: capture status and retained evidence disagree; no full-content certificate was issued; re-index the source with partial interpretation accounting")
	}
	return nil
}
