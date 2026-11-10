package discovery

import (
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/onboarding"
)

// Session source types are owned by onboarding.
type SessionSourceOrigin = onboarding.SessionSourceOrigin
type SessionSource = onboarding.SessionSource

const (
	SessionSourceOriginUnset                 = onboarding.SessionSourceOriginUnset
	SessionSourceOriginFile                  = onboarding.SessionSourceOriginFile
	SessionSourceOriginOpenCodeLegacySQLite  = onboarding.SessionSourceOriginOpenCodeLegacySQLite
	SessionSourceOriginOpenCodeCurrentSQLite = onboarding.SessionSourceOriginOpenCodeCurrentSQLite
)

// ListingSource records where a discovered session's transcript is, so a later
// step can read that transcript without the discovery result.
func ListingSource(session ingest.DiscoveredSession) SessionSource {
	return SessionSource{
		Path:   string(session.SourcePath),
		Root:   string(session.OriginalRoot),
		Origin: listingSourceOrigin(session.TranscriptOrigin),
	}
}

func listingSourceOrigin(origin ingest.TranscriptOrigin) SessionSourceOrigin {
	switch origin {
	case ingest.TranscriptOriginOpenCodeLegacySQLite:
		return SessionSourceOriginOpenCodeLegacySQLite
	case ingest.TranscriptOriginOpenCodeCurrentSQLite:
		return SessionSourceOriginOpenCodeCurrentSQLite
	default:
		return SessionSourceOriginFile
	}
}
