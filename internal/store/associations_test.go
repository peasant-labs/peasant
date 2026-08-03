package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

const associationLifecycleSessionID = "40000000-0000-0000-0000-000000000001"

// TestSessionCommitAssociationLifecycle exercises the production ingest store
// path. Current commit rows may be replaced, but the producer-owned association
// ledger is replay-safe and retains each original observed relationship.
func TestSessionCommitAssociationLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "associations.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	entry := makeStoreEntry(t, associationLifecycleSessionID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "github.com--peasant-labs--peasant", defaults.HarnessClaudeCode, 1000, 0, 0)
	if err := s.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}

	sessionID := ingest.SessionID(associationLifecycleSessionID)
	originalCommits := []ingest.CommitInfo{
		{Hash: "association-hash-a", Message: "original A", AuthorTime: 1700000000000},
		{Hash: "association-hash-b", Message: "original B", AuthorTime: 1700000001000},
	}
	if err := s.UpsertSessionCommits(ctx, sessionID, originalCommits); err != nil {
		t.Fatalf("first UpsertSessionCommits: %v", err)
	}

	first, err := s.ListCurrentSessionCommitAssociations(ctx, sessionID)
	if err != nil {
		t.Fatalf("first ListCurrentSessionCommitAssociations: %v", err)
	}
	if len(first) != len(originalCommits) {
		t.Fatalf("first current associations = %d, want %d", len(first), len(originalCommits))
	}
	firstIDs := make(map[string]schema.AssociationID, len(first))
	for _, association := range first {
		if err := association.ID.Validate(); err != nil {
			t.Errorf("allocated association ID %q: %v", association.ID, err)
		}
		firstIDs[association.ObservedCommitHash] = association.ID
	}

	// Replaying the same observed relationships must reuse their original IDs,
	// even if mutable commit metadata later differs.
	replayedCommits := []ingest.CommitInfo{
		{Hash: "association-hash-a", Message: "changed metadata", AuthorTime: 1700000100000},
		{Hash: "association-hash-b", Message: "changed metadata", AuthorTime: 1700000101000},
	}
	if err := s.UpsertSessionCommits(ctx, sessionID, replayedCommits); err != nil {
		t.Fatalf("replay UpsertSessionCommits: %v", err)
	}
	replayed, err := s.ListCurrentSessionCommitAssociations(ctx, sessionID)
	if err != nil {
		t.Fatalf("replay ListCurrentSessionCommitAssociations: %v", err)
	}
	for _, association := range replayed {
		if association.ID != firstIDs[association.ObservedCommitHash] {
			t.Errorf("replayed association for %q = %q, want original %q", association.ObservedCommitHash, association.ID, firstIDs[association.ObservedCommitHash])
		}
	}

	// A supplied second ID for the same relationship is an alias attempt, while
	// reusing an existing ID for another relationship is a rebind attempt.
	secondID, err := schema.NewAssociationID("assoc-" + uuid.NewString())
	if err != nil {
		t.Fatalf("new supplied association ID: %v", err)
	}
	if _, err := s.lookupOrCreateSessionCommitAssociation(ctx, associationLookupRequest{
		ID:                 &secondID,
		SessionID:          schema.SessionID(sessionID),
		ObservedCommitHash: "association-hash-a",
	}); !errors.Is(err, errAssociationAlias) {
		t.Errorf("second ID for same relationship = %v, want ErrAssociationAlias", err)
	}
	firstAssociationID := firstIDs["association-hash-a"]
	if _, err := s.lookupOrCreateSessionCommitAssociation(ctx, associationLookupRequest{
		ID:                 &firstAssociationID,
		SessionID:          schema.SessionID(sessionID),
		ObservedCommitHash: "association-hash-c",
	}); !errors.Is(err, errAssociationRebind) {
		t.Errorf("reused ID for another relationship = %v, want ErrAssociationRebind", err)
	}

	// A failed batch must retain the old current projection and roll back an
	// earlier association allocation from that same transaction.
	if err := s.UpsertSessionCommits(ctx, sessionID, []ingest.CommitInfo{
		{Hash: "association-hash-c", Message: "must roll back"},
		{Hash: "", Message: "invalid observed hash"},
	}); err == nil {
		t.Fatal("UpsertSessionCommits with empty observed hash succeeded, want error")
	}
	currentAfterFailure, err := s.ListCurrentSessionCommitAssociations(ctx, sessionID)
	if err != nil {
		t.Fatalf("current associations after failed upsert: %v", err)
	}
	if len(currentAfterFailure) != len(firstIDs) {
		t.Fatalf("current associations after failed upsert = %d, want %d", len(currentAfterFailure), len(firstIDs))
	}
	for _, association := range currentAfterFailure {
		if association.ID != firstIDs[association.ObservedCommitHash] {
			t.Errorf("failed upsert changed current association for %q: got %q, want %q", association.ObservedCommitHash, association.ID, firstIDs[association.ObservedCommitHash])
		}
	}

	// Re-ingest can remove a current relation, but it must not erase the durable
	// historical observation. Publish sees only the new current relationship.
	if err := s.UpsertSessionCommits(ctx, sessionID, []ingest.CommitInfo{{Hash: "association-hash-c", Message: "new current"}}); err != nil {
		t.Fatalf("replacement UpsertSessionCommits: %v", err)
	}
	current, err := s.ListCurrentSessionCommitAssociations(ctx, sessionID)
	if err != nil {
		t.Fatalf("replacement ListCurrentSessionCommitAssociations: %v", err)
	}
	if len(current) != 1 || current[0].ObservedCommitHash != "association-hash-c" {
		t.Errorf("current associations after replacement = %+v, want only association-hash-c", current)
	}

	conn, err := s.pool.Take(ctx)
	if err != nil {
		t.Fatalf("take connection: %v", err)
	}
	defer s.pool.Put(conn)
	var count int
	if err := sqlitex.ExecuteTransient(conn, `SELECT COUNT(*) FROM session_commit_associations WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{associationLifecycleSessionID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			count = stmt.ColumnInt(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("count durable associations: %v", err)
	}
	if got := count; got != 3 {
		t.Errorf("durable association ledger rows = %d, want 3 original observations", got)
	}
}

func takeConn(t *testing.T, pool *sqlitex.Pool) *sqlite.Conn {
	t.Helper()
	conn, err := pool.Take(context.Background())
	if err != nil {
		t.Fatalf("Pool.Take: %v", err)
	}
	return conn
}

func makeStoreEntry(t *testing.T, rawSessionID, rawProjectHash, rawHostSlug string, harness defaults.Harness, startMs int64, tokensIn, tokensOut int) ingest.StoreEntry {
	t.Helper()
	sessionID, err := ingest.NewSessionID(rawSessionID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	projectHash, err := ingest.NewProjectHash(rawProjectHash)
	if err != nil {
		t.Fatalf("NewProjectHash: %v", err)
	}
	hostSlug, err := ingest.NewHostSlug(rawHostSlug)
	if err != nil {
		t.Fatalf("NewHostSlug: %v", err)
	}
	model, err := ingest.NewModelID("claude-opus-4-6")
	if err != nil {
		t.Fatalf("NewModelID: %v", err)
	}
	resolvedPath, err := ingest.NewResolvedPath("/test/path/session.jsonl")
	if err != nil {
		t.Fatalf("NewResolvedPath: %v", err)
	}
	ingested := startMs + 120000
	return ingest.StoreEntry{
		Metadata: &ingest.UnifiedMetadata{
			SchemaVersion: ingest.CurrentSchemaVersion,
			SessionID:     sessionID,
			ModelHarness:  harness,
			Model:         model,
			HostSlug:      hostSlug,
			Timestamp:     ingest.TimestampInfo{Start: startMs, End: startMs + 60000, Ingested: &ingested},
			Source:        ingest.SourceInfo{FilePath: string(resolvedPath), Format: ingest.SourceFormatJSONL},
			Project:       ingest.ProjectInfo{Hash: projectHash, Name: "test-project", FilePath: "/home/test/project"},
			Stats:         ingest.StatsInfo{TurnCount: 10, ToolCallCount: 5, DurationMs: 60000, TokensIn: tokensIn, TokensOut: tokensOut},
		},
		Session: ingest.DiscoveredSession{SessionID: sessionID, Harness: harness, SourcePath: resolvedPath, SourceFormat: ingest.SourceFormatJSONL},
	}
}
