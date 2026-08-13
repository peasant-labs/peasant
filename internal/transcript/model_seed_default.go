//go:build !projection_negative_seed && !projection_negative_scope

package transcript

import (
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

func sessionModelSeed(session *ingest.Session) string {
	for _, turn := range session.Turns {
		if turn.Depth == 0 && turn.Role == schema.RoleAssistant {
			observation := turn.ObservedModel
			if observation != "" {
				return observation.String()
			}
		}
	}
	return session.Model
}
