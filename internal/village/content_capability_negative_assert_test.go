//go:build capability_scan_negative

package village_test

import (
	"sort"
	"testing"

	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
)

func TestCapabilityScanNegativeFailsOnlyMixedKnownUnknownCases(t *testing.T) {
	failed := []string{}
	for _, fixtureCase := range loadContentCapabilityFixture(t).Cases {
		got := len(village.MissingContentCapabilities(fixtureCase.Response.ContentCapabilities, []schema.ContentCapability{schema.ContentCapabilityObservedModelV1})) == 0
		if got != fixtureCase.SupportsObservedModel {
			failed = append(failed, fixtureCase.Name)
		}
	}
	sort.Strings(failed)
	want := []string{"unknown_after_known_token_is_tolerated", "unknown_before_known_token_is_tolerated"}
	if len(failed) != len(want) || failed[0] != want[0] || failed[1] != want[1] {
		t.Fatalf("negative failed cases=%v, want exactly %v", failed, want)
	}
}
