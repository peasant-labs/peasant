package ftue

import "github.com/peasant-labs/peasant/internal/discovery"

// SessionSourceOrigin names how a harness stores one session's transcript.
type SessionSourceOrigin = discovery.SessionSourceOrigin

const (
	SessionSourceOriginUnset                 = discovery.SessionSourceOriginUnset
	SessionSourceOriginFile                  = discovery.SessionSourceOriginFile
	SessionSourceOriginOpenCodeLegacySQLite  = discovery.SessionSourceOriginOpenCodeLegacySQLite
	SessionSourceOriginOpenCodeCurrentSQLite = discovery.SessionSourceOriginOpenCodeCurrentSQLite
)

// SessionSource locates the transcript the harness wrote for one session.
type SessionSource = discovery.SessionSource
