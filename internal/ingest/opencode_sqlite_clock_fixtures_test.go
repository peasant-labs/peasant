package ingest_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// TestOpenCodeAbsentSessionClockFixtureUsesFloor proves the absent-clock fixture
// leaves the session table without a usable clock, so freshness falls back to
// the database and WAL mtime floor. A row deletion moves the floor and re-ingests
// the session even though no session clock reports the change.
func TestOpenCodeAbsentSessionClockFixtureUsesFloor(t *testing.T) {
	const sessionID = "ses_3cd91f52effeXd3QAJ54jOyzGA"
	materialized := testfixture.MaterializeByName(t, "session-clock-absent-floor")
	databasePath := materialized.Path
	root, err := ingest.NewResolvedPath(filepath.Dir(databasePath))
	if err != nil {
		t.Fatal(err)
	}

	floorBefore := time.UnixMilli(1_600_000_000_000)
	setDatabaseModTime(t, databasePath, floorBefore)
	ingestedMS := floorBefore.Add(30 * time.Second).UnixMilli()
	location := ingest.SessionLocation{IngestedMs: &ingestedMS, SchemaVersion: int(ingest.CurrentSchemaVersion)}

	adapterFactory := canonicalAdapterFactory(t, mountedCurrentEnvironment{"OPENCODE_DB": databasePath})
	discover := func() ingest.DiscoveredSession {
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

	if got := ingest.ClassifyAgainstStore(discover(), location, 0); got != ingest.DiffUnchanged {
		t.Fatalf("clockless session was not unchanged before the deletion: %v", got)
	}
	deleteSyntheticSelectionRow(t, databasePath, "message", "msg_absent_b")
	if got := ingest.ClassifyAgainstStore(discover(), location, 0); got != ingest.DiffUpdated {
		t.Fatalf("clockless session did not re-ingest through the floor after its newest row was deleted: %v", got)
	}
}

// TestOpenCodeLaggingSessionClockFixtureUsesTheClock proves the lagging-clock
// fixture leaves session.time_updated behind the newest row time, and that a
// clock-bearing session uses its session clock as the changed time. The clock is
// the authority for a session that has one, so the row aggregate is not read and
// a clock that lags the newest row time reports the lagging clock. OpenCode moves
// this clock on every content edit, so the clock does not lag a real change.
func TestOpenCodeLaggingSessionClockFixtureUsesTheClock(t *testing.T) {
	const (
		sessionID = "ses_3cd91f52effeXd3QAJ54jOyzGB"
		laggingMS = 1000
	)
	materialized := testfixture.MaterializeByName(t, "session-clock-lagging")
	databasePath := materialized.Path
	root, err := ingest.NewResolvedPath(filepath.Dir(databasePath))
	if err != nil {
		t.Fatal(err)
	}

	if clock := readSessionClock(t, databasePath, sessionID); clock != laggingMS {
		t.Fatalf("lagging fixture session clock = %d, want %d", clock, laggingMS)
	}

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
	if !session.ModTime.Equal(time.UnixMilli(laggingMS)) {
		t.Fatalf("changed time did not follow the session clock: ModTime=%s want %s", session.ModTime, time.UnixMilli(laggingMS))
	}
}

func readSessionClock(t testing.TB, databasePath, sessionID string) int64 {
	t.Helper()
	var clock int64
	withCanonicalConnection(t, databasePath, func(connection *sqlite.Conn) error {
		return sqlitex.Execute(connection, "SELECT time_updated FROM session WHERE id = ?1", &sqlitex.ExecOptions{
			Args: []any{sessionID},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				clock = stmt.ColumnInt64(0)
				return nil
			},
		})
	})
	return clock
}
