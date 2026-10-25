package transcript

import (
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

func transcriptStringPtr(value string) *string { return &value }

// TestEntriesToTurns_SuppressesEmptySystemControlTurn pins the viewer rule for
// a managed Claude control record that carries no conversation: it is stored
// evidence with a source ref, but it must not become a header over an empty
// body. A control record with a preview still renders.
func TestEntriesToTurns_SuppressesEmptySystemControlTurn(t *testing.T) {
	t.Parallel()
	sid := ingest.SessionID("d2b1e6f3-5b7c-4d8e-9f0a-1b2c3d4e5f6a")
	attachment := "attachment"
	permission := "permission-mode"
	entries := []schema.SessionEntry{
		{
			SessionID: sid, EntryIndex: 0, Harness: schema.HarnessClaudeCode,
			Role: schema.RoleUser, EntryType: schema.EntryTypeText,
			ContentPreview: transcriptStringPtr("run the checks"), SourceEntryRef: "source:0",
		},
		{
			SessionID: sid, EntryIndex: 1, Harness: schema.HarnessClaudeCode,
			Role: schema.RoleSystem, EntryType: schema.EntryTypeSystem,
			PartType: &attachment, SourceEntryRef: "source:1",
		},
		{
			SessionID: sid, EntryIndex: 2, Harness: schema.HarnessClaudeCode,
			Role: schema.RoleSystem, EntryType: schema.EntryTypeSystem,
			PartType: &permission, ContentPreview: transcriptStringPtr("permission mode: acceptEdits"),
			SourceEntryRef: "source:2",
		},
	}

	turns := EntriesToTurns(entries)
	if len(turns) != 2 {
		t.Fatalf("turns = %d, want 2 (the prompt and the control record with a preview)", len(turns))
	}
	if turns[0].Content != "run the checks" {
		t.Fatalf("first turn = %q, want the prompt", turns[0].Content)
	}
	if turns[1].Content != "permission mode: acceptEdits" {
		t.Fatalf("second turn = %q, want the visible control preview", turns[1].Content)
	}
}
