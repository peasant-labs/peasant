package push

import (
	"github.com/peasant-labs/peasant/internal/githooks"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// PipelineConfig holds runtime flags for a single push run.
// Flags from the CLI layer populate this struct before passing to NewPipeline.
type PipelineConfig struct {
	// DryRun queries the store but makes no HTTP calls, writes no push_log, sets no pushed_at.
	DryRun bool
	// Force passes --force: calls AllPushableSessions regardless of pushed_at.
	Force bool
	// SourceProvider filters sessions to a single model_harness value (e.g. "claude").
	SourceProvider string
	// Visibility overrides config push.visibility for this run.
	// Empty string means "use whatever is in config".
	Visibility schema.Visibility
	// License overrides config push.license for this run (--license flag).
	// Empty string means "use whatever is in config".
	License schema.License
	// Concurrency is the maximum number of parallel uploads.
	// 0 means DefaultConcurrency.
	Concurrency int
	// JSONOutput requests machine-readable JSON on stdout.
	JSONOutput bool
	// Verbose requests per-session detail rows.
	Verbose bool
	// Quiet suppresses everything except errors and the final result line. A
	// git hook runs with it, so a degraded-but-recoverable notice must not print
	// into an ordinary commit or push.
	Quiet bool
	// FilterSessionIDs, when non-nil, restricts the push to only these session IDs.
	// Set by the push wizard after user confirmation.
	FilterSessionIDs []string
	// Selection, when non-nil, restricts the push to command-prepared decisions
	// computed from the complete stored-session cohort. nil means no selection
	// filter (push everything otherwise eligible).
	Selection *SessionSelection
	// Repository is an additional AND filter supplied by --repository. It uses
	// ingestion's canonical project identity and can only narrow the configured
	// selection. nil means no repository narrowing.
	Repository *RepositoryScope
	// CommandBinding is the typed, explicitly-bound config/data/state context
	// used to render recovery commands. Its zero value uses Peasant's defaults.
	CommandBinding githooks.Binding
}

// SessionSelection is the immutable branch-aware decision set prepared at the
// command boundary. A missing session fails closed. This keeps raw database path
// strings and filesystem resolution out of the push service while letting the
// wizard, dry run, annotation gate, and real pipeline consume one decision set.
type SessionSelection struct {
	decisions map[ingest.SessionID]ingest.BranchMatch
}

// NewSessionSelection copies the complete command-prepared decision set.
func NewSessionSelection(decisions map[ingest.SessionID]ingest.BranchMatch) *SessionSelection {
	copied := make(map[ingest.SessionID]ingest.BranchMatch, len(decisions))
	for sessionID, decision := range decisions {
		copied[sessionID] = decision
	}
	return &SessionSelection{decisions: copied}
}

// Decision returns the prepared result for sessionID. Rows that appear after
// preparation or otherwise lack cohort evidence fail closed.
func (s *SessionSelection) Decision(sessionID ingest.SessionID) ingest.BranchMatch {
	if s == nil {
		return ingest.BranchMatchYes
	}
	decision, ok := s.decisions[sessionID]
	if !ok {
		return ingest.BranchMatchNo
	}
	return decision
}

// PushStatus represents the outcome of pushing a single session.
type PushStatus int

const (
	// PushStatusNew means the session was uploaded for the first time (HTTP 201).
	PushStatusNew PushStatus = iota
	// PushStatusUpdated means the session was re-uploaded and the server already had it (HTTP 200).
	PushStatusUpdated
	// PushStatusSkipped means the session was intentionally not uploaded.
	PushStatusSkipped
	// PushStatusError means the upload attempt failed.
	PushStatusError
	// PushStatusHeld means the session was held back (e.g. missing metrics).
	PushStatusHeld
)

// String implements fmt.Stringer.
func (s PushStatus) String() string {
	switch s {
	case PushStatusNew:
		return "new"
	case PushStatusUpdated:
		return "updated"
	case PushStatusSkipped:
		return "skipped"
	case PushStatusError:
		return "error"
	case PushStatusHeld:
		return "held"
	default:
		return "unknown"
	}
}

// SessionPushResult is the per-session outcome from one push run.
type SessionPushResult struct {
	SessionID string
	HostSlug  string
	Title     string
	Status    PushStatus
	// Error is non-nil when Status == PushStatusError.
	Error error
}

// PushResult is the aggregate outcome of a complete push run.
type PushResult struct {
	// Sessions contains one entry per session that was considered.
	Sessions []SessionPushResult
	// New is the count of sessions uploaded for the first time.
	New int
	// Updated is the count of sessions re-uploaded (server already had them).
	Updated int
	// Skipped is the count of sessions intentionally not uploaded.
	Skipped int
	// Errors is the count of sessions that failed to upload.
	Errors int
	// Held is the count of sessions that were held back (e.g. missing metrics).
	Held int
	// EmptyReason is set when there are no sessions to push and explains why.
	EmptyReason string
	// BaseCandidateCount is the number of candidate sessions the base query
	// (QueryPushCandidates) returned BEFORE branch-aware selection filtering.
	// Callers use it to distinguish "selection excluded everything" (base > 0,
	// kept == 0) from "nothing to push at all" (base == 0) without re-querying.
	BaseCandidateCount int
}

// countStatus increments the appropriate counter in PushResult for the given status.
func (r *PushResult) countStatus(s PushStatus) {
	switch s {
	case PushStatusNew:
		r.New++
	case PushStatusUpdated:
		r.Updated++
	case PushStatusSkipped:
		r.Skipped++
	case PushStatusError:
		r.Errors++
	case PushStatusHeld:
		r.Held++
	}
}
