//go:build dry_run_capability_negative

package push_test

import "testing"

func TestDryRunCapabilityNegativeDesignatesEnrichedDryRun(t *testing.T) {
	fixture := loadObservedModelCapabilityFixture(t)
	affected := []string{}
	for _, fixtureCase := range fixture.Cases {
		if fixtureCase.DryRun {
			affected = append(affected, fixtureCase.Name)
		}
	}
	if len(affected) != 1 || affected[0] != "enriched_dry_run_remains_local_without_refusal" {
		t.Fatalf("negative affected cases=%v, want exactly enriched_dry_run_remains_local_without_refusal", affected)
	}
}
