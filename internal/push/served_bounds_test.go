package push

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

// TestPushContent_BoundsAnOversizedToolResult builds the outward publication body
// for a session holding a tool result larger than the contract lets a transcript
// document be. The body is assembled, redacted and re-scanned against the SAME
// contract cap the village enforces, so the assembly succeeding is the proof that
// the scan in marshalBuiltTranscriptContent passes; the note travels with the
// document so the receiving UI can say the result was shortened.
func TestPushContent_BoundsAnOversizedToolResult(t *testing.T) {
	t.Parallel()

	sessionID := schema.SessionID("11111111-2222-3333-4444-555555555555")
	meta := &ingest.UnifiedMetadata{
		SessionID:    sessionID,
		ModelHarness: defaults.HarnessClaudeCode,
		Model:        schema.ModelID(testutil.TestModel),
		CWD:          "/home/example/dev/widgets",
	}
	preview := "reading the whole log"
	toolID := "toolu_push_oversized"
	toolInput := `{"command":"cat huge.log"}`
	toolOutput := strings.Repeat("x", 9<<20)
	entries := []schema.SessionEntry{
		{
			SessionID: sessionID, EntryIndex: 0, Depth: 0, Role: schema.RoleAssistant,
			EntryType: schema.EntryTypeText, ContentPreview: &preview, HasToolUse: true,
		},
		{
			SessionID: sessionID, EntryIndex: 1, Depth: 1, Role: schema.RoleAssistant,
			EntryType: schema.EntryTypeToolUse, HasToolUse: true,
			ToolCallID: &toolID, ToolInput: &toolInput, ToolOutput: &toolOutput,
			ParentIndex: intPtrPushBounds(0), ToolNamesCSV: strPtrPushBounds("Bash"),
		},
	}
	const emit = schema.PushContractVersion("0.1.1")
	redactor, err := redact.NewRedactor(redact.Standard, nil, redact.XDGPaths{})
	if err != nil {
		t.Fatalf("build the redactor the push path uses: %v", err)
	}
	fields := config.PushFieldVisibility{ProjectPath: testutil.BoolPtr(true)}

	body, err := marshalTranscriptContent(meta, entries, emit, fields, sessionorigin.Agent, redactor)
	if err != nil {
		t.Fatalf("the publication body was refused for a session with an oversized record: %v", err)
	}
	if len(body) > defaults.SessionDetailDocumentCapBytes {
		t.Errorf("the publication body is %d bytes, over the contract cap %d", len(body), defaults.SessionDetailDocumentCapBytes)
	}
	if _, err := schema.DecodeTranscriptContentRaw(body); err != nil {
		t.Fatalf("the publication body does not decode through the contract: %v", err)
	}

	var content schema.TranscriptContent
	if err := json.Unmarshal(body, &content); err != nil {
		t.Fatalf("decode the publication body: %v", err)
	}
	if content.SessionDetail == nil {
		t.Fatal("the publication body carries no session detail")
	}
	found := ""
	for _, turn := range content.SessionDetail.Turns {
		for _, call := range turn.ToolCalls {
			if len(call.Result) > len(found) {
				found = call.Result
			}
		}
	}
	note := strings.Index(found, "\n[")
	if note < 0 {
		t.Fatalf("the published tool result carries no bound note")
	}
	if note != defaults.ServedTextFieldBudgetBytes {
		t.Errorf("the published tool result shows %d bytes, want the per-field budget %d", note, defaults.ServedTextFieldBudgetBytes)
	}
	for _, want := range []string{"tool result bounded for display", "showing 1 MiB of 9 MiB", "kept by the peasant store that recorded this session"} {
		if !strings.Contains(found[note:], want) {
			t.Errorf("bound note %q does not contain %q", found[note:], want)
		}
	}
	// This body is uploaded and read on another machine. A reader there has no
	// store of their own, so nothing about the record is local to them.
	if strings.Contains(found[note:], "stored locally") {
		t.Errorf("the published bound note tells a remote reader the record is %q: %q", "stored locally", found[note:])
	}
}

func intPtrPushBounds(v int) *int       { return &v }
func strPtrPushBounds(v string) *string { return &v }
