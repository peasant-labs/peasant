package ingest_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// currentFreshnessFaultSource fails only the current row-freshness read, so a
// test can force the winner's freshness read to fail while discovery succeeds.
type currentFreshnessFaultSource struct {
	ingest.OpenCodeSQLiteSource
}

func (source currentFreshnessFaultSource) CurrentFreshnessBySession(context.Context) (map[string]time.Time, error) {
	return nil, fmt.Errorf("synthetic current freshness read failure")
}

// TestOpenCodeWinnerFreshnessFailureFallsBackToFloor proves a session that has a
// readable representation is never dropped when the winner's freshness read
// fails. The session is present in both JSON and current SQLite, the SQLite
// winner's freshness read is fault-injected, and the session still discovers
// with the database and WAL mtime floor as its freshness.
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
