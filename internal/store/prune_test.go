package store_test

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

// ---------------------------------------------------------------------------
// Test fixtures
// ---------------------------------------------------------------------------

const (
	pruneSessionA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	pruneSessionB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	pruneSessionC = "cccccccc-cccc-cccc-cccc-cccccccccccc"
	pruneSessionD = "dddddddd-dddd-dddd-dddd-dddddddddddd"
	pruneProjectH = "1111111111111111111111111111111111111111111111111111111111111111"
	pruneProjectI = "2222222222222222222222222222222222222222222222222222222222222222"
	pruneHostSlug = "github.com-test-prune"
)

// seedPruneSession inserts a session with minimal data for prune tests.
func seedPruneSession(t *testing.T, s *store.Store, sessionID, projectHash string, provider defaults.Harness, startMs int64) {
	t.Helper()
	ingested := startMs + 120000
	entry := ingest.StoreEntry{
		Metadata: &schema.UnifiedMetadata{
			SchemaVersion: ingest.CurrentSchemaVersion,
			SessionID:     schema.SessionID(sessionID),
			ModelHarness:  provider,
			Model:         schema.ModelID("claude-opus-4-6"),
			HostSlug:      schema.HostSlug(pruneHostSlug),
			Project: schema.ProjectContext{
				Hash:     schema.ProjectHash(projectHash),
				Name:     "test-project",
				FilePath: "/test/project",
			},
			Timestamp: schema.TimestampInfo{
				Start:    startMs,
				End:      startMs + 60000,
				Ingested: &ingested,
			},
			Source: schema.SourceInfo{
				FilePath: "/test/session.jsonl",
				Format:   schema.SourceFormatJSONL,
			},
		},
	}
	if err := s.InsertSessions(context.Background(), []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("seedPruneSession(%s): %v", sessionID, err)
	}
}

// seedPruneEntries inserts session_entries for a session.
func seedPruneEntries(t *testing.T, s *store.Store, sessionID string, count int) {
	t.Helper()
	entries := make([]schema.SessionEntry, count)
	for i := range entries {
		entries[i] = schema.SessionEntry{
			SessionID:  schema.SessionID(sessionID),
			EntryIndex: i,
			Harness:    ingest.HarnessClaudeCode,
			EntryType:  schema.EntryTypeText,
			Role:       schema.RoleAssistant,
		}
	}
	if err := s.IndexSessionEntries(context.Background(), schema.SessionID(sessionID), entries); err != nil {
		t.Fatalf("seedPruneEntries(%s, %d): %v", sessionID, count, err)
	}
}

// ---------------------------------------------------------------------------
// QueryPrunableSessions tests
// ---------------------------------------------------------------------------

func TestStore_QueryPrunableSessions_All(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	seedPruneSession(t, s, pruneSessionA, pruneProjectH, defaults.HarnessClaudeCode, 1700000000000)
	seedPruneSession(t, s, pruneSessionB, pruneProjectI, defaults.HarnessOpenCode, 1700100000000)

	rows, err := s.QueryPrunableSessions(ctx, ingest.PruneFilter{All: true})
	if err != nil {
		t.Fatalf("QueryPrunableSessions: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(rows))
	}
}

func TestStore_QueryPrunableSessions_ByProvider(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	seedPruneSession(t, s, pruneSessionA, pruneProjectH, defaults.HarnessClaudeCode, 1700000000000)
	seedPruneSession(t, s, pruneSessionB, pruneProjectI, defaults.HarnessOpenCode, 1700100000000)

	provider := ingest.HarnessClaudeCode
	rows, err := s.QueryPrunableSessions(ctx, ingest.PruneFilter{Harness: &provider})
	if err != nil {
		t.Fatalf("QueryPrunableSessions: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 session, got %d", len(rows))
	}
	if rows[0].SessionID != schema.SessionID(pruneSessionA) {
		t.Errorf("expected session A, got %s", rows[0].SessionID)
	}
}

func TestStore_QueryPrunableSessions_ByDateRange(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	seedPruneSession(t, s, pruneSessionA, pruneProjectH, defaults.HarnessClaudeCode, 1700000000000)
	seedPruneSession(t, s, pruneSessionB, pruneProjectI, defaults.HarnessClaudeCode, 1700100000000)

	before := int64(1700050000000)
	rows, err := s.QueryPrunableSessions(ctx, ingest.PruneFilter{Before: &before})
	if err != nil {
		t.Fatalf("QueryPrunableSessions: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 session, got %d", len(rows))
	}
	if rows[0].SessionID != schema.SessionID(pruneSessionA) {
		t.Errorf("expected session A, got %s", rows[0].SessionID)
	}
}

func TestStore_QueryPrunableSessions_BySessionID(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	seedPruneSession(t, s, pruneSessionA, pruneProjectH, defaults.HarnessClaudeCode, 1700000000000)
	seedPruneSession(t, s, pruneSessionB, pruneProjectI, defaults.HarnessClaudeCode, 1700100000000)

	rows, err := s.QueryPrunableSessions(ctx, ingest.PruneFilter{
		SessionIDs: []ingest.SessionID{schema.SessionID(pruneSessionB)},
	})
	if err != nil {
		t.Fatalf("QueryPrunableSessions: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 session, got %d", len(rows))
	}
	if rows[0].SessionID != schema.SessionID(pruneSessionB) {
		t.Errorf("expected session B, got %s", rows[0].SessionID)
	}
}

func TestStore_QueryPrunableSessions_ByProjectHash(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	seedPruneSession(t, s, pruneSessionA, pruneProjectH, defaults.HarnessClaudeCode, 1700000000000)
	seedPruneSession(t, s, pruneSessionB, pruneProjectI, defaults.HarnessClaudeCode, 1700100000000)

	rows, err := s.QueryPrunableSessions(ctx, ingest.PruneFilter{
		ProjectHash: ptrStr(pruneProjectI),
	})
	if err != nil {
		t.Fatalf("QueryPrunableSessions: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 session, got %d", len(rows))
	}
	if rows[0].SessionID != schema.SessionID(pruneSessionB) {
		t.Errorf("expected session B, got %s", rows[0].SessionID)
	}
}

func TestStore_QueryPrunableSessions_CombinedFilters(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	seedPruneSession(t, s, pruneSessionA, pruneProjectH, defaults.HarnessClaudeCode, 1700000000000)
	seedPruneSession(t, s, pruneSessionB, pruneProjectH, defaults.HarnessOpenCode, 1700100000000)
	seedPruneSession(t, s, pruneSessionC, pruneProjectH, defaults.HarnessClaudeCode, 1700200000000)

	// Filter by provider=claude AND before the third session.
	provider := ingest.HarnessClaudeCode
	before := int64(1700150000000)
	rows, err := s.QueryPrunableSessions(ctx, ingest.PruneFilter{
		Harness: &provider,
		Before:  &before,
	})
	if err != nil {
		t.Fatalf("QueryPrunableSessions: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 session, got %d", len(rows))
	}
	if rows[0].SessionID != schema.SessionID(pruneSessionA) {
		t.Errorf("expected session A, got %s", rows[0].SessionID)
	}
}

func TestStore_QueryPrunableSessions_NoMatch(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	seedPruneSession(t, s, pruneSessionA, pruneProjectH, defaults.HarnessClaudeCode, 1700000000000)

	provider := ingest.HarnessOpenCode
	rows, err := s.QueryPrunableSessions(ctx, ingest.PruneFilter{Harness: &provider})
	if err != nil {
		t.Fatalf("QueryPrunableSessions: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected 0 sessions, got %d", len(rows))
	}
}

func TestStore_QueryPrunableSessions_ZeroTimestampExcludedFromTimeFilter(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	// Session D has start_ms=0 (unknown timestamp).
	seedPruneSession(t, s, pruneSessionD, pruneProjectH, defaults.HarnessClaudeCode, 0)
	// Session A has a real timestamp.
	seedPruneSession(t, s, pruneSessionA, pruneProjectH, defaults.HarnessClaudeCode, 1700000000000)

	// --before filter: should return only session A, not the zero-timestamp session.
	before := int64(1800000000000)
	rows, err := s.QueryPrunableSessions(ctx, ingest.PruneFilter{Before: &before})
	if err != nil {
		t.Fatalf("QueryPrunableSessions(Before): %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("Before filter: expected 1 session, got %d", len(rows))
	}
	if rows[0].SessionID != schema.SessionID(pruneSessionA) {
		t.Errorf("Before filter: expected session A, got %s", rows[0].SessionID)
	}

	// --after filter: should return only session A, not the zero-timestamp session.
	after := int64(1600000000000)
	rows, err = s.QueryPrunableSessions(ctx, ingest.PruneFilter{After: &after})
	if err != nil {
		t.Fatalf("QueryPrunableSessions(After): %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("After filter: expected 1 session, got %d", len(rows))
	}
	if rows[0].SessionID != schema.SessionID(pruneSessionA) {
		t.Errorf("After filter: expected session A, got %s", rows[0].SessionID)
	}

	// No time filter (All=true): zero-timestamp session IS returned.
	rows, err = s.QueryPrunableSessions(ctx, ingest.PruneFilter{All: true})
	if err != nil {
		t.Fatalf("QueryPrunableSessions(All): %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("All filter: expected 2 sessions, got %d", len(rows))
	}

	// No time filter (empty filter, no All): zero-timestamp session IS returned.
	rows, err = s.QueryPrunableSessions(ctx, ingest.PruneFilter{})
	if err != nil {
		t.Fatalf("QueryPrunableSessions(empty): %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("Empty filter: expected 2 sessions, got %d", len(rows))
	}
}

// ---------------------------------------------------------------------------
// PruneSessions tests
// ---------------------------------------------------------------------------

func TestStore_PruneSessions_CascadeDelete(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	// Seed session A with entries.
	seedPruneSession(t, s, pruneSessionA, pruneProjectH, defaults.HarnessClaudeCode, 1700000000000)
	seedPruneEntries(t, s, pruneSessionA, 2)

	// Seed session B (should remain after pruning A).
	seedPruneSession(t, s, pruneSessionB, pruneProjectI, defaults.HarnessClaudeCode, 1700100000000)
	seedPruneEntries(t, s, pruneSessionB, 1)

	// Prune session A.
	result, err := s.PruneSessions(ctx, []ingest.SessionID{schema.SessionID(pruneSessionA)})
	if err != nil {
		t.Fatalf("PruneSessions: %v", err)
	}
	if result.Deleted != 1 {
		t.Errorf("expected 1 deleted, got %d", result.Deleted)
	}

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// Verify session A is gone from all tables.
	if c := queryInt(t, conn, "SELECT COUNT(*) FROM sessions WHERE session_id = ?", pruneSessionA); c != 0 {
		t.Errorf("sessions: expected 0 rows for A, got %d", c)
	}
	if c := queryInt(t, conn, "SELECT COUNT(*) FROM session_metrics WHERE session_id = ?", pruneSessionA); c != 0 {
		t.Errorf("session_metrics: expected 0 rows for A, got %d", c)
	}
	if c := queryInt(t, conn, "SELECT COUNT(*) FROM session_entries WHERE session_id = ?", pruneSessionA); c != 0 {
		t.Errorf("session_entries: expected 0 rows for A, got %d", c)
	}

	// Verify session B is intact.
	if c := queryInt(t, conn, "SELECT COUNT(*) FROM sessions WHERE session_id = ?", pruneSessionB); c != 1 {
		t.Errorf("sessions: expected 1 row for B, got %d", c)
	}
	if c := queryInt(t, conn, "SELECT COUNT(*) FROM session_metrics WHERE session_id = ?", pruneSessionB); c != 1 {
		t.Errorf("session_metrics: expected 1 row for B, got %d", c)
	}
	if c := queryInt(t, conn, "SELECT COUNT(*) FROM session_entries WHERE session_id = ?", pruneSessionB); c != 1 {
		t.Errorf("session_entries: expected 1 row for B, got %d", c)
	}
}

func TestStore_PruneSessions_Empty(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	result, err := s.PruneSessions(ctx, nil)
	if err != nil {
		t.Fatalf("PruneSessions: %v", err)
	}
	if result.Deleted != 0 {
		t.Errorf("expected 0 deleted, got %d", result.Deleted)
	}
}

func TestStore_PruneSessions_WithCommits(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	seedPruneSession(t, s, pruneSessionA, pruneProjectH, defaults.HarnessClaudeCode, 1700000000000)

	// Insert a commit row.
	commits := []ingest.CommitInfo{
		{Hash: "abc123def456", AuthorName: "test", AuthorEmail: "test@test.com", Message: "test commit", CommitTime: 1700000000, AuthorTime: 1700000000},
	}
	if err := s.UpsertSessionCommits(ctx, schema.SessionID(pruneSessionA), commits); err != nil {
		t.Fatalf("UpsertSessionCommits: %v", err)
	}

	// Verify commit exists.
	conn := takeConn(t, s.PoolForTest())
	commitCount := queryInt(t, conn, "SELECT COUNT(*) FROM session_commits WHERE session_id = ?", pruneSessionA)
	s.PoolForTest().Put(conn)
	if commitCount != 1 {
		t.Fatalf("expected 1 commit, got %d", commitCount)
	}

	// Prune.
	result, err := s.PruneSessions(ctx, []ingest.SessionID{schema.SessionID(pruneSessionA)})
	if err != nil {
		t.Fatalf("PruneSessions: %v", err)
	}
	if result.Deleted != 1 {
		t.Errorf("expected 1 deleted, got %d", result.Deleted)
	}

	// Verify commit is gone.
	conn = takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)
	commitCount = queryInt(t, conn, "SELECT COUNT(*) FROM session_commits WHERE session_id = ?", pruneSessionA)
	if commitCount != 0 {
		t.Errorf("expected 0 commits after prune, got %d", commitCount)
	}
}

func TestStore_PruneSessions_MultipleAtOnce(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	seedPruneSession(t, s, pruneSessionA, pruneProjectH, defaults.HarnessClaudeCode, 1700000000000)
	seedPruneSession(t, s, pruneSessionB, pruneProjectI, defaults.HarnessClaudeCode, 1700100000000)
	seedPruneSession(t, s, pruneSessionC, pruneProjectH, defaults.HarnessClaudeCode, 1700200000000)

	// Prune A and B, keep C.
	result, err := s.PruneSessions(ctx, []ingest.SessionID{
		schema.SessionID(pruneSessionA),
		schema.SessionID(pruneSessionB),
	})
	if err != nil {
		t.Fatalf("PruneSessions: %v", err)
	}
	if result.Deleted != 2 {
		t.Errorf("expected 2 deleted, got %d", result.Deleted)
	}

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	if c := queryInt(t, conn, "SELECT COUNT(*) FROM sessions WHERE session_id = ?", pruneSessionA); c != 0 {
		t.Errorf("sessions: expected 0 for A, got %d", c)
	}
	if c := queryInt(t, conn, "SELECT COUNT(*) FROM sessions WHERE session_id = ?", pruneSessionB); c != 0 {
		t.Errorf("sessions: expected 0 for B, got %d", c)
	}
	if c := queryInt(t, conn, "SELECT COUNT(*) FROM sessions WHERE session_id = ?", pruneSessionC); c != 1 {
		t.Errorf("sessions: expected 1 for C, got %d", c)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func ptrStr(s string) *string { return &s }
