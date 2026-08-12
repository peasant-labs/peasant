//go:build evidence_negative_suppression || evidence_negative_dedup

package transcript

import "testing"

func assertExactlyFailedSurvivalCases(t *testing.T, results []modelObservationSurvivalResult, expected []string) {
	t.Helper()
	failed := make([]string, 0, len(results))
	for _, result := range results {
		if len(result.Failures) > 0 {
			failed = append(failed, result.Name)
		}
	}
	if len(failed) != len(expected) {
		t.Fatalf("failed cases = %v, want exactly %v", failed, expected)
	}
	for index := range expected {
		if failed[index] != expected[index] {
			t.Fatalf("failed cases = %v, want exactly %v", failed, expected)
		}
	}
}
