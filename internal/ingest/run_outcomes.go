package ingest

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// RunOutcomeKind separates worker failures from nonfatal diagnostics. Neither
// is an index-stage counter; index_log remains the index attempt ledger.
type RunOutcomeKind string

const (
	RunOutcomeWorkerError RunOutcomeKind = "worker_error"
	RunOutcomeDiagnostic  RunOutcomeKind = "diagnostic"
)

func NewRunOutcomeKind(raw string) (RunOutcomeKind, error) {
	switch raw {
	case string(RunOutcomeWorkerError):
		return RunOutcomeWorkerError, nil
	case string(RunOutcomeDiagnostic):
		return RunOutcomeDiagnostic, nil
	default:
		return "", fmt.Errorf("read run outcome kind %q during audit: unknown kind; no outcome was recorded; use worker_error or diagnostic", raw)
	}
}

// RunOutcome is deliberately payload-free. Messages and paths from raw errors
// and diagnostics can contain source text or private filesystem locations.
// The durable ledger retains identity and the existing reason vocabulary only.
// An empty SessionID denotes a run-wide diagnostic, not a guessed session.
type RunOutcome struct {
	RunID      int64
	SessionID  SessionID
	Kind       RunOutcomeKind
	ReasonCode string
	CreatedAt  int64
}

type sessionDiagnostic struct {
	sessionID  SessionID
	diagnostic DiagnosticEntry
}

func (p *Pipeline) runOutcomes(results []SessionResult, createdAt int64) []RunOutcome {
	var outcomes []RunOutcome
	for _, result := range results {
		if result.Error == nil {
			continue
		}
		// Untyped worker errors have no existing finer reason code. Reuse the
		// existing error outcome rather than classifying arbitrary error text.
		reason := IndexOutcomeError.String()
		var acquisition *adapterAcquisitionError
		if errors.As(result.Error, &acquisition) {
			reason = "adapter_refresh_unavailable"
		}
		outcomes = append(outcomes, RunOutcome{SessionID: result.SessionID, Kind: RunOutcomeWorkerError, ReasonCode: reason, CreatedAt: createdAt})
	}
	p.diagnosticsMu.Lock()
	defer p.diagnosticsMu.Unlock()
	associated := make(map[DiagnosticEntry]bool, len(p.diagnosticSessions))
	for entry := range p.diagnosticSessions {
		associated[entry.diagnostic] = true
		outcomes = append(outcomes, RunOutcome{SessionID: entry.sessionID, Kind: RunOutcomeDiagnostic, ReasonCode: entry.diagnostic.ErrorType, CreatedAt: createdAt})
	}
	for diagnostic := range p.diagnosticSet {
		if associated[diagnostic] {
			continue
		}
		outcomes = append(outcomes, RunOutcome{Kind: RunOutcomeDiagnostic, ReasonCode: diagnostic.ErrorType, CreatedAt: createdAt})
	}
	slices.SortFunc(outcomes, func(a, b RunOutcome) int {
		if order := strings.Compare(string(a.SessionID), string(b.SessionID)); order != 0 {
			return order
		}
		if order := strings.Compare(string(a.Kind), string(b.Kind)); order != 0 {
			return order
		}
		return strings.Compare(a.ReasonCode, b.ReasonCode)
	})
	outcomes = slices.Compact(outcomes)
	return outcomes
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
