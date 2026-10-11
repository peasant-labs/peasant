package ingest

import (
	"context"
	"fmt"
)

// SourceUnavailabilityReason describes why a stored session cannot be repaired.
// It is local maintenance evidence, not transcript or wire metadata.
type SourceUnavailabilityReason string

const SourceUnavailableNoSavedCopy SourceUnavailabilityReason = "original-source-unavailable-no-usable-saved-copy"

func NewSourceUnavailabilityReason(raw string) (SourceUnavailabilityReason, error) {
	if raw == string(SourceUnavailableNoSavedCopy) {
		return SourceUnavailableNoSavedCopy, nil
	}
	return "", fmt.Errorf("read session source availability: unknown reason %q; maintenance state was refused; use a supported reason or upgrade peasant", raw)
}

// SourceAvailabilityStore records a transition only once. A successful saved
// artifact refresh clears this evidence without removing prior session content.
type SourceAvailabilityStore interface {
	RecordSourceUnavailable(context.Context, SessionID, SourceUnavailabilityReason) (bool, error)
	ReadSourceUnavailability(context.Context, SessionID) (*SourceUnavailabilityReason, error)
}
