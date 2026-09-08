package store_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
)

func TestMigrationV52LegacyCaptureIncomplete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id := ingest.SessionID("aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa")
	seedSession(t, s, string(id))
	entries := batchTestEntries(id, "short-preview", 1)
	if err := s.IndexSessionEntries(context.Background(), id, entries); err != nil {
		t.Fatal(err)
	}
	// Reconstruct the exact V51 schema by removing only the new objects. All
	// shipped migrations and their fixtures remain immutable.
	execContentSQL(t, s, `DROP TABLE session_entry_full_content_chunks; DROP TABLE session_entry_full_content; DROP TABLE session_content_captures; PRAGMA user_version=51;`)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := capture(t, s, id)
	if c.Status != ingest.ContentCaptureIncomplete || c.SourceAuthority != ingest.ContentSourceNone || c.CaptureFormat != ingest.ContentCaptureFormatLegacyPreviewOnly || c.FailureCode != "legacy_preview_only" || c.FullCaptureSHA256 != "" || c.PublicationCaptureRevision != 0 {
		t.Fatalf("legacy capture incorrectly inferred: %+v", c)
	}
	if _, err := s.ReadSessionEntries(context.Background(), id, ingest.SessionEntryReadOptions{Mode: ingest.SessionEntryReadFullContent}); err == nil {
		t.Fatal("short legacy preview became complete")
	}
	got, err := s.ListEntries(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if *got[0].ContentPreview != *entries[0].ContentPreview {
		t.Fatal("migration changed legacy preview")
	}
}
