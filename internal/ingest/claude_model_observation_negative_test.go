//go:build evidence_negative_indexer

package ingest_test

import (
	"testing"

	"github.com/peasant-labs/schema"
)

func TestNegativeIndexerDropsAssistantModelObservation(t *testing.T) {
	fixture := loadClaudeModelObservationFixture(t)
	assertClaudeModelObservationFixture(t, fixture, func(entries []schema.SessionEntry) []schema.SessionEntry {
		for index := range entries {
			entries[index].Extra = nil
		}
		return entries
	})
}
