package onboarding

import (
	"fmt"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"time"
)

// SessionListing is one discovered session the picker and later stages consume.
// It keeps discovery details independent from ingest's full session type.
type SessionListing struct {
	Harness     string    `yaml:"harness"`
	ProjectName string    `yaml:"projectName"`
	GitRemote   string    `yaml:"gitRemote"`
	Branch      string    `yaml:"branch"`
	Title       string    `yaml:"title"`
	Agent       string    `yaml:"agent"`
	Date        time.Time `yaml:"date"`
	TurnCount   int       `yaml:"turnCount"`
	SessionID   string    `yaml:"sessionId"`
	SubagentIDs []string  `yaml:"subagentIds"`
	WorkingDir  string    `yaml:"workingDir"`
	// Origin is who drove the session. The empty string is not a menu value:
	// it marks a listing an adapter mined no evidence for, and callers resolve
	// it to sessionorigin.Unknown before relying on it.
	Origin sessionorigin.Origin `yaml:"origin"`
	// Source locates the transcript the harness wrote, so a selection step can
	// preview a session that has not been imported.
	Source SessionSource `yaml:"source"`
}

// SessionSourceOrigin names how a harness stores one session's transcript.
// It is a closed set. It mirrors the origins the ingest indexers dispatch on.
type SessionSourceOrigin string

const (
	// SessionSourceOriginUnset is the zero value. Discovery did not report an
	// origin, so no reader may guess one.
	SessionSourceOriginUnset SessionSourceOrigin = ""

	// SessionSourceOriginFile is a transcript the harness wrote itself.
	SessionSourceOriginFile SessionSourceOrigin = "file"

	// SessionSourceOriginOpenCodeLegacySQLite is a projection of a legacy
	// OpenCode SQLite session.
	SessionSourceOriginOpenCodeLegacySQLite SessionSourceOrigin = "opencode-legacy-sqlite"

	// SessionSourceOriginOpenCodeCurrentSQLite is a projection of a current
	// OpenCode SQLite session.
	SessionSourceOriginOpenCodeCurrentSQLite SessionSourceOrigin = "opencode-current-sqlite"
)

// String returns the wire form of the origin.
func (o SessionSourceOrigin) String() string { return string(o) }

// Validate rejects an origin outside the closed set.
func (o SessionSourceOrigin) Validate() error {
	switch o {
	case SessionSourceOriginFile,
		SessionSourceOriginOpenCodeLegacySQLite,
		SessionSourceOriginOpenCodeCurrentSQLite:
		return nil
	default:
		return fmt.Errorf(
			"read session source origin %q.\n"+
				"what: %q is outside the supported closed set.\n"+
				"why: a reader must know whether the transcript is a file or an OpenCode SQLite projection before it opens it.\n"+
				"where: discovery.SessionSourceOrigin.Validate.\n"+
				"when: a preview or import asked to read the transcript.\n"+
				"means: the transcript was not read and no path was guessed.\n"+
				"fix: record file, opencode-legacy-sqlite, or opencode-current-sqlite, then retry.",
			o, o)
	}
}

// SessionSource locates the transcript the harness wrote for one session.
// Peasant reads it in place. It never copies it and never writes it.
type SessionSource struct {
	Path   string              `yaml:"path"`
	Root   string              `yaml:"root"`
	Origin SessionSourceOrigin `yaml:"origin"`
}

// IsZero reports that discovery recorded no transcript location.
func (s SessionSource) IsZero() bool {
	return s.Path == "" && s.Root == "" && s.Origin == SessionSourceOriginUnset
}
