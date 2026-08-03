package store_test

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// cleanupSessionID constants for orphan tests (distinct from prune_test.go constants).
const (
	cleanupSessionA = "a1a1a1a1-a1a1-a1a1-a1a1-a1a1a1a1a1a1"
	cleanupSessionB = "b2b2b2b2-b2b2-b2b2-b2b2-b2b2b2b2b2b2"
	cleanupSessionC = "c3c3c3c3-c3c3-c3c3-c3c3-c3c3c3c3c3c3"
	// project hash: 64 hex chars
	cleanupProjectX = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	cleanupProjectY = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// seedCleanupSession inserts a session with a specific project hash for orphan tests.
func seedCleanupSession(t *testing.T, s *store.Store, sessionID, projectHash string) {
	t.Helper()
	startMs := int64(1700000000000)
	ingested := startMs + 60000
	entry := ingest.StoreEntry{
		Metadata: &schema.UnifiedMetadata{
			SchemaVersion: ingest.CurrentSchemaVersion,
			SessionID:     schema.SessionID(sessionID),
			ModelHarness:  defaults.HarnessClaudeCode,
			Model:         schema.ModelID("claude-opus-4-6"),
			HostSlug:      schema.HostSlug("github.com--cleanup--test"),
			Project: schema.ProjectContext{
				Hash:     schema.ProjectHash(projectHash),
				Name:     "cleanup-test",
				FilePath: "/cleanup/test",
			},
			Timestamp: schema.TimestampInfo{
				Start:    startMs,
				End:      startMs + 30000,
				Ingested: &ingested,
			},
			Source: schema.SourceInfo{
				FilePath: "/cleanup/session.jsonl",
				Format:   schema.SourceFormatJSONL,
			},
		},
	}
	if err := s.InsertSessions(context.Background(), []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("seedCleanupSession(%s): %v", sessionID, err)
	}
}

// countCleanupProjectRows returns the number of rows in the projects table for a given hash.
func countCleanupProjectRows(t *testing.T, s *store.Store, projectHash string) int {
	t.Helper()
	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)
	var count int
	if err := sqlitex.ExecuteTransient(conn,
		"SELECT COUNT(*) FROM projects WHERE project_hash = ?",
		&sqlitex.ExecOptions{
			Args:       []any{projectHash},
			ResultFunc: func(stmt *sqlite.Stmt) error { count = stmt.ColumnInt(0); return nil },
		},
	); err != nil {
		t.Fatalf("countProjectRows(%s): %v", projectHash, err)
	}
	return count
}

// TestCleanupOrphanProjects_RemovesOrphanProject verifies that project rows with no
// remaining sessions are deleted by CleanupOrphanProjects.
func TestCleanupOrphanProjects_RemovesOrphanProject(t *testing.T) {
	t.Parallel()
	s := storetest.Open(t)
	ctx := context.Background()

	// Insert two sessions belonging to two different projects.
	seedCleanupSession(t, s, cleanupSessionA, cleanupProjectX)
	seedCleanupSession(t, s, cleanupSessionB, cleanupProjectY)

	// Verify both projects are present.
	if countCleanupProjectRows(t, s, cleanupProjectX) != 1 {
		t.Fatalf("setup: project X not found before prune")
	}
	if countCleanupProjectRows(t, s, cleanupProjectY) != 1 {
		t.Fatalf("setup: project Y not found before prune")
	}

	// Prune session A (project X) via PruneSessions so sessions table is empty for X.
	if _, err := s.PruneSessions(ctx, []ingest.SessionID{schema.SessionID(cleanupSessionA)}); err != nil {
		t.Fatalf("PruneSessions: %v", err)
	}

	// Project X now has no sessions; project Y still has session B.
	// Run CleanupOrphanProjects.
	if err := s.CleanupOrphanProjects(ctx); err != nil {
		t.Fatalf("CleanupOrphanProjects: %v", err)
	}

	// Project X should be gone.
	if countCleanupProjectRows(t, s, cleanupProjectX) != 0 {
		t.Errorf("orphan project X still present after cleanup")
	}
	// Project Y should remain.
	if countCleanupProjectRows(t, s, cleanupProjectY) != 1 {
		t.Errorf("active project Y was incorrectly removed by cleanup")
	}
}

// TestCleanupOrphanProjects_NoOrphans verifies that CleanupOrphanProjects is a no-op
// when all projects have at least one session.
func TestCleanupOrphanProjects_NoOrphans(t *testing.T) {
	t.Parallel()
	s := storetest.Open(t)
	ctx := context.Background()

	seedCleanupSession(t, s, cleanupSessionC, cleanupProjectX)

	if err := s.CleanupOrphanProjects(ctx); err != nil {
		t.Fatalf("CleanupOrphanProjects on non-empty store: %v", err)
	}

	// Project should still be present.
	if countCleanupProjectRows(t, s, cleanupProjectX) != 1 {
		t.Errorf("active project was incorrectly removed by cleanup")
	}
}

// TestCleanupOrphanProjects_EmptyDB verifies that CleanupOrphanProjects on an
// empty database returns nil (no-op, no panic).
func TestCleanupOrphanProjects_EmptyDB(t *testing.T) {
	t.Parallel()
	s := storetest.Open(t)
	ctx := context.Background()

	if err := s.CleanupOrphanProjects(ctx); err != nil {
		t.Errorf("CleanupOrphanProjects on empty DB: %v", err)
	}
}
