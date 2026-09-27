//go:build evidence_negative_dedup

package transcript

import "testing"

func TestNegativeDedupFailsOnlyObservationBoundaryCase(t *testing.T) {
	t.Parallel()
	results := runModelObservationSurvivalFixture(loadModelObservationSurvivalFixture(t))
	assertExactlyFailedSurvivalCases(t, results, []string{"adjacent_observation_boundaries_survive_real_dedup"})
}
