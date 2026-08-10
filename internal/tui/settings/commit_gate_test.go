package settings_test

import (
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/selectionprojection/testfixture"
	"github.com/peasant-labs/peasant/internal/tui/settings"
)

func TestEvaluateCommitGate_SharedEffectiveProjectFixtures(t *testing.T) {
	t.Parallel()

	fixtures, err := testfixture.LoadEffectiveProjectFixtures()
	if err != nil {
		t.Fatalf("load effective-project fixtures: %v", err)
	}
	for _, gateCase := range fixtures.GateCases {
		gateCase := gateCase
		t.Run(gateCase.Name, func(t *testing.T) {
			t.Parallel()

			projectionCase, ok := fixtures.ProjectionCaseByName(gateCase.ProjectionCase)
			if !ok {
				t.Fatalf("projection case %q is unavailable after fixture validation", gateCase.ProjectionCase)
			}
			candidates, err := projectionCase.ProductionCandidates()
			if err != nil {
				t.Fatalf("build production candidates: %v", err)
			}

			matcher := matcherForGateFixture(t, projectionCase.Selection)
			got := settings.EvaluateCommitGate(projectionCase.Selection, candidates, matcher)
			want := productionCommitGate(t, gateCase.Expected)
			if got != want {
				t.Errorf("EvaluateCommitGate() = %d, want %d", got, want)
			}
		})
	}
}

func matcherForGateFixture(t *testing.T, selection config.SelectionConfig) *ingest.SelectionMatcher {
	t.Helper()

	switch selection.Mode {
	case config.SelectionModeAll:
		return nil
	case config.SelectionModeSelected:
		matcher := config.CompileSelectionMatcher(selection)
		return &matcher
	default:
		t.Fatalf("fixture passed validation with unsupported selection mode %q", selection.Mode)
		return nil
	}
}

func productionCommitGate(t *testing.T, expectation testfixture.GateExpectation) settings.CommitGate {
	t.Helper()

	switch expectation {
	case testfixture.GateExpectationNone:
		return settings.CommitGateNone
	case testfixture.GateExpectationConfirmNoProjects:
		return settings.CommitGateConfirmNoProjects
	default:
		t.Fatalf("fixture passed validation with unsupported gate expectation %q", expectation)
		return settings.CommitGateNone
	}
}
