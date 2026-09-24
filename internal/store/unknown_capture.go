package store

import (
	"errors"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// A status flag alone is not an evidence certificate. Verify the exact stored
// payload and coordinates on both sides of the full-content trust boundary.
func validateUnknownCapture(entries []schema.SessionEntry, status ingest.ContentCaptureStatus, code ingest.ContentCaptureFailureCode) error {
	records, err := ingest.CollectRetainedUnknown(entries, "")
	if err != nil {
		return fmt.Errorf("store full content evidence validation: %w; prior capture remains authoritative", err)
	}
	if (len(records) == 0 && code == ingest.ContentCaptureUnknownDataRetained) || (len(records) > 0 && (status != ingest.ContentCaptureIncomplete || (code != ingest.ContentCaptureUnknownDataRetained && code != ingest.ContentCaptureSourceRecordsOmitted))) {
		return fmt.Errorf("store full content evidence validation: capture status and retained evidence disagree; no full-content certificate was issued; re-index the source with partial interpretation accounting")
	}
	return nil
}

// preflightUnknownEvidence runs retained integrity validation before the
// full/preview branch, for preview as well as full writes. The explicit
// legacy preview policy permits only wholly absent coordinates after all
// other evidence validates. Corrupt preview input refuses before entry
// replacement, preserving last-good authority.
func preflightUnknownEvidence(entries []schema.SessionEntry, requireFull bool) error {
	_, err := ingest.CollectRetainedUnknown(entries, "")
	if err == nil {
		return nil
	}
	if errors.Is(err, ingest.ErrUnknownPositionUnavailable) {
		if !requireFull && ingest.LegacyCoordinatesWhollyAbsent(entries) {
			if legacyErr := ingest.ValidateV1LegacyEvidence(entries, ""); legacyErr != nil {
				return fmt.Errorf("store preview evidence validation: %w; prior capture remains authoritative", legacyErr)
			}
			return nil
		}
		return fmt.Errorf("store content evidence validation: %w; prior capture remains authoritative", err)
	}
	return fmt.Errorf("store content evidence validation: %w; prior capture remains authoritative", err)
}
