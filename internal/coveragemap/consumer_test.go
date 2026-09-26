// Package coveragemap_test is an external consumer of the coverage-map
// contract: it decodes and validates a map with only exported names.
package coveragemap_test

import (
	"testing"

	"github.com/peasant-labs/peasant/internal/coveragemap"
)

func TestConsumer_DecodesAndValidates(t *testing.T) {
	inventory, err := coveragemap.DecodeInventory([]byte(
		"version: 1\nfrozen_from: 80b0ed58abc1234567890abcdef1234567890ab\nnames:\n" +
			"  - { name: TestA, kind: count-guard, where: a.go:1 }\n" +
			"  - { name: seedB, kind: seed-helper, where: b.go:2 }\n"))
	if err != nil {
		t.Fatalf("decode inventory: %v", err)
	}
	if err := coveragemap.ValidateInventory(inventory); err != nil {
		t.Fatalf("validate inventory: %v", err)
	}

	m, err := coveragemap.DecodeCoverageMap([]byte(
		"version: 1\ninventory: testdata/inventory.yaml\nentries:\n" +
			"  - { name: TestA, destination: retained-in-place }\n" +
			"  - { name: seedB, destination: \"followup:task-7\" }\n"))
	if err != nil {
		t.Fatalf("decode map: %v", err)
	}
	if m.Entries[1].Destination.Kind != coveragemap.DestinationFollowup {
		t.Fatalf("destination kind = %q, want %q", m.Entries[1].Destination.Kind, coveragemap.DestinationFollowup)
	}
	if err := coveragemap.ValidateCoverageMap("", inventory, m); err != nil {
		t.Fatalf("validate coverage map: %v", err)
	}
}
