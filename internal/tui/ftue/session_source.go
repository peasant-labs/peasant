package ftue

import "fmt"

// SessionSourceOrigin names how a harness stores one session's transcript.
//
// It is a closed set. It mirrors the origins the ingest indexers dispatch on,
// but it stays a value of this package so the wizard keeps its own vocabulary
// and does not import the ingest package.
type SessionSourceOrigin string

const (
	// SessionSourceOriginUnset is the zero value. Discovery did not report an
	// origin for the session, so no reader may guess one.
	SessionSourceOriginUnset SessionSourceOrigin = ""

	// SessionSourceOriginFile is a transcript the harness wrote itself, either
	// as one file or as a directory of files.
	SessionSourceOriginFile SessionSourceOrigin = "file"

	// SessionSourceOriginOpenCodeLegacySQLite is a Peasant projection of a
	// legacy OpenCode SQLite session.
	SessionSourceOriginOpenCodeLegacySQLite SessionSourceOrigin = "opencode-legacy-sqlite"

	// SessionSourceOriginOpenCodeCurrentSQLite is a Peasant projection of a
	// current OpenCode SQLite session.
	SessionSourceOriginOpenCodeCurrentSQLite SessionSourceOrigin = "opencode-current-sqlite"
)

// String returns the wire form of the origin.
func (o SessionSourceOrigin) String() string { return string(o) }

// Validate rejects an origin outside the closed set. A reader calls it before
// it reads a transcript, so an unknown origin fails closed instead of taking a
// default path that parses the wrong shape.
func (o SessionSourceOrigin) Validate() error {
	switch o {
	case SessionSourceOriginFile,
		SessionSourceOriginOpenCodeLegacySQLite,
		SessionSourceOriginOpenCodeCurrentSQLite:
		return nil
	default:
		return fmt.Errorf("session source origin %q is outside the supported closed set", string(o))
	}
}

// SessionSource locates the transcript the harness wrote for one session.
//
// Discovery finds this transcript before Peasant imports anything, so the
// selection step can read it to preview a session the local store does not
// hold. Peasant reads the transcript in place. It never copies it and never
// writes it.
type SessionSource struct {
	// Path is the transcript file the indexer reads for a file source. It is
	// also the projection file for an OpenCode SQLite origin.
	Path string `yaml:"path"`

	// Root is the harness directory that holds the session data when the
	// transcript is a tree of files instead of one file.
	Root string `yaml:"root"`

	// Origin tells the reader which shape the transcript has.
	Origin SessionSourceOrigin `yaml:"origin"`
}

// IsZero reports that discovery recorded no transcript location for the
// session. A reader must then show the session as not imported instead of
// reading a path it does not have.
func (s SessionSource) IsZero() bool {
	return s.Path == "" && s.Root == "" && s.Origin == SessionSourceOriginUnset
}
