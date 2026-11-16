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

// SourceUnavailabilityLane identifies the diagnostic that owns a recorded
// source fault. It is local maintenance evidence, not transcript metadata.
type SourceUnavailabilityLane string

const (
	SourceLaneStoredContent      SourceUnavailabilityLane = "stored_content"
	SourceLanePairRepair         SourceUnavailabilityLane = "pair_repair"
	SourceLanePublicationCapture SourceUnavailabilityLane = "publication_capture"
	SourceLaneIndex              SourceUnavailabilityLane = "index"
)

var sourceUnavailabilityLanes = [...]SourceUnavailabilityLane{
	SourceLaneStoredContent, SourceLanePairRepair, SourceLanePublicationCapture, SourceLaneIndex,
}

// AllSourceUnavailabilityLanes is the closed diagnostic-lane set.
func AllSourceUnavailabilityLanes() []SourceUnavailabilityLane {
	return append([]SourceUnavailabilityLane(nil), sourceUnavailabilityLanes[:]...)
}

// NewSourceUnavailabilityLane validates a stored diagnostic lane.
func NewSourceUnavailabilityLane(raw string) (SourceUnavailabilityLane, error) {
	for _, lane := range sourceUnavailabilityLanes {
		if string(lane) == raw {
			return lane, nil
		}
	}
	return "", fmt.Errorf("read session source availability: unknown diagnostic lane %q; maintenance state was refused; use a supported lane or upgrade peasant", raw)
}

// SourceUnavailability identifies the lane owning a recorded source cause.
type SourceUnavailability struct {
	Lane   SourceUnavailabilityLane
	Reason SourceUnavailabilityReason
}

const (
	// SourceUnavailableNoSavedCopy means the original source is missing, so a
	// stored session cannot be repaired from it. Existing rows keep this value.
	SourceUnavailableNoSavedCopy SourceUnavailabilityReason = "original-source-unavailable-no-usable-saved-copy"
	// SourceUnavailableRecordUnusable means the original source is present but
	// a record in it cannot be used. It is a different cause from a missing
	// source, so a later harvest can report it instead of staying silent.
	SourceUnavailableRecordUnusable SourceUnavailabilityReason = "original-source-present-record-unusable"
)

func NewSourceUnavailabilityReason(raw string) (SourceUnavailabilityReason, error) {
	switch SourceUnavailabilityReason(raw) {
	case SourceUnavailableNoSavedCopy, SourceUnavailableRecordUnusable:
		return SourceUnavailabilityReason(raw), nil
	}
	return "", fmt.Errorf("read session source availability: unknown reason %q; maintenance state was refused; use a supported reason or upgrade peasant", raw)
}

// SupersededReason is the less-specific cause this observation replaces.
// The zero value means this cause does not supersede another recorded cause.
func (reason SourceUnavailabilityReason) SupersededReason() SourceUnavailabilityReason {
	if reason == SourceUnavailableRecordUnusable {
		return SourceUnavailableNoSavedCopy
	}
	return ""
}

// laneCauseReport identifies one diagnostic a harvest has already reported.
type laneCauseReport struct {
	session SessionID
	lane    SourceUnavailabilityLane
	cause   SourceUnavailabilityReason
}

// SourceAvailabilityStore records a cause transition for one diagnostic lane.
// Repeats and less-specific causes are not changes. A successful saved artifact
// refresh clears the session's evidence without removing prior content.
type SourceAvailabilityStore interface {
	RecordSourceUnavailable(context.Context, SessionID, SourceUnavailabilityLane, SourceUnavailabilityReason) (bool, error)
	ReadSourceUnavailabilityForLane(context.Context, SessionID, SourceUnavailabilityLane) (*SourceUnavailabilityReason, error)
}

// sourceUnavailable reports whether a stored-content failure is about the
// source bytes themselves: the source is gone, or a record in it cannot be
// parsed. Both are the user's to repair, so both settle. A store, locator, or
// write fault is neither, so it keeps reporting.
func sourceUnavailable(err error) bool {
	_, ok := sourceUnavailableCause(err)
	return ok
}

// sourceUnavailableCause classifies a source-shaped failure. A malformed
// record means the source is present but cannot be used. A missing file is
// the other class: there are no saved bytes to read. Anything else, including a
// store, locator, or write fault, is not a source cause.
func sourceUnavailableCause(err error) (SourceUnavailabilityReason, bool) {
	if err == nil {
		return "", false
	}
	var malformed malformedSourceRecord
	if errors.As(err, &malformed) {
		return SourceUnavailableRecordUnusable, true
	}
	if errors.Is(err, fs.ErrNotExist) {
		return SourceUnavailableNoSavedCopy, true
	}
	return "", false
}

// settleUnavailableSource decides whether a diagnostic lane owes a report.
// The store atomically inserts or upgrades the lane's cause, never downgrading
// it. Force reports again without changing that ordering. A failed write cannot
// suppress a diagnostic; a per-run claim prevents duplicate reports for the
// same lane and cause even on force or when the store is unavailable.
func (p *Pipeline) settleUnavailableSource(ctx context.Context, sid SessionID, diagnosticLane SourceUnavailabilityLane, cause SourceUnavailabilityReason, writeLane *storeWriteLane, stage Stage) bool {
	recorder, ok := p.metricsStore.(SourceAvailabilityStore)
	if !ok || p.config.DryRun {
		return p.claimLaneCauseReport(sid, diagnosticLane, cause)
	}
	var changed bool
	var err error
	p.runStoreWrite(writeLane, stage, func() {
		changed, err = recorder.RecordSourceUnavailable(ctx, sid, diagnosticLane, cause)
	})
	if err != nil {
		p.reportMetadataRefusal(string(sid), err)
		return p.claimLaneCauseReport(sid, diagnosticLane, cause)
	}
	if !changed && !p.config.Force {
		return false
	}
	return p.claimLaneCauseReport(sid, diagnosticLane, cause)
}

func (p *Pipeline) claimLaneCauseReport(sid SessionID, lane SourceUnavailabilityLane, cause SourceUnavailabilityReason) bool {
	p.sourceRunStateMu.Lock()
	defer p.sourceRunStateMu.Unlock()
	if p.laneCauseReportedThisRun == nil {
		p.laneCauseReportedThisRun = make(map[laneCauseReport]struct{})
	}
	key := laneCauseReport{session: sid, lane: lane, cause: cause}
	if _, seen := p.laneCauseReportedThisRun[key]; seen {
		return false
	}
	p.laneCauseReportedThisRun[key] = struct{}{}
	return true
}

func (p *Pipeline) resetUnavailableReportsThisRun() {
	p.sourceRunStateMu.Lock()
	defer p.sourceRunStateMu.Unlock()
	p.laneCauseReportedThisRun = nil
}
