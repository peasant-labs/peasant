package settings

import (
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/selectionprojection"
)

const noProjectsConfirmationEffects = "You selected no projects.\n" +
	"Your existing projects stay ingested and indexed.\n" +
	"The web viewer will not list them.\n" +
	"You cannot select them for a future push until you change this selection.\n" +
	"Peasant does not delete data."

const noProjectsConfirmationQuestion = "Save this choice?"

// CommitGate names the action required before a settings draft can commit.
type CommitGate uint8

const (
	// CommitGateNone permits the existing commit path to continue.
	CommitGateNone CommitGate = iota
	// CommitGateConfirmNoProjects requires confirmation before saving a choice
	// with no effective project or available selected descendant.
	CommitGateConfirmNoProjects
)

// CommitGateEvaluator evaluates the current draft selection immediately before
// its single atomic commit point.
type CommitGateEvaluator func(config.SelectionConfig) CommitGate

// NewCommitGateEvaluator snapshots one complete available project cohort and
// returns the evaluator mounted by kickstart. The evaluator compiles the
// canonical selected-mode matcher and always delegates the decision to
// EvaluateCommitGate; callers never run matcher methods or create gate logic of
// their own.
func NewCommitGateEvaluator(candidates []selectionprojection.ProjectCandidate) CommitGateEvaluator {
	snapshot := cloneProjectCandidates(candidates)
	return func(selection config.SelectionConfig) CommitGate {
		var matcher *ingest.SelectionMatcher
		if selection.Mode == config.SelectionModeSelected {
			compiled := config.CompileSelectionMatcher(selection)
			matcher = &compiled
		}
		return EvaluateCommitGate(selection, snapshot, matcher)
	}
}

// WithCommitGate mounts evaluator at the receipt's save action.
func WithCommitGate(evaluator CommitGateEvaluator) FlowOption {
	return func(flow *Flow) {
		flow.commitGate = evaluator
	}
}

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

func cloneProjectCandidates(candidates []selectionprojection.ProjectCandidate) []selectionprojection.ProjectCandidate {
	cloned := append([]selectionprojection.ProjectCandidate(nil), candidates...)
	for index := range cloned {
		cloned[index].Descendants = append([]selectionprojection.SessionCandidate(nil), candidates[index].Descendants...)
	}
	return cloned
}
