package push

import (
	"context"

	"github.com/peasant-labs/peasant/internal/clicontract"
	"github.com/peasant-labs/peasant/internal/githooks"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/perf"
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
	// Empty string means "use whatever is in config". A first publish opens at
	// it; an update keeps the audience the transcript has on the village unless
	// ChangeVisibility is set.
	Visibility schema.Visibility
	// ChangeVisibility says the caller asked for a visibility change, so an
	// update also moves a transcript the village already holds to Visibility.
	// An unchanged session gets the change as an owner update alone, sent
	// whatever the local receipt says, because the receipt may be stale. Only
	// an explicit request sets it (the --visibility flag), and it takes effect
	// only with a Visibility to change to. A configured default, or the
	// visibility the Share wizard opens a publication at, is not one: the
	// owner may have shared the transcript with collectives on the village
	// since, and nothing on this machine knows that.
	ChangeVisibility bool
	// License overrides config push.license for this run (--license flag).
	// Empty string means "use whatever is in config". A first publish sends it;
	// an update sends no license, so Village keeps the one the transcript has,
	// unless ChangeLicense is set.
	License schema.License
	// ChangeLicense says the caller asked for a license change, so an update
	// also sends License. Only an explicit request sets it (the --license flag),
	// and it takes effect only with a License to send.
	ChangeLicense bool
	// Concurrency is the maximum number of parallel uploads.
	// 0 means DefaultConcurrency.
	Concurrency int
	// JSONOutput requests machine-readable JSON on stdout.
	JSONOutput bool
	// Verbose requests per-session detail rows.
	Verbose bool
	// Quiet suppresses everything except errors, a waiting prompt request, and
	// the final result line. A git hook runs with it, so a degraded-but-
	// recoverable notice must not print into an ordinary commit or push; a
	// waiting prompt request is not a notice about the run, it is the reason the
	// author is being reached at all, and the command prints it rather than the
	// pipeline.
	Quiet bool
	// FilterSessionIDs, when non-nil, restricts the push to only these session IDs.
	// Set by the push wizard after user confirmation.
	FilterSessionIDs []string
	// PinnedSessionIDs, when non-nil, are the only sessions this run may send,
	// even when it is empty. The auto-publish rules decide the audience of the
	// sessions they matched before the run, so a session the run would
	// otherwise pick up later, or one a rule holds back, is not sent.
	PinnedSessionIDs map[string]bool
	// UpdateHold, when set, is asked before the content of a session this
	// account published to the Village before is sent again, and only then: a
	// session the Village already holds unchanged is skipped before it is
	// asked, and a first publication never reaches it. A non-empty answer
	// holds the session: nothing is sent, the result is PushStatusHeld, and
	// the answer is its HeldReason. The run's concurrent uploads call it.
	UpdateHold func(ctx context.Context, sessionID string) string
	// Selection, when non-nil, restricts the push to command-prepared decisions
	// computed from the complete stored-session cohort. nil means no selection
	// filter (push everything otherwise eligible).
	Selection *SessionSelection
	// InheritSelection maps each subagent session a push pulled in with a
	// named session to the named session at the root of its tree. Such a
	// session follows its root's selection decision: when the root passes
	// the saved selection, the subagent session is not checked against the
	// selection itself, because subagent sessions often run on branches the
	// developer never chose to exclude, such as an auto-named worktree
	// branch. The inheritance holds only while the root is sent by the same
	// run and is ready to publish; every other filter and the subagent
	// session's own readiness still apply. nil inherits nothing.
	InheritSelection map[string]string
	// Repository is an additional AND filter supplied by --repository. It uses
	// ingestion's canonical project identity and can only narrow the configured
	// selection. nil means no repository narrowing.
	Repository *RepositoryScope
	// CommandBinding is the typed, explicitly-bound config/data/state context
	// used to render recovery commands. Its zero value uses Peasant's defaults.
	CommandBinding githooks.Binding
	// Recorder optionally overrides the context recorder for direct pipeline
	// callers. The CLI supplies its shared recorder through the run context.
	// Nil resolves through perf.RecorderFromContext (Nop when disabled).
	Recorder perf.Recorder
	// LastGood selects the last-good full authority for publication
	// (--last-good). It bypasses ONLY the latest-attempt refusal and reads the
	// authority through the unchanged active read, so an explicitly consented
	// publication can still send the last-good full transcript while a newer
	// incomplete attempt is kept beside it. It is never set by a hook: the
	// flag is refused together with the hook's auto-publish flags.
	LastGood bool
}

// SelectionInheritedFrom reports the root a session's selection is inherited
// from: it was pulled in with that root, the saved selection excludes it on
// its own, and the root passes the selection. It is false for a session the
// selection admits on its own, so it names only the sessions the inheritance
// actually lets through.
func (c PipelineConfig) SelectionInheritedFrom(sessionID string) (string, bool) {
	root, pulled := c.InheritSelection[sessionID]
	if !pulled || c.Selection == nil {
		return "", false
	}
	own := c.Selection.Decision(ingest.SessionID(sessionID))
	if own == ingest.BranchMatchYes || c.Selection.Decision(ingest.SessionID(root)) != ingest.BranchMatchYes {
		return "", false
	}
	return root, true
}

// selectionDecision is the saved-selection decision a run applies to a
// session: its own, or yes when it inherits its root's.
func (c PipelineConfig) selectionDecision(sessionID ingest.SessionID) ingest.BranchMatch {
	if _, inherited := c.SelectionInheritedFrom(string(sessionID)); inherited {
		return ingest.BranchMatchYes
	}
	return c.Selection.Decision(sessionID)
}

// inheritingSelection returns the selection with each inherited session admitted, as
// SelectionInheritedFrom decides.
func (c PipelineConfig) inheritingSelection() *SessionSelection {
	if c.Selection == nil || len(c.InheritSelection) == 0 {
		return c.Selection
	}
	out := NewSessionSelection(c.Selection.decisions)
	for sessionID := range c.InheritSelection {
		if _, inherited := c.SelectionInheritedFrom(sessionID); inherited {
			out.decisions[ingest.SessionID(sessionID)] = ingest.BranchMatchYes
		}
	}
	return out
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
	// PushStatusUpdated means the village already had the session: it was
	// uploaded again (HTTP 200), or, for an explicit visibility change to an
	// unchanged session, changed by an owner update alone.
	PushStatusUpdated
	// PushStatusSkipped means the session was not uploaded because the village
	// already holds it unchanged. Annotation scoping relies on that: a skipped
	// session is on the village.
	PushStatusSkipped
	// PushStatusError means the upload attempt failed.
	PushStatusError
	// PushStatusHeld means the session was held back and nothing was sent:
	// the run's UpdateHold refused to send an update, and HeldReason says why.
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
	// HeldReason says why a session with Status PushStatusHeld was held back.
	HeldReason string
	// RequiredCapabilities is the exact receiver capability inventory the
	// session's durable payload requires, derived locally by the offline scan.
	// It is populated on every path that reaches the scan — a dry-run forecast,
	// a successful upload, and a capability refusal — so a caller can report what
	// a receiver must advertise before any negotiation and can explain a refusal
	// in terms of the payload. It is never a statement about a receiver's support.
	RequiredCapabilities []schema.ContentCapability
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
	// EmptyCode is EmptyReason in the closed set a --json document carries.
	// It is "" when the push sent something, or when the cause could not be
	// determined.
	EmptyCode clicontract.EmptyCode
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
