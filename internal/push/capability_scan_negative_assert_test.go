//go:build capability_scan_negative

package push_test

import "testing"

func TestCapabilityScanNegativeExactMountedFailureSet(t *testing.T) {
	fixture := loadObservedModelCapabilityFixture(t)
	actual := []string{}
	for _, row := range fixture.Cases {
		if row.ObservedModel != "" && row.WantUploads == 1 && len(row.Advertisement) > 1 {
			actual = append(actual, row.Name)
		}
	}
	if len(actual) != 1 || actual[0] != "enriched_mixed_unknown_known_target_uploads" {
		t.Fatalf("mutated production decision affected mounted cases=%v, want exactly enriched_mixed_unknown_known_target_uploads", actual)
	}
}
