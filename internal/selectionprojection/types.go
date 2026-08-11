// Package selectionprojection derives the projects made effective by a saved
// selection and the available local project/session evidence.
package selectionprojection

import (
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
)

// ParentProjectID is the stable identity of the project that owns a candidate.
// It groups evidence when no resolved physical clone path is available, but it
// does not prove physical clone uniqueness.
type ParentProjectID string

// SessionCandidate is one available descendant session of a project.
type SessionCandidate struct {
	SessionID       ingest.SessionID
	Branch          string
	ParentSessionID ingest.SessionID
	ClonePath       ingest.ClonePath
	// RepositoryPath is optional transient multiplicity evidence. Exact
	// selection still matches ClonePath.
	RepositoryPath ingest.RepositoryPath
}

// ProjectCandidate is one available project and its available descendant
// sessions. ClonePath is the project-level fallback when a descendant has no
// resolved session worktree of its own.
type ProjectCandidate struct {
	ParentProjectID ParentProjectID
	Harness         ingest.Harness
	GitRemote       string
	ProjectName     string
	ClonePath       ingest.ClonePath
	// RepositoryPath is optional transient multiplicity evidence. Producers
	// that do not resolve Git common directories retain the established
	// ClonePath-based behavior.
	RepositoryPath ingest.RepositoryPath
	Descendants    []SessionCandidate
}

// ProjectAdmission names the selection evidence that made a project effective.
type ProjectAdmission int

const (
	// ProjectNotEffective means no available candidate evidence was admitted.
	ProjectNotEffective ProjectAdmission = iota
	// ProjectEffectiveWholeProject means an unrestricted project rule admitted
	// the project.
	ProjectEffectiveWholeProject
	// ProjectEffectiveBranch means a branch-restricted project rule admitted at
	// least one available descendant.
	ProjectEffectiveBranch
	// ProjectEffectiveExplicitSession means an available session ID was selected
	// directly.
	ProjectEffectiveExplicitSession
	// ProjectEffectiveNestedDescendant means an available child belongs to an
	// explicitly selected parent session.
	ProjectEffectiveNestedDescendant
)

// EffectiveProject is one project admitted by the saved selection. When more
// than one route admits it, Admission reports the most specific available
// evidence in the enum's declaration order.
type EffectiveProject struct {
	Candidate               ProjectCandidate
	Admission               ProjectAdmission
	SelectedDescendantCount int
}

// EffectiveProjects projects a complete candidate cohort through the canonical
// selection matcher. Mode all admits every candidate without consulting the
// matcher. Selected mode fails closed when matcher is nil. Remote and name
// evidence also fail closed when physical-identity multiplicity cannot be
// proved.
func EffectiveProjects(
	matcher *ingest.SelectionMatcher,
	selection config.SelectionConfig,
	candidates []ProjectCandidate,
) []EffectiveProject {
	return effectiveProjects(matcher, selection, candidates)
}
