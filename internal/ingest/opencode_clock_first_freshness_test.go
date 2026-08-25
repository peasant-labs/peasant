package ingest_test

import (
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
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
