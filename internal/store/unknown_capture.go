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
// full/preview branch, for preview as well as full writes. The sentinel for
// missing traversal coordinates proves every record decoded, every
// coordinated record validated, and ownership agreed inside the collector, so
// on the preview path it admits the set as legacy preview: mixed
// legacy+valid sets stay legacy (bounded storage, never a full certificate;
// export still refuses below). Corruption surfaces as integrity errors, never
// this sentinel. Fresh candidates keep the stricter wholly-absent rule in
// AssessCapture; the store is the durable boundary old rows cross on reindex.
// Corrupt preview input refuses before entry replacement, preserving
// last-good authority.
func preflightUnknownEvidence(entries []schema.SessionEntry, requireFull bool) error {
	_, err := ingest.CollectRetainedUnknown(entries, "")
	if err == nil {
		return nil
	}
	if errors.Is(err, ingest.ErrUnknownPositionUnavailable) {
		if !requireFull {
			return nil
		}
		return fmt.Errorf("store content evidence validation: %w; prior capture remains authoritative", err)
	}
	return fmt.Errorf("store content evidence validation: %w; prior capture remains authoritative", err)
}
