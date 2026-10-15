//go:build !projection_negative_seed && !projection_negative_scope

package transcript

import (
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// sessionThinkingSeed resolves the session thinking-level seed: the earliest
// root assistant turn carrying a level or raw spelling wins; otherwise the
// stored session seed. Later, nested, or most frequent observations never
// replace it.
func sessionThinkingSeed(session *ingest.Session) (schema.ThinkingLevel, schema.ThinkingLevelRaw) {
	for _, turn := range session.Turns {
		if turn.Depth == 0 && turn.Role == schema.RoleAssistant && (turn.ThinkingLevel != "" || turn.ThinkingLevelRaw != "") {
			return turn.ThinkingLevel, turn.ThinkingLevelRaw
		}
	}
	return session.ThinkingLevel, session.ThinkingLevelRaw
}
