package ingest_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// TestOpenCodeClockedSessionsReadNoFreshnessAggregate proves freshness is
// clock-first: the per-table row aggregate is read only when a selected session
// on a database has no usable session clock. A database whose sessions all carry
// a clock runs no freshness aggregate at all, so a very large legacy table is
// never scanned for a session that already has a clock. When one session loses
// its clock, the aggregate for the table that session uses runs exactly once.
//
// The two sessions live in one materialized database from the fixture corpus;
// the scenarios differ only by whether one session keeps its clock, so the
// database rows stay in the shared YAML fixture rather than in an inline table.
func TestOpenCodeClockedSessionsReadNoFreshnessAggregate(t *testing.T) {
	const (
		firstSession  = "ses_3cd91f52effeXd3QAJ54jOyzvE"
		secondSession = "ses_3cd91f52effeXd3QAJ54jOyzvF"
	)

	countFreshnessStatements := func(t *testing.T, databasePath string) (current, legacy int) {
		t.Helper()
		root, err := ingest.NewResolvedPath(filepath.Dir(databasePath))
		if err != nil {
			t.Fatal(err)
		}
		recorder := newCanonicalFreshnessRecorder()
		filesystem := &ingest.OSFileSystem{}
		environment := mountedCurrentEnvironment{"OPENCODE_DB": databasePath}
		adapter, err := ingest.NewOpenCodeAdapterWithCandidateProbe(filesystem, testutil.NoGitResolver(), salt.Salt{}, "latest", environment, filesystem, canonicalRecordingOpener(recorder), ingest.DefaultOpenCodeSQLiteSourceOptions())
		if err != nil {
			t.Fatal(err)
		}
		discovered, err := adapter.Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}})
		if err != nil {
			t.Fatal(err)
		}
		if len(discovered) != 2 {
			t.Fatalf("discovery kept %d sessions, want both sessions on the database", len(discovered))
		}
		recorder.mu.Lock()
		defer recorder.mu.Unlock()
		return recorder.currentBatch, recorder.legacyBatch
	}

	t.Run("every session has a clock reads zero freshness statements", func(t *testing.T) {
		materialized := testfixture.MaterializeByName(t, "session-clock-present-and-absent")
		current, legacy := countFreshnessStatements(t, materialized.Path)
		if current != 0 || legacy != 0 {
			t.Fatalf("clocked-only database read freshness statements current=%d legacy=%d, want 0/0", current, legacy)
		}
	})

	t.Run("one clockless session reads the per-table aggregate once", func(t *testing.T) {
		materialized := testfixture.MaterializeByName(t, "session-clock-present-and-absent")
		// The two sessions live in the legacy message and part tables, so the
		// clockless session reads the legacy aggregate once and never the current
		// aggregate. The clock-bearing sibling reads nothing.
		updateSyntheticSessionClock(t, materialized.Path, secondSession, 0)
		current, legacy := countFreshnessStatements(t, materialized.Path)
		if current != 0 || legacy != 1 {
			t.Fatalf("one clockless session read freshness statements current=%d legacy=%d, want 0/1", current, legacy)
		}
	})
}

// legacyFreshnessFailingSource fails the batch legacy freshness read so every
// session on the database falls back to the mtime floor.
type legacyFreshnessFailingSource struct {
	ingest.OpenCodeSQLiteSource
}

func (source legacyFreshnessFailingSource) LegacyFreshnessBySession(context.Context) (map[string]time.Time, error) {
	return nil, errors.New("synthetic legacy freshness read failure")
}

// TestOpenCodeFreshnessDiagnosticAggregatedPerPath proves that a database whose
// row freshness read fails for every clockless session records one freshness
// diagnostic naming the affected sessions, not one diagnostic per session. Both
// sessions lose their session clock so both take the row-aggregate path that the
// failing read exercises.
func TestOpenCodeFreshnessDiagnosticAggregatedPerPath(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "session-clock-present-and-absent")
	updateSyntheticSessionClock(t, materialized.Path, "ses_3cd91f52effeXd3QAJ54jOyzvE", 0)
	updateSyntheticSessionClock(t, materialized.Path, "ses_3cd91f52effeXd3QAJ54jOyzvF", 0)
	root, err := ingest.NewResolvedPath(filepath.Dir(materialized.Path))
	if err != nil {
		t.Fatalf("resolve synthetic OpenCode root: %v", err)
	}
	opener := func(ctx context.Context, path ingest.OpenCodeSQLiteSourcePath, options ingest.OpenCodeSQLiteSourceOptions) (ingest.OpenCodeSQLiteSource, error) {
		source, openErr := ingest.OpenOpenCodeSQLiteSource(ctx, path, options)
		if openErr != nil {
			return nil, openErr
		}
		return legacyFreshnessFailingSource{OpenCodeSQLiteSource: source}, nil
	}
	filesystem := &ingest.OSFileSystem{}
	adapter, err := ingest.NewOpenCodeAdapterWithCandidateProbe(filesystem, testutil.NoGitResolver(), salt.Salt{}, "latest", fixedCandidateEnvironment{}, filesystem, opener, ingest.DefaultOpenCodeSQLiteSourceOptions())
	if err != nil {
		t.Fatalf("construct candidate-capable adapter: %v", err)
	}
	discovered, err := adapter.Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}})
	if err != nil {
		t.Fatalf("discover with a failing freshness read: %v", err)
	}
	if len(discovered) != 2 {
		t.Fatalf("discovery kept %d sessions, want both sessions on the mtime floor", len(discovered))
	}
	freshnessDiagnostics := 0
	var message string
	for _, evidence := range adapter.CandidateEvidence() {
		if filepath.Clean(evidence.Candidate.Path) != filepath.Clean(materialized.Path) {
			continue
		}
		for _, diagnostic := range evidence.Diagnostics {
			if diagnostic.Stage == ingest.OpenCodeProbeFreshness {
				freshnessDiagnostics++
				message = diagnostic.What
			}
		}
	}
	if freshnessDiagnostics != 1 {
		t.Fatalf("freshness diagnostics on the path = %d, want exactly one aggregated diagnostic for both sessions", freshnessDiagnostics)
	}
	if !strings.Contains(message, "ses_3cd91f52effeXd3QAJ54jOyzvE") || !strings.Contains(message, "ses_3cd91f52effeXd3QAJ54jOyzvF") {
		t.Fatalf("aggregated freshness diagnostic %q does not name both affected sessions", message)
	}
}

// currentFreshnessFaultSource fails only the current row-freshness read, so a
// test can force the winner's freshness read to fail while discovery succeeds.
type currentFreshnessFaultSource struct {
	ingest.OpenCodeSQLiteSource
}

func (source currentFreshnessFaultSource) CurrentFreshnessBySession(context.Context) (map[string]time.Time, error) {
	return nil, fmt.Errorf("synthetic current freshness read failure")
}

// TestOpenCodeWinnerFreshnessFailureFallsBackToFloor proves a clockless session
// that has a readable representation is never dropped when the winner's row
// freshness read fails. The session is present in both JSON and current SQLite,
// its session clock is cleared so it needs the row aggregate, the SQLite winner's
// freshness read is fault-injected, and the session still discovers with the
// database and WAL mtime floor as its freshness.
func TestOpenCodeWinnerFreshnessFailureFallsBackToFloor(t *testing.T) {
	const sessionID = "ses_3cd91f52effeXd3QAJ54jOyzv5"
	materialized := testfixture.MaterializeByName(t, "semantic-parity-current")
	databasePath := materialized.Path
	rootPath := filepath.Dir(databasePath)
	root, err := ingest.NewResolvedPath(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	writeOpenCodeJSONSession(t, rootPath, sessionID)

	// A clock-bearing session reads no row aggregate, so it would never reach the
	// fault. Clear the clock so the session takes the row-aggregate path the
	// fault exercises.
	updateSyntheticSessionClock(t, databasePath, sessionID, 0)

	floor := time.UnixMilli(1_600_000_000_000)
	setDatabaseModTime(t, databasePath, floor)

	opener := func(ctx context.Context, path ingest.OpenCodeSQLiteSourcePath, options ingest.OpenCodeSQLiteSourceOptions) (ingest.OpenCodeSQLiteSource, error) {
		source, openErr := ingest.OpenOpenCodeSQLiteSource(ctx, path, options)
		if openErr != nil {
			return nil, openErr
		}
		return currentFreshnessFaultSource{OpenCodeSQLiteSource: source}, nil
	}
	filesystem := &ingest.OSFileSystem{}
	environment := mountedCurrentEnvironment{"OPENCODE_DB": databasePath}
	adapter, err := ingest.NewOpenCodeAdapterWithCandidateProbe(filesystem, testutil.NoGitResolver(), salt.Salt{}, "latest", environment, filesystem, opener, ingest.DefaultOpenCodeSQLiteSourceOptions())
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := adapter.Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}})
	if err != nil {
		t.Fatal(err)
	}
	var session ingest.DiscoveredSession
	found := false
	for _, candidate := range sessions {
		if string(candidate.SessionID) == sessionID {
			session = candidate
			found = true
		}
	}
	if !found {
		t.Fatalf("a session with a readable representation was dropped when the winner freshness read failed")
	}
	if session.TranscriptOrigin != ingest.TranscriptOriginOpenCodeCurrentSQLite {
		t.Fatalf("the current SQLite winner was not retained: origin=%d", session.TranscriptOrigin)
	}
	if !session.ModTime.Equal(floor) {
		t.Fatalf("the winner did not fall back to the mtime floor: ModTime=%s want %s", session.ModTime, floor)
	}
}

func writeOpenCodeJSONSession(t testing.TB, rootPath, sessionID string) {
	t.Helper()
	sessionDir := filepath.Join(rootPath, "storage", "session", "synthetic")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sessionJSON := fmt.Sprintf(`{"id":%q,"version":"synthetic","directory":"/synthetic/fallback","title":%q,"time":{"created":3000,"updated":3010}}`, sessionID, sessionID)
	if err := os.WriteFile(filepath.Join(sessionDir, sessionID+".json"), []byte(sessionJSON), 0o600); err != nil {
		t.Fatal(err)
	}
}

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
