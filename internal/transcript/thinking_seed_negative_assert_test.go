//go:build projection_negative_seed || projection_negative_scope

package transcript

import (
	"sort"
	"testing"
)

// TestThinkingSeedNegativeFailsOnlyDesignatedCases proves the fixture detects
// each deliberately wrong seed policy and nothing else.
func TestThinkingSeedNegativeFailsOnlyDesignatedCases(t *testing.T) {
	t.Parallel()
	var failed []string
	fixture := loadThinkingSeedFixture(t)
	for name, failures := range runThinkingSeedFixture(fixture) {
		if len(failures) > 0 {
			failed = append(failed, name)
		}
	}
	sort.Strings(failed)
	want := thinkingSeedNegativeExpectedFailures(fixture)
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
