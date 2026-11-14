package ingest

import (
	"cmp"
	"errors"
	"slices"
	"strings"
)

// RunOutcome is deliberately payload-free. Messages and paths from raw errors
// and diagnostics can contain source text or private filesystem locations.
// The durable ledger retains identity and the existing reason vocabulary only.
// An empty SessionID denotes a run-wide diagnostic, not a guessed session.
// The kind is not stored: it derives from the reason.
type RunOutcome struct {
	RunID     int64
	SessionID SessionID
	Reason    OutcomeReason
	CreatedAt int64
}

type sessionDiagnostic struct {
	sessionID  SessionID
	diagnostic DiagnosticEntry
}

func (p *Pipeline) runOutcomes(results []SessionResult, indexLog []IndexLogEntry, createdAt int64) []RunOutcome {
	var outcomes []RunOutcome
	for _, result := range results {
		if result.Error == nil {
			continue
		}
		// Untyped worker errors have no existing finer reason code. Reuse the
		// existing error outcome rather than classifying arbitrary error text.
		reason := OutcomeReasonError
		var acquisition *adapterAcquisitionError
		if errors.As(result.Error, &acquisition) {
			// The capture failed. This is not the diagnostic of the same wire
			// spelling, which says a refresh fell back to the stored pair.
			reason = OutcomeReasonAdapterCaptureFailed
		}
		outcomes = append(outcomes, RunOutcome{SessionID: result.SessionID, Reason: reason, CreatedAt: createdAt})
	}
	outcomes = append(outcomes, p.refreshOutcomes(results, indexLog, createdAt)...)
	p.diagnosticsMu.Lock()
	defer p.diagnosticsMu.Unlock()
	associated := make(map[DiagnosticEntry]bool, len(p.diagnosticSessions))
	for entry := range p.diagnosticSessions {
		associated[entry.diagnostic] = true
		outcomes = append(outcomes, RunOutcome{SessionID: entry.sessionID, Reason: diagnosticOutcomeReason(entry.diagnostic), CreatedAt: createdAt})
	}
	for diagnostic := range p.diagnosticSet {
		if associated[diagnostic] {
			continue
		}
		outcomes = append(outcomes, RunOutcome{Reason: diagnosticOutcomeReason(diagnostic), CreatedAt: createdAt})
	}
	slices.SortFunc(outcomes, func(a, b RunOutcome) int {
		if order := strings.Compare(string(a.SessionID), string(b.SessionID)); order != 0 {
			return order
		}
		if order := cmp.Compare(a.Reason.Kind(), b.Reason.Kind()); order != 0 {
			return order
		}
		return cmp.Compare(a.Reason, b.Reason)
	})
	outcomes = slices.Compact(outcomes)
	return outcomes
}

// diagnosticOutcomeReason converts a wire diagnostic to its audit reason at the
// audit boundary. The wire ErrorType is untouched. An unknown producer code,
// or a code that is not a diagnostic, becomes the invalid zero value, never raw
// text: the store rejects it while preserving the independent aggregate.
func diagnosticOutcomeReason(diagnostic DiagnosticEntry) OutcomeReason {
	reason, err := NewOutcomeReason(diagnostic.ErrorType)
	if err != nil || reason.Kind() != OutcomeKindDiagnostic {
		return 0
	}
	return reason
}

func (p *Pipeline) reportSessionDiagnostic(sid SessionID, diagnostic DiagnosticEntry) {
	p.reportDiagnostic(diagnostic)
	p.retainSessionDiagnostic(sid, diagnostic)
}

// The store's write advisory contract places the validated session identity in
// Location. Other locations stay unattributed rather than being path-parsed.
func (p *Pipeline) reportWriteDiagnostic(diagnostic DiagnosticEntry) {
	if sid, err := NewSessionID(diagnostic.Location); err == nil {
		p.reportSessionDiagnostic(sid, diagnostic)
	} else {
		p.reportDiagnostic(diagnostic)
	}
}

// Retaining metadata warnings must not change the CLI's existing report.
func (p *Pipeline) retainSessionDiagnostic(sid SessionID, diagnostic DiagnosticEntry) {
	p.diagnosticsMu.Lock()
	defer p.diagnosticsMu.Unlock()
	if p.diagnosticSessions == nil {
		p.diagnosticSessions = make(map[sessionDiagnostic]struct{})
	}
	p.diagnosticSessions[sessionDiagnostic{sessionID: sid, diagnostic: diagnostic}] = struct{}{}
}
