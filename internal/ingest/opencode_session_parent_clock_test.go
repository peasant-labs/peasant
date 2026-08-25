package ingest_test

import (
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

func parentClockAdapter(t testing.TB) *ingest.OpenCodeAdapter {
	t.Helper()
	filesystem := &ingest.OSFileSystem{}
	adapter, err := ingest.NewOpenCodeAdapterWithCandidateProbe(filesystem, testutil.NoGitResolver(), salt.Salt{}, "latest", fixedCandidateEnvironment{}, filesystem, ingest.OpenOpenCodeSQLiteSource, ingest.DefaultOpenCodeSQLiteSourceOptions())
	if err != nil {
		t.Fatalf("construct candidate-capable adapter: %v", err)
	}
	return adapter
}

func discoveryFailedDiagnostic(evidence []ingest.OpenCodeProbeResult, path string) *ingest.OpenCodeProbeDiagnostic {
	for _, result := range evidence {
		if filepath.Clean(result.Candidate.Path) != filepath.Clean(path) {
			continue
		}
		for index := range result.Diagnostics {
			if result.Diagnostics[index].Code == ingest.OpenCodeDiagnosticDiscoveryFailed {
				return &result.Diagnostics[index]
			}
		}
	}
	return nil
}

// TestOpenCodeSessionParentLinkedWithoutClock proves that a session table with
// parent_id but no time_updated still links parents, so the changed clock and
// the parent link are independent columns. It also proves that a session table
// carrying neither column records a diagnostic rather than losing the link
// silently.
func TestOpenCodeSessionParentLinkedWithoutClock(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "session-parent-no-clock")
	root, err := ingest.NewResolvedPath(filepath.Dir(materialized.Path))
	if err != nil {
		t.Fatalf("resolve synthetic OpenCode root: %v", err)
	}
	const child = "ses_3cd91f52effeXd3QAJ54jOyzP1"
	const parent = "ses_3cd91f52effeXd3QAJ54jOyzP2"
	// The absent-clock session table carries id, parent_id, and time_created but
	// no time_updated. Link the child to the parent with a parent_id row.
	withCanonicalConnection(t, materialized.Path, func(connection *sqlite.Conn) error {
		return sqlitex.Execute(connection, `INSERT INTO session(id, parent_id, time_created) VALUES(?1, ?2, ?3)`, &sqlitex.ExecOptions{Args: []any{child, parent, 1000}})
	})

	discovered, err := parentClockAdapter(t).Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}})
	if err != nil {
		t.Fatalf("discover with a clockless session table: %v", err)
	}
	var childSession *ingest.DiscoveredSession
	for index := range discovered {
		if string(discovered[index].SessionID) == child {
			childSession = &discovered[index]
		}
	}
	if childSession == nil {
		t.Fatalf("clockless discovery = %+v, want the child session discovered", discovered)
	}
	if childSession.ParentUUID == nil || string(*childSession.ParentUUID) != parent {
		t.Fatalf("child session parent = %v, want the parent link %q read without a clock column", childSession.ParentUUID, parent)
	}
	if childSession.ModTime.IsZero() {
		t.Fatalf("clockless child freshness is zero; the database and WAL mtime floor should apply")
	}

	// The parent link exists, so no diagnostic is recorded for this database.
	firstAdapter := parentClockAdapter(t)
	if _, discoverErr := firstAdapter.Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}}); discoverErr != nil {
		t.Fatalf("discover for diagnostic check: %v", discoverErr)
	}
	if diagnostic := discoveryFailedDiagnostic(firstAdapter.CandidateEvidence(), materialized.Path); diagnostic != nil {
		t.Fatalf("a session table with parent_id recorded a discovery diagnostic %+v; parent-present tables must not", diagnostic)
	}

	// Drop parent_id so the session table carries neither the parent link nor
	// the clock. Discovery must record a diagnostic rather than fail silently.
	withCanonicalConnection(t, materialized.Path, func(connection *sqlite.Conn) error {
		return sqlitex.Execute(connection, `ALTER TABLE session DROP COLUMN parent_id`, nil)
	})
	secondAdapter := parentClockAdapter(t)
	if _, discoverErr := secondAdapter.Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}}); discoverErr != nil {
		t.Fatalf("discover after dropping parent_id: %v", discoverErr)
	}
	diagnostic := discoveryFailedDiagnostic(secondAdapter.CandidateEvidence(), materialized.Path)
	if diagnostic == nil {
		t.Fatalf("a session table with neither parent_id nor time_updated recorded no diagnostic: evidence=%+v", secondAdapter.CandidateEvidence())
	}
}
