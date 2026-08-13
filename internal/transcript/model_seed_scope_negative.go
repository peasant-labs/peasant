//go:build projection_negative_scope

package transcript

import "github.com/peasant-labs/peasant/internal/ingest"

func sessionModelSeed(session *ingest.Session) string {
	model := session.Model
	foundRoot := false
	for _, turn := range session.Turns {
		if turn.Depth == 0 && turn.Role == ingest.RoleAssistant && turn.ObservedModel != "" && !foundRoot {
			model = turn.ObservedModel.String()
			foundRoot = true
			continue
		}
		if turn.Depth > 0 && foundRoot && turn.ObservedModel != "" {
			model = turn.ObservedModel.String()
		}
	}
	return model
}
