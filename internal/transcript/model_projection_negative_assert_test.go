//go:build projection_negative_fold || projection_negative_seed || projection_negative_scope

package transcript

import (
	"sort"
	"testing"
)

func TestModelProjectionNegativeFailsOnlyDesignatedCase(t *testing.T) {
	results := runModelProjectionFixture(loadModelProjectionFixture(t))
	failed := make([]string, 0, len(results))
	for _, result := range results {
		if len(result.Failures) > 0 {
			failed = append(failed, result.Name)
		}
	}
	sort.Strings(failed)
	want := modelProjectionNegativeExpectedFailures()
	sort.Strings(want)
	if len(failed) != len(want) {
		t.Fatalf("negative failed cases=%v, want exactly %v", failed, want)
	}
	for index := range want {
		if failed[index] != want[index] {
			t.Fatalf("negative failed cases=%v, want exactly %v", failed, want)
		}
	}
}
