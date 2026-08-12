//go:build evidence_negative_suppression

package transcript

import (
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

func TestNegativeSuppressionDropsModelOnlyAssistant(t *testing.T) {
	fixture := loadModelObservationSurvivalFixture(t)
	assertModelObservationSurvivalFixture(t, fixture, func(entries []schema.SessionEntry) []ingest.Turn {
		turns := EntriesToTurns(entries)
		kept := turns[:0]
		for _, turn := range turns {
			if strings.TrimSpace(turn.Content) == "" && len(turn.ToolCalls) == 0 {
				continue
			}
			kept = append(kept, turn)
		}
		return kept
	})
}
