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

// laneCauseReport is one source cause already reported for a session by the
// stored-content lane or the pair-repair lane during the current harvest.
type laneCauseReport struct {
	session SessionID
	cause   SourceUnavailabilityReason
}

// SourceAvailabilityStore records a cause transition. Repeating the stored
// cause is not a change. A successful saved artifact refresh clears this
// evidence without removing prior session content.
type SourceAvailabilityStore interface {
	RecordSourceUnavailable(context.Context, SessionID, SourceUnavailabilityReason) (bool, error)
	ReadSourceUnavailability(context.Context, SessionID) (*SourceUnavailabilityReason, error)
}

// sourceRecordUnusable reports whether a stored-content failure is about the
// source bytes themselves: the source is gone, or a record in it cannot be
// parsed. A store, locator, or write fault is neither, so it keeps reporting.
func sourceRecordUnusable(err error) bool {
	_, ok := sourceUnavailableCause(err)
	return ok
}

// sourceUnavailableCause classifies a source-shaped failure. A malformed
// record means the source is present but cannot be used. A missing file is
// the other class. Anything else, including a store, locator, or write fault,
// is not a source cause.
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

// pairUnavailableCause classifies a pair-repair failure. A nil cause is a
// missing source: the repair was not attempted because no file could be
// reached. Any other cause uses the source-byte classifier.
func pairUnavailableCause(cause error) (SourceUnavailabilityReason, bool) {
	if cause == nil {
		return SourceUnavailableNoSavedCopy, true
	}
	return sourceUnavailableCause(cause)
}

// causeSupersedes reports whether next is a more specific observation than
// the cause already stored. A source that is present but unusable replaces a
// missing-source row, so restoring a missing file as a bad record is reported.
// The reverse does not replace it: another lane can still be stating a path
// that is absent while the bytes a later lane parsed are unusable, and
// letting that flip the row would make every later harvest report again.
func causeSupersedes(stored, next SourceUnavailabilityReason) bool {
	return stored == SourceUnavailableNoSavedCopy && next == SourceUnavailableRecordUnusable
}

// settleUnavailableSource records that a session's source cannot repair it
// and reports whether this run owes the user the transition. A stored row
// with the same cause is quiet on a routine harvest. A missing row, a cause
// that supersedes the stored one, or an explicit --force retry is recorded
// and owed. A less specific cause does not replace a more specific one and
// is quiet on a later harvest. A record error is reported and still owed, so
// a failure to keep the evidence cannot hide the diagnostic. A row written
// earlier in this same harvest is still owed by a later lane: the index lane
// reports even when stored-content recovery already recorded the same cause.
// Callers keep doing their work: only the report settles, and the recorded
// row is never a reason to skip indexing.
func (p *Pipeline) settleUnavailableSource(ctx context.Context, sid SessionID, cause SourceUnavailabilityReason, lane *storeWriteLane) bool {
	stored, readErr := p.readUnavailableCause(ctx, sid)
	same := readErr == nil && stored != nil && *stored == cause
	if stored != nil && !p.config.Force && (same || !causeSupersedes(*stored, cause)) {
		// A routine harvest owes no repeat of a cause the user already heard,
		// and it does not let a less specific observation erase a more
		// specific one. A row this run just wrote is still owed by a later lane.
		return p.unavailableRecordedThisRun(sid)
	}
	recorder, ok := p.metricsStore.(SourceAvailabilityStore)
	if !ok || p.config.DryRun {
		return true
	}
	var changed bool
	var err error
	// Parser workers reach this too, so the write takes the store lane the
	// other maintenance writes use instead of racing them.
	p.runStoreWrite(lane, func() {
		changed, err = recorder.RecordSourceUnavailable(ctx, sid, cause)
	})
	if err != nil {
		p.reportMetadataRefusal(string(sid), err)
		return true
	}
	if changed {
		p.markUnavailableRecordedThisRun(sid)
		return true
	}
	// The upsert found the same cause. --force still owes the report, and so
	// does a lane that lost the insert to another lane in this harvest.
	return p.config.Force || p.unavailableRecordedThisRun(sid)
}

func (p *Pipeline) readUnavailableCause(ctx context.Context, sid SessionID) (*SourceUnavailabilityReason, error) {
	reader, ok := p.metricsStore.(SourceAvailabilityStore)
	if !ok {
		return nil, nil
	}
	return reader.ReadSourceUnavailability(ctx, sid)
}

func (p *Pipeline) unavailableRecordedThisRun(sid SessionID) bool {
	p.unavailableSettledMu.Lock()
	defer p.unavailableSettledMu.Unlock()
	_, ok := p.unavailableSettledThisRun[sid]
	return ok
}

func (p *Pipeline) markUnavailableRecordedThisRun(sid SessionID) {
	p.unavailableSettledMu.Lock()
	defer p.unavailableSettledMu.Unlock()
	if p.unavailableSettledThisRun == nil {
		p.unavailableSettledThisRun = make(map[SessionID]struct{})
	}
	p.unavailableSettledThisRun[sid] = struct{}{}
}

func (p *Pipeline) resetUnavailableSettledThisRun() {
	p.unavailableSettledMu.Lock()
	defer p.unavailableSettledMu.Unlock()
	p.unavailableSettledThisRun = nil
	p.laneCauseReportedThisRun = nil
}

// laneCauseAlreadyReported reports whether the stored-content lane or the
// pair-repair lane already reported this cause for the session during this
// harvest. The index lane does not consult it.
func (p *Pipeline) laneCauseAlreadyReported(sid SessionID, cause SourceUnavailabilityReason) bool {
	p.unavailableSettledMu.Lock()
	defer p.unavailableSettledMu.Unlock()
	_, ok := p.laneCauseReportedThisRun[laneCauseReport{session: sid, cause: cause}]
	return ok
}

// markLaneCauseReported records a cause the stored-content lane or the
// pair-repair lane has reported in this harvest, so the other of those two
// lanes stays quiet for the same cause. A different cause is not marked.
func (p *Pipeline) markLaneCauseReported(sid SessionID, cause SourceUnavailabilityReason) {
	p.unavailableSettledMu.Lock()
	defer p.unavailableSettledMu.Unlock()
	if p.laneCauseReportedThisRun == nil {
		p.laneCauseReportedThisRun = make(map[laneCauseReport]struct{})
	}
	p.laneCauseReportedThisRun[laneCauseReport{session: sid, cause: cause}] = struct{}{}
}
