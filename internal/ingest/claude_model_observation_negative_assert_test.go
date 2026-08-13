//go:build evidence_negative_indexer

package ingest_test

import "testing"

func TestNegativeIndexerFailsOnlySourceObservationCase(t *testing.T) {
	results := runClaudeModelObservationFixture(t, loadClaudeModelObservationFixture(t))
	assertExactlyFailedCases(t, results, []string{"assistant_observations_keep_exact_order"})
}

func assertExactlyFailedCases(t *testing.T, results []claudeModelObservationResult, expected []string) {
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
