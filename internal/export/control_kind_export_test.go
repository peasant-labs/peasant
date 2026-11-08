package export_test

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// TestExportSession_CarriesControlKind is the export parity gate: the local
// export builds the same SessionDetailPayload the viewer's WebSocket serves, so
// a control-record turn must carry its producer kind here too, and a system
// entry must not be counted as a conversation turn.
func TestExportSession_CarriesControlKind(t *testing.T) {
	t.Parallel()
	const sessionID = "45454545-4545-4545-4545-45454545454e"
	db := storetest.Open(t)
	storetest.SeedSession(t, db, sessionID)
	sid := schema.SessionID(sessionID)

	sourceRef := func(value string) schema.SourceEntryRef { return schema.SourceEntryRef(value) }
	mode := "mode"
	attachment := "attachment"
	text := func(value string) *string { return &value }
	entries := []schema.SessionEntry{
		{
			SessionID: sid, EntryIndex: 0, Harness: schema.HarnessClaudeCode,
			EntryType: schema.EntryTypeText, Role: schema.RoleUser,
			ContentPreview: text("run the checks"), SourceEntryRef: sourceRef("source:0"),
		},
		{
			SessionID: sid, EntryIndex: 1, Harness: schema.HarnessClaudeCode,
			EntryType: schema.EntryTypeText, Role: schema.RoleSystem,
			PartType: &mode, ContentPreview: text("mode: normal"), SourceEntryRef: sourceRef("source:1"),
		},
		{
			SessionID: sid, EntryIndex: 2, Harness: schema.HarnessClaudeCode,
			EntryType: schema.EntryTypeText, Role: schema.RoleSystem,
			PartType: &attachment, SourceEntryRef: sourceRef("source:2"),
		},
		{
			SessionID: sid, EntryIndex: 3, Harness: schema.HarnessClaudeCode,
			EntryType: schema.EntryTypeText, Role: schema.RoleAssistant,
			ContentPreview: text("checks are running"), SourceEntryRef: sourceRef("source:3"),
		},
	}
	if err := testutil.WriteFullEntries(context.Background(), db, sid, entries); err != nil {
		t.Fatalf("WriteFullEntries: %v", err)
	}

	payload, err := export.ExportSession(context.Background(), db, testutil.NewMemFS(), sessionID)
	if err != nil {
		t.Fatalf("ExportSession: %v", err)
	}
	if len(payload.Turns) != 3 {
		t.Fatalf("exported %d turns, want 3 (the empty attachment is suppressed)", len(payload.Turns))
	}
	kinds := make(map[int]string, len(payload.Turns))
	for _, turn := range payload.Turns {
		if turn.ControlKind != nil {
			kinds[turn.Index] = *turn.ControlKind
		}
	}
	if len(kinds) != 1 || kinds[1] != "mode" {
		t.Fatalf("exported control kinds = %v, want only turn 1 = mode", kinds)
	}
	if payload.TurnCount != 2 {
		t.Fatalf("exported turnCount = %d, want 2 conversation turns", payload.TurnCount)
	}
	if err := schema.ValidateSessionDetailPayload(*payload); err != nil {
		t.Fatalf("exported payload is invalid: %v", err)
	}
}
