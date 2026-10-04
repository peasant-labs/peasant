//go:build projection_negative_seed

package transcript

import (
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// sessionThinkingSeed (negative build) ignores turn observations and returns
// only the stored seed, so the fixture proves observations beat stored metadata.
func sessionThinkingSeed(session *ingest.Session) (schema.ThinkingLevel, schema.ThinkingLevelRaw) {
	return session.ThinkingLevel, session.ThinkingLevelRaw
}
