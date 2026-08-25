// Package sessionvisibility applies the persisted kickstart selection to
// user-facing discovery. It deliberately does not authorize direct access or
// deletion: callers use it only when enumerating sessions and projects.
package sessionvisibility

import (
	"errors"
	"fmt"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
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

// Candidate is the discovery-relevant identity of one stored session: the
// selection-scope fields, plus the origin the producer declared for it.
type Candidate struct {
	SessionID   ingest.SessionID
	Harness     ingest.Harness
	GitRemote   string
	ProjectName string
	ClonePath   ingest.ClonePath
	GitBranch   string
	// Origin is who drove the session, as recorded in sessions.session_origin.
	// It is read only by VisibleForDiscovery: selection scope never consults it,
	// which is what keeps the two scopes independent axes.
	Origin sessionorigin.Origin
	// ParentSessionID names the root a subagent transcript belongs to, empty for
	// a root session.
	//
	// It is deliberately NOT an input to any predicate here. A subagent row
	// leaves a discovery list through its origin, exactly like an agent-driven
	// root, because this path has no parent filter and must not grow one: a
	// parent filter would hide the row for a reason origin scope is responsible
	// for, and the two would then be impossible to tell apart. The field exists
	// so that a candidate describes the row completely and so the behaviour
	// fixture can state a subagent-shaped row whose origin alone decides it.
	ParentSessionID ingest.SessionID
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

	switch p.matcher.MatchBranchCandidate(ingest.DiscoveryCandidate{
		SessionID:          candidate.SessionID,
		Harness:            candidate.Harness,
		GitRemote:          candidate.GitRemote,
		ProjectName:        candidate.ProjectName,
		ClonePath:          candidate.ClonePath,
		Branch:             candidate.GitBranch,
		RemoteMultiplicity: ingest.DiscoveryIdentityUnique,
		NameMultiplicity:   ingest.DiscoveryIdentityUnique,
	}) {
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

// VisibleForDiscovery reports whether a candidate belongs in a discovery list:
// a picker, a chooser, a sessions list. It is the conjunction of the configured
// selection scope and the origin scope, which are separate, independent axes —
// either one alone can withhold a candidate, and neither can admit one the
// other withheld.
//
// Origin scope withholds exactly one value: a session a program drove
// (sessionorigin.Agent). user and unknown are both offered, because unknown is
// the fail-safe value and must behave like a person's own session everywhere in
// Peasant.
//
// Like selection scope, this is a DISCOVERY boundary and NEVER an access control
// boundary. A direct link to a hidden session still resolves — link resolution
// runs through a by-id path that applies neither scope, because a caller holding
// an identifier is not browsing.
func (p Policy) VisibleForDiscovery(candidate Candidate) (bool, error) {
	if err := candidate.Origin.Validate(); err != nil {
		return false, errorf(
			"session visibility: session %q carries an unusable origin in internal/sessionvisibility.Policy.VisibleForDiscovery while building a discovery list, so the list was withheld rather than guessed at: neither substitution is safe, because treating it as user would offer an agent-driven session and treating it as agent would hide a person's own work; re-ingest the session so sessions.session_origin holds a menu value, then retry: %v",
			candidate.SessionID, err,
		)
	}
	selected, err := p.Visible(candidate)
	if err != nil {
		return false, err
	}
	if !selected {
		return false, nil
	}
	return candidate.Origin != sessionorigin.Agent, nil
}
