//go:build evidence_negative_dedup

package transcript

import (
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

func TestNegativeDedupIgnoresModelObservation(t *testing.T) {
	fixture := loadModelObservationSurvivalFixture(t)
	assertModelObservationSurvivalFixture(t, fixture, func(entries []schema.SessionEntry) []ingest.Turn {
		turns := EntriesToTurns(entries)
		deduped := make([]ingest.Turn, 0, len(turns))
		for _, turn := range turns {
			if len(deduped) > 0 {
				previous := deduped[len(deduped)-1]
				if previous.Role == turn.Role && previous.Content == turn.Content && strings.TrimSpace(turn.Content) != "" {
					continue
				}
			}
			deduped = append(deduped, turn)
		}
		return deduped
	})
}
