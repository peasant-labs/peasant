package transcript

import (
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// sessionThinkingHistory resolves the session thinking-level history for the
// detail payload. Like sessionThinkingSeed, the folded root-assistant
// observations win when any turn carries evidence: they are the same
// depth-0 scope the seed reads, so the seed and the history stay consistent.
// A session whose turns carry no evidence (a push, export or snapshot
// projection, or a legacy row) falls back to the stored history the extractor
// recorded.
func sessionThinkingHistory(session *ingest.Session) []schema.ThinkingLevelChange {
	if history, ok := thinkingLevelHistoryFromTurns(session.Turns); ok {
		return history
	}
	return session.ThinkingLevelHistory
}

// thinkingLevelHistoryFromTurns derives the in-session history from the folded
// root-assistant observations using the shared accumulation rule. It reports
// false when no root-assistant turn carries evidence, so the caller can fall
// back to the stored history. An empty history with true means the observations
// never changed level.
func thinkingLevelHistoryFromTurns(turns []ingest.Turn) ([]schema.ThinkingLevelChange, bool) {
	var state ingest.ThinkingLevelHistoryState
	found := false
	for _, turn := range turns {
		if turn.Depth != 0 || turn.Role != schema.RoleAssistant {
			continue
		}
		if turn.ThinkingLevel == "" && turn.ThinkingLevelRaw == "" {
			continue
		}
		found = true
		state.Observe(turn.ThinkingLevel, turn.ThinkingLevelRaw)
	}
	return state.History, found
}
