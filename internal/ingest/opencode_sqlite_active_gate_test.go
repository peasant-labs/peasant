package ingest_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// TestOpenCodeActiveGateUsesFileModTime proves the staleness (active) gate reads
// the source file mtime, not the changed clock. A current SQLite database whose
// file mtime is within the staleness threshold, while its row times and session
// clock are older, must classify active because the source is still being
// written, even though its changed clock is old.
func TestOpenCodeActiveGateUsesFileModTime(t *testing.T) {
	const sessionID = "ses_3cd91f52effeXd3QAJ54jOyzv5"
	materialized := testfixture.MaterializeByName(t, "semantic-parity-current")
	databasePath := materialized.Path
	root, err := ingest.NewResolvedPath(filepath.Dir(databasePath))
	if err != nil {
		t.Fatal(err)
	}

	// The file mtime is within the staleness threshold; the row times and the
	// session clock stay in the far past.
	recent := time.Now().Add(-10 * time.Second)
	setDatabaseModTime(t, databasePath, recent)

	adapter := canonicalAdapterFactory(t, mountedCurrentEnvironment{"OPENCODE_DB": databasePath})(&ingest.OSFileSystem{}, testutil.NoGitResolver(), salt.Salt{})
	sessions, err := adapter.Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}})
	if err != nil {
		t.Fatal(err)
	}
	var session ingest.DiscoveredSession
	for _, candidate := range sessions {
		if string(candidate.SessionID) == sessionID {
			session = candidate
		}
	}
	if string(session.SessionID) != sessionID {
		t.Fatalf("session %q was not discovered", sessionID)
	}

	// The changed clock stays old; the active time follows the file mtime.
	if !session.ModTime.Equal(time.UnixMilli(1140)) {
		t.Fatalf("changed clock moved off the row times: ModTime=%s want %s", session.ModTime, time.UnixMilli(1140))
	}
	if session.ActiveModTime.Before(recent.Add(-2*time.Second)) || session.ActiveModTime.After(recent.Add(2*time.Second)) {
		t.Fatalf("active time did not follow the file mtime: ActiveModTime=%s want near %s", session.ActiveModTime, recent)
	}

	ingestedMS := time.Now().UnixMilli()
	location := ingest.SessionLocation{IngestedMs: &ingestedMS, SchemaVersion: int(ingest.CurrentSchemaVersion)}
	if got := ingest.ClassifyAgainstStore(session, location, time.Minute); got != ingest.DiffActive {
		t.Fatalf("still-active source did not classify active on its file mtime: %v", got)
	}
}
