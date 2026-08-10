package settings

import (
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/selectionprojection"
)

// CommitGate names the action required before a settings draft can commit.
type CommitGate uint8

const (
	// CommitGateNone permits the existing commit path to continue.
	CommitGateNone CommitGate = iota
	// CommitGateConfirmNoProjects requires confirmation before saving a choice
	// with no effective project or available selected descendant.
	CommitGateConfirmNoProjects
)

// EvaluateCommitGate evaluates the complete available project cohort through
// the shared effective-project projection. EffectiveProjects promotes every
// admitted available descendant to its parent result, so an empty projection
// means that both the effective-parent and effective-descendant sets are empty.
// Mode all remains the projection's concern; this function never compiles a
// selected-mode matcher or derives a second selection model.
func EvaluateCommitGate(
	selection config.SelectionConfig,
	candidates []selectionprojection.ProjectCandidate,
	matcher *ingest.SelectionMatcher,
) CommitGate {
	effectiveProjects := selectionprojection.EffectiveProjects(matcher, selection, candidates)
	if len(effectiveProjects) != 0 {
		return CommitGateNone
	}
	return CommitGateConfirmNoProjects
}
