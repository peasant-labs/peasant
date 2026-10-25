package ingest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
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

// sourceUnavailableRecorded reports whether the store already recorded that a
// session's source could not repair it. It reads the recorded state only: no
// managed file is opened, so a settled session costs no pair read.
func (p *Pipeline) sourceUnavailableRecorded(ctx context.Context, sid SessionID) bool {
	reader, ok := p.metricsStore.(SourceAvailabilityStore)
	if !ok {
		return false
	}
	reason, err := reader.ReadSourceUnavailability(ctx, sid)
	return err == nil && reason != nil
}

// sourceRecordUnusable reports whether a stored-content failure is about the
// source bytes themselves: the source is gone, or a record in it cannot be
// parsed. A store, locator, or write fault is neither, so it keeps reporting.
func sourceRecordUnusable(err error) bool {
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	var malformed malformedSourceRecord
	return errors.As(err, &malformed)
}
