package store_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// TestManagedGenerationMetadataRefreshCommitsOnce pins the write budget: a
// metadata-only refresh is one transaction, so the write-ahead log gains one
// commit frame whatever the entry count. The generation is seeded through the
// public activation path; the measurement starts after that seed.
func TestManagedGenerationMetadataRefreshCommitsOnce(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "refresh.db")
	artifacts, err := store.NewOSGenerationArtifactStore(filepath.Join(dir, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	locker, err := store.NewFileSessionLocker(filepath.Join(dir, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(dbPath,
		store.WithPoolSize(1),
		store.WithIndexFormats(store.V2IndexFormat()),
		store.WithGenerationArtifacts(artifacts, locker),
		store.WithWALAutocheckpointDisabled())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	}()

	sid := schema.SessionID("eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee")
	inputCount := int64(1)
	ingested := int64(3)
	metadata := schema.UnifiedMetadata{
		SchemaVersion: ingest.CurrentSchemaVersion,
		SessionID:     sid,
		ModelHarness:  defaults.HarnessClaudeCode,
		Model:         "claude-sonnet-4-5",
		Version:       "1.0.0",
		Timestamp:     schema.TimestampInfo{Start: 1, End: 2, Ingested: &ingested},
		Source:        schema.SourceInfo{FilePath: "/synthetic/refresh.jsonl", Format: schema.SourceFormatJSONL},
		Project:       schema.ProjectContext{Hash: schema.ProjectHash(strings.Repeat("1", 64)), Name: "commit"},
		HostSlug:      "host-commit",
		Stats:         schema.SessionStats{TurnCount: 1, InputSubmissionCount: &inputCount},
		ContentHash:   strings.Repeat("a", 64),
	}
	preview := "commit-frame refresh entry"
	generation := indexformat.V2{Generation: indexformat.Generation{
		ID:           "gen-commit-source",
		Completeness: indexformat.GenerationCompletenessComplete,
		Metadata:     metadata,
		Main: indexformat.Partition{Entries: []schema.SessionEntry{{
			SessionID: sid, EntryIndex: 0, Harness: defaults.HarnessClaudeCode,
			EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: &preview,
			SourceEntryRef: "e_commit_0000",
		}}},
		SourceEvidenceDigest: strings.Repeat("e", 64),
		TitleRefs:            []schema.SourceEntryRef{"e_commit_0000"},
	}}
	storetest.SeedGenerationPublication(t, db, &metadata, generation, nil)
	// The activation seed establishes the producer revision and completion time;
	// this test also states the input proof a metadata-only refresh requires.
	conn, err := db.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET indexed_input_hash = ? WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{strings.Repeat("b", 64), string(sid)},
	}); err != nil {
		db.Pool().Put(conn)
		t.Fatal(err)
	}
	db.Pool().Put(conn)
	expected, err := db.ReadIndexState(t.Context(), ingest.SessionID(sid))
	if err != nil {
		t.Fatalf("read the seeded index proof: %v", err)
	}

	before, err := storetest.CountWALCommitFrames(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	refreshed := metadata
	refreshed.Version = "1.1.0"
	result, err := db.RefreshManagedGeneration(t.Context(), store.ManagedGenerationRefresh{
		SessionID: sid, SourceGenerationID: generation.Generation.ID,
		Metadata: refreshed, Expected: expected,
	})
	if err != nil {
		t.Fatalf("refresh the managed generation: %v", err)
	}
	after, err := storetest.CountWALCommitFrames(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if result.Disposition != store.ManagedGenerationRefreshInstalled {
		t.Fatalf("refresh disposition = %q, want installed", result.Disposition)
	}
	if got := after - before; got != 1 {
		t.Fatalf("metadata-only refresh gained %d WAL commit frames, want 1", got)
	}
}
