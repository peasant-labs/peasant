//go:build projection_negative_seed

package transcript

import "github.com/peasant-labs/peasant/internal/ingest"

func sessionModelSeed(session *ingest.Session) string {
	return session.Model
}
