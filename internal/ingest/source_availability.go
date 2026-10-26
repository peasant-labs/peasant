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

// settleUnavailableSource records that a session's source cannot repair it and
// reports whether this run owes the user the transition. The stored row is
// written once. Every lane in the harvest that wrote it still owes its own
// report; a later harvest sees the row and owes nothing. It returns true when
// the evidence was recorded during this run or cannot be kept, so reporting
// never goes silent by accident. Callers keep doing their work: only the
// report settles, and the recorded row is never a reason to skip indexing.
func (p *Pipeline) settleUnavailableSource(ctx context.Context, sid SessionID, lane *storeWriteLane) bool {
	if p.unavailableRecordedThisRun(sid) {
		return true
	}
	// A row from an earlier harvest is not owed again. Reading it avoids a
	// write on every later recovery of a source the user already heard about.
	if p.sourceUnavailableRecorded(ctx, sid) && !p.config.Force {
		// A routine harvest owes no repeat, but --force is the remediation the
		// refusal names: it must report again, so a repair that did not take
		// cannot read as success.
		return false
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
		changed, err = recorder.RecordSourceUnavailable(ctx, sid, SourceUnavailableNoSavedCopy)
	})
	if err != nil {
		p.reportMetadataRefusal(string(sid), err)
		return true
	}
	if !changed {
		// A row written by an earlier harvest, or by another lane that does
		// not share this run's report, is not owed again. A row this run just
		// wrote is owed even when this call lost the insert, and an explicit
		// --force retry is always owed: it is the remediation the refusal names.
		return p.config.Force || p.unavailableRecordedThisRun(sid)
	}
	p.markUnavailableRecordedThisRun(sid)
	return true
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
}
