package store

import (
	"context"
	"fmt"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// TestSessionsByIDsReadsOnlyRequestedRows proves the exact-ID lookup returns
// exactly the named stored rows, in the caller's order, while a large unrelated
// library is present. It is the row-level contract the link-resolution path
// relies on so resolving a couple of targets never materializes the whole
// library.
func TestSessionsByIDsReadsOnlyRequestedRows(t *testing.T) {
	db, _ := openGenerationStore(t)
	const librarySize = 512
	const projectHash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	entries := make([]ingest.StoreEntry, 0, librarySize)
	for i := 0; i < librarySize; i++ {
		unrelated := fmt.Sprintf("10000000-0000-4000-8000-%012d", i)
		entries = append(entries, minimalReaderStoreEntry(t, unrelated, projectHash))
	}
	const targetA = "20000000-0000-4000-8000-00000000000a"
	const targetB = "20000000-0000-4000-8000-00000000000b"
	entries = append(entries, minimalReaderStoreEntry(t, targetA, projectHash))
	entries = append(entries, minimalReaderStoreEntry(t, targetB, projectHash))
	if err := db.InsertSessions(context.Background(), entries); err != nil {
		t.Fatalf("seed library: %v", err)
	}

	rows, err := db.SessionsByIDs(context.Background(), []string{targetB, "20000000-0000-4000-8000-00000000000c", targetA, targetB})
	if err != nil {
		t.Fatalf("SessionsByIDs: %v", err)
	}
	got := make(map[string]bool, len(rows))
	for i := range rows {
		got[rows[i].SessionID] = true
	}
	// The absent identifier is omitted; the duplicate target is returned once.
	if len(got) != 2 || !got[targetA] || !got[targetB] {
		t.Fatalf("SessionsByIDs returned %d rows (%v), want exactly the two named targets with a %d-row unrelated library", len(got), got, librarySize)
	}
}

func TestSessionsByIDsEmptyRequestReturnsNothing(t *testing.T) {
	db, _ := openGenerationStore(t)
	rows, err := db.SessionsByIDs(context.Background(), nil)
	if err != nil {
		t.Fatalf("SessionsByIDs(nil): %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("SessionsByIDs(nil) = %d rows, want none", len(rows))
	}
}

func minimalReaderStoreEntry(t *testing.T, sessionID, projectHash string) ingest.StoreEntry {
	t.Helper()
	ingested := int64(3)
	return ingest.StoreEntry{
		Metadata: &schema.UnifiedMetadata{
			SessionID:    schema.SessionID(sessionID),
			ModelHarness: ingest.HarnessClaudeCode,
			Model:        schema.ModelID("claude-opus-4-6"),
			HostSlug:     schema.HostSlug("testslug"),
			Project: schema.ProjectContext{
				Hash:     schema.ProjectHash(projectHash),
				Name:     "reader-scope-project",
				FilePath: "/reader-scope",
			},
			Timestamp: schema.TimestampInfo{Start: 1, End: 2, Ingested: &ingested},
			Source:    schema.SourceInfo{FilePath: "/reader-scope.jsonl", Format: schema.SourceFormatJSONL},
		},
	}
}
