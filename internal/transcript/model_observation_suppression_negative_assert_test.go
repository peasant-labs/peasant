//go:build evidence_negative_suppression

package transcript

import "testing"

func TestNegativeSuppressionFailsOnlyModelOnlyCase(t *testing.T) {
	results := runModelObservationSurvivalFixture(loadModelObservationSurvivalFixture(t))
	assertExactlyFailedSurvivalCases(t, results, []string{"model_only_assistant_survives"})
}
