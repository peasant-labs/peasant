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

func discoverOpenCodeSessionsByID(t *testing.T, fixtureName string) map[string]ingest.DiscoveredSession {
	t.Helper()
	materialized := testfixture.MaterializeByName(t, fixtureName)
	before := testfixture.SnapshotSource(t, materialized)
	t.Cleanup(func() { testfixture.AssertUnchanged(t, materialized, before) })
	root, err := ingest.NewResolvedPath(filepath.Dir(materialized.Path))
	if err != nil {
		t.Fatalf("resolve synthetic OpenCode root: %v", err)
	}
	adapter := ingest.NewOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.NoGitResolver(), salt.Salt{})
	discovered, err := adapter.Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}})
	if err != nil {
		t.Fatalf("run production OpenCode discovery against %q: %v", fixtureName, err)
	}
	byID := make(map[string]ingest.DiscoveredSession, len(discovered))
	for _, session := range discovered {
		byID[string(session.SessionID)] = session
	}
	return byID
}

func TestOpenCodeDiscoveryPrefersNativeSessionAuthority(t *testing.T) {
	byID := discoverOpenCodeSessionsByID(t, "native-v2-preferred")
	if _, ok := byID["ses_3cd91f52effeXd3QAJ54jOyzn1"]; !ok {
		t.Fatalf("native session present in session_v2 was lost through the empty legacy table: %v", byID)
	}
}

// TestOpenCodeDiscoveryAttributesSQLiteSessions proves that discovery sets the
// working directory, title, and creation time on every SQLite-discovered
// OpenCode session from its session row, for both the legacy-only winner and
// the current winner of a hybrid database. Removing the attribution assignments
// in discoverSQLiteCandidate makes this case fail because the fields stay empty.
func TestOpenCodeDiscoveryAttributesSQLiteSessions(t *testing.T) {
	t.Parallel()
	byID := discoverOpenCodeSessionsByID(t, "hybrid-attribution")

	legacyWinner, ok := byID["ses_3cd91f52effeXd3QAJ54jOyzL1"]
	if !ok {
		t.Fatalf("discovered sessions = %v, want the legacy-only winner", byID)
	}
	if legacyWinner.CWD != "/home/dev/peasant-labs/garden" || legacyWinner.Title != "legacy winner attribution" || !legacyWinner.CreatedAt.Equal(time.UnixMilli(3000)) {
		t.Fatalf("legacy winner attribution = cwd %q title %q created %s, want the session-row attribution", legacyWinner.CWD, legacyWinner.Title, legacyWinner.CreatedAt)
	}

	currentWinner, ok := byID["ses_3cd91f52effeXd3QAJ54jOyzL2"]
	if !ok {
		t.Fatalf("discovered sessions = %v, want the current winner", byID)
	}
	if currentWinner.CWD != "/home/dev/peasant-labs/tool" || currentWinner.Title != "current winner attribution" || !currentWinner.CreatedAt.Equal(time.UnixMilli(3010)) {
		t.Fatalf("current winner attribution = cwd %q title %q created %s, want the session-row attribution", currentWinner.CWD, currentWinner.Title, currentWinner.CreatedAt)
	}
}

// TestOpenCodeDiscoveryWithoutAttributionColumnsLeavesFieldsEmpty proves that a
// session table without the attribution columns yields empty working directory
// and title and a zero creation time rather than failing discovery.
func TestOpenCodeDiscoveryWithoutAttributionColumnsLeavesFieldsEmpty(t *testing.T) {
	t.Parallel()
	byID := discoverOpenCodeSessionsByID(t, "current-session-message")
	if len(byID) == 0 {
		t.Fatal("current session-message discovered no sessions")
	}
	for id, session := range byID {
		if session.CWD != "" || session.Title != "" || !session.CreatedAt.IsZero() {
			t.Fatalf("session %q without attribution columns = cwd %q title %q created %s, want empty attribution", id, session.CWD, session.Title, session.CreatedAt)
		}
	}
}
