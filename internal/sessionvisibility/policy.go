// Package sessionvisibility holds the discovery projection a browsing surface
// applies to stored sessions, and the saved-selection membership that ingest
// and push metadata report.
//
// Browsing is origin-only. The saved kickstart selection controls what Peasant
// discovers and ingests next, and what a hook or CLI push publishes; it never
// decides which already-ingested rows a person may browse. A restrictive, empty,
// stale or invalid saved selection therefore cannot hide the ingested library.
package sessionvisibility

import (
	"errors"
	"fmt"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
)

// Error marks a failure caused by unusable stored session metadata or an
// invalid saved selection. Browsing surfaces use it for an unusable origin; the
// ingest/push metadata read uses it for a saved selection it cannot apply.
type Error struct{ message string }

func (e *Error) Error() string { return e.message }

func errorf(format string, args ...any) error { return &Error{message: fmt.Sprintf(format, args...)} }

// IsError reports whether err was caused by unusable session metadata or an
// invalid saved selection.
func IsError(err error) bool {
	var target *Error
	return errors.As(err, &target)
}

// browsingRevision identifies the browsing projection a member scope was issued
// under. It is deliberately independent of the saved selection: changing which
// sessions Peasant will ingest next must not invalidate a browsing continuation.
const browsingRevision = "browsing/v1"

// Candidate is the discovery-relevant identity of one stored session: the
// origin the producer declared for it, plus the identity fields the canonical
// selection matcher needs when a caller reports ingest or push membership.
type Candidate struct {
	SessionID   ingest.SessionID
	Harness     ingest.Harness
	GitRemote   string
	ProjectName string
	ClonePath   ingest.ClonePath
	GitBranch   string
	// Origin is who drove the session, as recorded in sessions.session_origin.
	// VisibleForDiscovery reads it; selection membership never consults it, which
	// is what keeps the two axes independent.
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

// Policy carries two independent projections:
//
//   - the discovery projection, which is origin-only and answers
//     VisibleForDiscovery; every browsing surface uses this;
//   - the saved-selection membership: SelectedForIngest answers for an isolated
//     candidate; Selection supplies the selector to cohort-aware publication
//     metadata, which also needs repository topology and clone multiplicities.
//
// A browsing caller must not use SelectedForIngest or the saved selection for
// any list, count, picker, search, or paging decision.
type Policy struct {
	selection config.SelectionConfig
	matcher   ingest.SelectionMatcher
	// revision is the browsing revision, never the selection digest.
	revision string
}

// New carries a saved selection for the ingest/push metadata read. It does not
// validate the selection, because browsing does not depend on it: an invalid
// saved selection must not stop the server from serving the stored library. A
// metadata read that needs the selection fails at the point of use, where the
// error can name the remedy without touching browsing.
func New(selection config.SelectionConfig) Policy {
	return Policy{
		selection: selection,
		matcher:   config.CompileSelectionMatcher(selection),
		revision:  browsingRevision,
	}
}

// Browsing returns the origin-only discovery policy. It carries no saved
// selection: browsing never consults one.
func Browsing() Policy {
	return New(config.SelectionConfig{Mode: config.SelectionModeAll})
}

// Selection returns the saved ingest/push selector for cohort-aware metadata
// projection. Callers must treat it as immutable; browsing must not consult it.
func (p Policy) Selection() config.SelectionConfig { return p.selection }

// Revision returns the stable browsing revision. Discovery readers record it
// beside a list so a member continuation replays the same browsing projection.
func (p Policy) Revision() string { return p.revision }

// SelectedForIngest reports whether the saved selection admits a candidate for
// ingest or push metadata. It is NOT a browsing predicate: use
// VisibleForDiscovery for discovery lists.
func (p Policy) SelectedForIngest(candidate Candidate) (bool, error) {
	if !p.selection.Mode.IsValid() {
		return false, errorf(
			"session visibility: the saved kickstart selection has no valid mode while ingest/push membership was requested for session %q; browsing is unaffected, but the publication metadata cannot be reported truthfully; run `peasant kickstart` to repair selection.mode and retry the publication read",
			candidate.SessionID,
		)
	}
	if p.selection.Mode == config.SelectionModeAll {
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
			"session visibility: conflicting project branch rules for session %q; multiple persisted kickstart entries identify the same project but disagree about branch %q, so ingest/push membership cannot be reported for that session; browsing is unaffected; run `peasant kickstart` and make the project's branch selection consistent",
			candidate.SessionID, candidate.GitBranch,
		)
	default:
		return false, errorf("session visibility: unknown matcher result for session %q while reporting ingest/push membership; update peasant or rerun kickstart", candidate.SessionID)
	}
}

// VisibleForDiscovery reports whether a candidate belongs in a discovery list:
// a picker, a chooser, a sessions list. It applies the origin scope ONLY. The
// saved kickstart selection is deliberately absent: selection decides what is
// ingested next, not what an already-ingested row may show in the library.
//
// Origin scope withholds exactly one value: a session a program drove
// (sessionorigin.Agent). user and unknown are both offered, because unknown is
// the fail-safe value and must behave like a person's own session everywhere in
// Peasant.
//
// This is a DISCOVERY boundary and NEVER an access control boundary. A direct
// link to a hidden session still resolves — link resolution runs through a
// by-id path that applies neither scope, because a caller holding an identifier
// is not browsing.
func (p Policy) VisibleForDiscovery(candidate Candidate) (bool, error) {
	if err := candidate.Origin.Validate(); err != nil {
		return false, errorf(
			"session visibility: session %q carries an unusable origin in internal/sessionvisibility.Policy.VisibleForDiscovery while building a discovery list, so the list was withheld rather than guessed at: neither substitution is safe, because treating it as user would offer an agent-driven session and treating it as agent would hide a person's own work; re-ingest the session so sessions.session_origin holds a menu value, then retry: %v",
			candidate.SessionID, err,
		)
	}
	return candidate.Origin != sessionorigin.Agent, nil
}
