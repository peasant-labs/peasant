//go:build dry_run_capability_negative

package push_test

import "testing"

func TestDryRunCapabilityNegativeExactMountedFailureSet(t *testing.T) {
	fixture := loadObservedModelCapabilityFixture(t)
	actual := []string{}
	for _, row := range fixture.Cases {
		if row.DryRun {
			actual = append(actual, row.Name)
		}
	}
	if len(actual) != 1 || actual[0] != "enriched_dry_run_remains_local_without_refusal" {
		t.Fatalf("mutated production decision affected mounted cases=%v, want exactly enriched_dry_run_remains_local_without_refusal", actual)
	}
}
