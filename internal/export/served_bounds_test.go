package export_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// TestExportSession_BoundsAnOversizedToolResult exports a session whose stored
// tool result is larger than the wire contract lets a served document be.
// `peasant export` reads the same projection the viewer reads, so the export
// carries the same visible note and stays a valid contract document instead of
// failing the whole session.
func TestExportSession_BoundsAnOversizedToolResult(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s := storetest.Open(t)
	sessionID := testutil.TestSessionUUID
	storetest.SeedSession(t, s, sessionID)

	sid := schema.SessionID(sessionID)
	toolID := "toolu_export_oversized"
	toolInput := `{"command":"cat huge.log"}`
	toolOutput := strings.Repeat("x", 9<<20)
	preview := "reading the whole log"

	entries := []schema.SessionEntry{
		{
			SessionID: sid, EntryIndex: 0, Harness: defaults.HarnessClaudeCode,
			EntryType: ingest.EntryTypeText, Role: ingest.RoleAssistant,
			ContentPreview: &preview, HasToolUse: true, Depth: 0,
		},
		{
			SessionID: sid, EntryIndex: 1, Harness: defaults.HarnessClaudeCode,
			EntryType: ingest.EntryTypeToolUse, Role: ingest.RoleAssistant,
			HasToolUse: true, ToolCallID: &toolID, ToolInput: &toolInput,
			ToolOutput: &toolOutput, Depth: 1, ParentIndex: intPtrExportBounds(0),
			ToolNamesCSV: strPtrExportBounds("Bash"),
		},
	}
	if err := testutil.WriteFullEntries(ctx, s, sid, entries); err != nil {
		t.Fatalf("WriteFullEntries: %v", err)
	}

	fs := testutil.NewMemFS()
	exported, err := export.ExportSession(ctx, s, fs, sessionID)
	if err != nil {
		t.Fatalf("ExportSession refused a session with an oversized record: %v", err)
	}
	encoded, err := json.Marshal(exported)
	if err != nil {
		t.Fatalf("marshal export: %v", err)
	}
	if _, err := schema.DecodeSessionDetailPayloadRaw(encoded); err != nil {
		t.Fatalf("the exported document does not decode through the contract: %v", err)
	}

	found := ""
	for _, turn := range exported.Turns {
		for _, call := range turn.ToolCalls {
			if len(call.Result) > len(found) {
				found = call.Result
			}
		}
	}
	note := strings.Index(found, "\n[")
	if note < 0 {
		t.Fatalf("the exported tool result carries no bound note")
	}
	if note != defaults.ServedTextFieldBudgetBytes {
		t.Errorf("the exported tool result shows %d bytes, want the per-field budget %d", note, defaults.ServedTextFieldBudgetBytes)
	}
	if !strings.HasPrefix(toolOutput, found[:note]) {
		t.Errorf("the exported tool result is not the leading bytes of the stored record")
	}
	for _, want := range []string{"tool result bounded for display", "showing 1.0 MiB of 9.0 MiB", "kept by the peasant store that recorded this session"} {
		if !strings.Contains(found[note:], want) {
			t.Errorf("bound note %q does not contain %q", found[note:], want)
		}
	}
}

func intPtrExportBounds(v int) *int       { return &v }
func strPtrExportBounds(v string) *string { return &v }
