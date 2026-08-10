// Package sessionvisibility applies the persisted kickstart selection to
// user-facing discovery. It deliberately does not authorize direct access or
// deletion: callers use it only when enumerating sessions and projects.
package sessionvisibility

import (
	"errors"
	"fmt"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
)

// Error marks a discovery failure caused by invalid persisted selection state.
// Callers use this type to attach recovery guidance without rewriting unrelated
// database, transport, or server failures as kickstart problems.
type Error struct{ message string }

func (e *Error) Error() string { return e.message }

func errorf(format string, args ...any) error { return &Error{message: fmt.Sprintf(format, args...)} }

// IsError reports whether err was caused by invalid persisted selection state.
func IsError(err error) bool {
	var target *Error
	return errors.As(err, &target)
}

// Candidate is the selection-relevant identity of one stored session.
type Candidate struct {
	SessionID   ingest.SessionID
	Harness     ingest.Harness
	GitRemote   string
	ProjectName string
	GitBranch   string
}

// Policy is an immutable, validated discovery projection.
type Policy struct {
	mode    config.SelectionMode
	matcher ingest.SelectionMatcher
}

// New validates a persisted selection and builds its immutable matcher.
func New(selection config.SelectionConfig) (Policy, error) {
	if !selection.Mode.IsValid() {
		return Policy{}, errorf(
			"session visibility: invalid selection mode %q; the persisted kickstart selection is missing or unsupported in internal/sessionvisibility.New while the server is starting, so discovery cannot be served safely; run `peasant kickstart` to repair selection.mode (valid: all, selected)",
			selection.Mode,
		)
	}

	return Policy{mode: selection.Mode, matcher: config.CompileSelectionMatcher(selection)}, nil
}

// All returns an explicit all-data policy for tests and non-configured tools.
func All() Policy { return Policy{mode: config.SelectionModeAll} }

// Active reports whether the policy is narrowing discovery (mode=selected),
// as opposed to exposing everything (mode=all). Callers use this to explain
// a sparse project/session list to the user rather than leaving it looking
// broken — it reports state only, never which
// projects or sessions the selection hides.
func (p Policy) Active() bool { return p.mode == config.SelectionModeSelected }

// ProjectionInputs returns the validated mode and canonical matcher for a
// cohort-aware discovery projection. The caller must still compute candidate
// identity multiplicity over its complete cohort before it uses the matcher.
// Mode all returns a nil matcher because batch projections must bypass selected-
// mode matching entirely.
func (p Policy) ProjectionInputs() (config.SelectionMode, *ingest.SelectionMatcher, error) {
	if !p.mode.IsValid() {
		return "", nil, errorf(
			"session visibility: uninitialized policy; no validated kickstart selection reached internal/sessionvisibility.Policy.ProjectionInputs while preparing a discovery cohort, so the caller must not expose a partial list; construct the provider with sessionvisibility.New(config.Selection), run `peasant kickstart` if the saved selection is invalid, and retry",
		)
	}
	if p.mode == config.SelectionModeAll {
		return p.mode, nil, nil
	}
	matcher := p.matcher
	return p.mode, &matcher, nil
}

// Visible reports whether a candidate belongs in a user-facing list.
func (p Policy) Visible(candidate Candidate) (bool, error) {
	if !p.mode.IsValid() {
		return false, errorf(
			"session visibility: uninitialized policy; no validated kickstart selection reached internal/sessionvisibility.Policy.Visible during discovery, so the caller must not expose stored rows; construct the provider with sessionvisibility.New(config.Selection) and retry",
		)
	}
	if p.mode == config.SelectionModeAll {
		return true, nil
	}

	switch p.matcher.MatchBranch(candidate.Harness, candidate.GitRemote, candidate.ProjectName, candidate.GitBranch, candidate.SessionID) {
	case ingest.BranchMatchYes:
		return true, nil
	case ingest.BranchMatchNo:
		return false, nil
	case ingest.BranchMatchWithheldConflict:
		return false, errorf(
			"session visibility: conflicting project branch rules for session %q; multiple persisted kickstart entries identify the same project but disagree about branch %q in internal/sessionvisibility.Policy.Visible during discovery, so the session was withheld and no partial list is safe; run `peasant kickstart` and make the project's branch selection consistent",
			candidate.SessionID, candidate.GitBranch,
		)
	default:
		return false, errorf("session visibility: unknown matcher result for session %q in internal/sessionvisibility.Policy.Visible; discovery was stopped to avoid exposing an unselected row; update peasant or rerun kickstart", candidate.SessionID)
	}
}
