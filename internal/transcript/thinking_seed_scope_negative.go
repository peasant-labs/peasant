//go:build projection_negative_scope

package transcript

import (
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// sessionThinkingSeed (negative build) lets every later observation, at any
// depth, replace the seed, so the fixture proves the seed is the earliest root
// observation rather than the latest one.
func sessionThinkingSeed(session *ingest.Session) (schema.ThinkingLevel, schema.ThinkingLevelRaw) {
	level, raw := session.ThinkingLevel, session.ThinkingLevelRaw
	for _, turn := range session.Turns {
		if turn.Role == schema.RoleAssistant && (turn.ThinkingLevel != "" || turn.ThinkingLevelRaw != "") {
			level, raw = turn.ThinkingLevel, turn.ThinkingLevelRaw
		}
	}
	return level, raw
}
