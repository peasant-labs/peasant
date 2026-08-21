package ingest_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// TestOpenCodeSessionClockIsPerSession proves the changed clock is one optional
// value per session, not one flag for the whole database. Two sessions share
// one database: one keeps its session clock, the other has a zero clock. When
// the newest row of the clockless session is deleted, only the database and WAL
// mtime floor can report the change, so that session re-ingests while the
// session that keeps its clock stays unchanged.
func TestOpenCodeSessionClockIsPerSession(t *testing.T) {
	const (
		clockPresent = "ses_3cd91f52effeXd3QAJ54jOyzvE"
		clockAbsent  = "ses_3cd91f52effeXd3QAJ54jOyzvF"
	)
	materialized := testfixture.MaterializeByName(t, "session-clock-present-and-absent")
	databasePath := materialized.Path
	rootPath := filepath.Dir(databasePath)
	root, err := ingest.NewResolvedPath(rootPath)
	if err != nil {
		t.Fatal(err)
	}

	// The clockless session keeps its rows but loses its usable session clock,
	// so only the mtime floor can report a later change.
	updateSyntheticSessionClock(t, databasePath, clockAbsent, 0)

	// Anchor the floor below the recorded ingest time so both sessions start
	// unchanged, then let the deletion push the floor past it.
	floorBefore := time.UnixMilli(1_600_000_000_000)
	setDatabaseModTime(t, databasePath, floorBefore)
	ingestedMS := floorBefore.Add(30 * time.Second).UnixMilli()
	location := ingest.SessionLocation{IngestedMs: &ingestedMS, SchemaVersion: int(ingest.CurrentSchemaVersion)}

	adapterFactory := canonicalAdapterFactory(t, mountedCurrentEnvironment{"OPENCODE_DB": databasePath})
	discover := func(sessionID string) ingest.DiscoveredSession {
		adapter := adapterFactory(&ingest.OSFileSystem{}, testutil.NoGitResolver(), salt.Salt{})
		sessions, discoverErr := adapter.Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}})
		if discoverErr != nil {
			t.Fatal(discoverErr)
		}
		for _, session := range sessions {
			if string(session.SessionID) == sessionID {
				return session
			}
		}
		t.Fatalf("session %q was not discovered", sessionID)
		return ingest.DiscoveredSession{}
	}

	if got := ingest.ClassifyAgainstStore(discover(clockPresent), location, 0); got != ingest.DiffUnchanged {
		t.Fatalf("clock-present session was not unchanged before the deletion: %v", got)
	}
	if got := ingest.ClassifyAgainstStore(discover(clockAbsent), location, 0); got != ingest.DiffUnchanged {
		t.Fatalf("clockless session was not unchanged before the deletion: %v", got)
	}

	// Delete the newest row of the clockless session. The surviving rows' own
	// times go down, so only the floor moves.
	deleteSyntheticSelectionRow(t, databasePath, "message", "msg_absent_new")

	if got := ingest.ClassifyAgainstStore(discover(clockAbsent), location, 0); got != ingest.DiffUpdated {
		t.Fatalf("clockless session did not re-ingest after its newest row was deleted: %v", got)
	}
	if got := ingest.ClassifyAgainstStore(discover(clockPresent), location, 0); got != ingest.DiffUnchanged {
		t.Fatalf("clock-present session changed when a sibling session lost a row: %v", got)
	}
}

func setDatabaseModTime(t testing.TB, databasePath string, modified time.Time) {
	t.Helper()
	if err := os.Chtimes(databasePath, modified, modified); err != nil {
		t.Fatalf("set synthetic database mtime: %v", err)
	}
}
