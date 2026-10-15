//go:build !projection_negative_seed && !projection_negative_scope

package transcript

import "testing"

func TestThinkingSeedProjectionProductionPath(t *testing.T) {
	t.Parallel()
	for name, failures := range runThinkingSeedFixture(loadThinkingSeedFixture(t)) {
		for _, failure := range failures {
			t.Errorf("%s: %s", name, failure)
		}
	}
}
