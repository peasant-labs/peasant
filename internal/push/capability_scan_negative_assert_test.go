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

	// The session-graph fixture notices the same mutant in its one uploading
	// case that advertises an unknown token beside the required v1 token.
	graph := loadSessionGraphPublishFixture(t)
	graphActual := []string{}
	for _, row := range graph.Cases {
		if row.Arm == "pipeline" && row.WantUploads == 1 && len(row.Advertisement) > 1 {
			graphActual = append(graphActual, row.Name)
		}
	}
	if len(graphActual) != 1 || graphActual[0] != "unknown_extra_token_with_v1_uploads" {
		t.Fatalf("mutated capability decision affected session-graph cases=%v, want exactly unknown_extra_token_with_v1_uploads", graphActual)
	}
}
