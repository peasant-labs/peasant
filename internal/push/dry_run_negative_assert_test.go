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

	// The session-graph fixture notices the same mutant in every case that runs
	// an offline scan expecting zero network calls.
	graph := loadSessionGraphPublishFixture(t)
	graphActual := []string{}
	for _, row := range graph.Cases {
		if row.DryRun || row.ScanFirst {
			graphActual = append(graphActual, row.Name)
		}
	}
	want := map[string]bool{
		"offline_scan_derives_graph_requirement_with_no_network": true,
		"offline_scan_plain_payload_requires_nothing":            true,
		"support_changed_after_scan_to_unsupported_refuses":      true,
	}
	if len(graphActual) != len(want) {
		t.Fatalf("mutated dry-run decision affected session-graph cases=%v, want %v", graphActual, want)
	}
	for _, name := range graphActual {
		if !want[name] {
			t.Fatalf("mutated dry-run decision affected unexpected session-graph case %q", name)
		}
	}
}
