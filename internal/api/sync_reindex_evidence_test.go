package api

import (
	"context"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// TestStoredEvidenceStillRefusedReportsLegacyNullExtra pins the post-re-index
// check that keeps the publish popup from offering the same repair again. An
// older build could store an Extra of "null"; the stored entry reads back, but
// the strict publication reader refuses the field. The check must return that
// refusal, and the response note must name a step that can change the outcome.
func TestStoredEvidenceStillRefusedReportsLegacyNullExtra(t *testing.T) {
	t.Parallel()
	s := storetest.Open(t)
	ctx := context.Background()
	sid := ingest.SessionID("9f8e7d6c-5b4a-4938-8271-6f5e4d3c2b1a")
	storetest.SeedSession(t, s, string(sid))

	preview := "a stored prompt"
	if err := s.IndexSessionEntries(ctx, sid, []schema.SessionEntry{{
		SessionID: sid, EntryIndex: 0, Harness: schema.HarnessClaudeCode,
		Role: schema.RoleUser, EntryType: schema.EntryTypeText,
		ContentPreview: &preview,
	}}); err != nil {
		t.Fatal(err)
	}
	// An earlier build could write this value; the current writer refuses it,
	// so place the legacy row directly to exercise the read side.
	conn, err := s.Pool().Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Pool().Put(conn)
	if err := sqlitex.ExecuteTransient(conn,
		`UPDATE session_entries SET extra = 'null' WHERE session_id = ?`,
		&sqlitex.ExecOptions{Args: []any{string(sid)}}); err != nil {
		t.Fatal(err)
	}

	handler := &syncHandler{store: s}
	evidenceErr := handler.storedEvidenceStillRefused(ctx, sid)
	if evidenceErr == nil {
		t.Fatal("a stored null Extra passed the strict publication evidence check")
	}
	if !strings.Contains(evidenceErr.Error(), "extra") {
		t.Fatalf("refusal does not name the extra field: %v", evidenceErr)
	}

	note := storedEvidenceStillRefusedNote(evidenceErr)
	if !strings.Contains(note, "will not change it") || !strings.Contains(note, "Restore or repair the original recording") {
		t.Fatalf("post-re-index note does not name an outcome-changing step: %q", note)
	}
}
