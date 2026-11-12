package ingest_test

import (
	"slices"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// TestHarvestableHarnessesFollowTheAdapterRegistry holds the harvestable set to
// its one owner, the production adapter registry, and to the contract's known
// order. The registry is read by ranging over the map, so the expectation
// restates no harness list.
func TestHarvestableHarnessesFollowTheAdapterRegistry(t *testing.T) {
	t.Parallel()
	got := ingest.HarvestableHarnesses()

	registered := make(map[ingest.Harness]bool, len(ingest.DefaultAdapterRegistry))
	for harness := range ingest.DefaultAdapterRegistry {
		registered[harness] = true
	}
	returned := make(map[ingest.Harness]bool, len(got))
	for _, harness := range got {
		if returned[harness] {
			t.Errorf("HarvestableHarnesses() lists %q more than once: %v", harness, got)
		}
		returned[harness] = true
		if !registered[harness] {
			t.Errorf("HarvestableHarnesses() lists %q, which has no adapter in DefaultAdapterRegistry; the registry is the only owner of the harvestable set", harness)
		}
	}
	for harness := range registered {
		if !returned[harness] {
			t.Errorf("HarvestableHarnesses() omits %q, which has an adapter in DefaultAdapterRegistry: %v", harness, got)
		}
	}

	known := schema.Harnesses()
	previous := -1
	for _, harness := range got {
		index := slices.Index(known, harness)
		if index < 0 {
			t.Errorf("harvestable harness %q is not in the contract's known set %v", harness, known)
			continue
		}
		if index <= previous {
			t.Errorf("HarvestableHarnesses() = %v is not in the contract's known order %v: %q is out of place", got, known, harness)
		}
		previous = index
	}
}
