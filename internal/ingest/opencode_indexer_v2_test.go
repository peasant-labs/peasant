package ingest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// addOpenCodePartTyped writes a typed part file (tool_use, tool_result, text) to MemFS.
func addOpenCodePartTyped(t *testing.T, fs *testutil.MemFS, msgID, partID, partType string, extra map[string]any) {
	t.Helper()
	root := "/opencode-store"

	partData := map[string]any{
		"id":   partID,
		"type": partType,
	}
	for k, v := range extra {
		partData[k] = v
	}
	b, err := json.Marshal(partData)
	if err != nil {
		t.Fatalf("marshal part: %v", err)
	}

	partPath := fmt.Sprintf("%s/storage/part/%s/%s.json", root, msgID, partID)
	if err := fs.WriteFile(partPath, b, 0644); err != nil {
		t.Fatalf("write part: %v", err)
	}
}

func TestOpenCodeIndexer_FullDepthPartDecomposition(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewOpenCodeIndexer(fs, ingest.WithOpenCodeFullDepth(true))
	ctx := context.Background()

	sesID := testutil.TestOpenCodeSesID
	session := setupOpenCodeFixture(t, fs, sesID, "proj1")

	// Add an assistant message.
	addOpenCodeMessage(t, fs, sesID, "msg_001abc", "assistant", 500, 200)

	// Add part files: tool_use, text, tool_result.
	addOpenCodePartTyped(t, fs, "msg_001abc", "part_001_tool", "tool_use", map[string]any{
		"name":  "Read",
		"input": map[string]any{"file": "/tmp/test.go"},
	})
	addOpenCodePartTyped(t, fs, "msg_001abc", "part_002_text", "text", map[string]any{
		"text": "Here is the file content.",
	})
	addOpenCodePartTyped(t, fs, "msg_001abc", "part_003_result", "tool_result", map[string]any{
		"content": "file content here",
	})

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}

	// Expect 4 entries: 1 depth=0 message + 3 depth=1 parts.
	if len(entries) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(entries))
	}

	// Entry 0: depth=0 message.
	e0 := entries[0]
	if e0.Depth != 0 {
		t.Errorf("e0 depth: expected 0, got %d", e0.Depth)
	}
	if e0.ParentIndex != nil {
		t.Errorf("e0 parent_index: expected nil, got %v", e0.ParentIndex)
	}
	if e0.EntryIndex != 0 {
		t.Errorf("e0 entry_index: expected 0, got %d", e0.EntryIndex)
	}
	if e0.Role != ingest.RoleAssistant {
		t.Errorf("e0 role: expected %s, got %s", ingest.RoleAssistant, e0.Role)
	}
	if e0.EntryType != ingest.EntryTypeToolUse {
		t.Errorf("e0 type: expected %s (parts exist), got %s", ingest.EntryTypeToolUse, e0.EntryType)
	}
	if !e0.HasToolUse {
		t.Error("e0 has_tool_use: expected true")
	}
	if e0.Harness != defaults.HarnessOpenCode {
		t.Errorf("e0 provider: expected %s, got %s", defaults.HarnessOpenCode, e0.Harness)
	}
	// Depth=0 entries must NOT have PartType set.
	if e0.PartType != nil {
		t.Errorf("e0 PartType: expected nil for depth=0, got %q", *e0.PartType)
	}

	// Entry 1: depth=1 tool_use part (part_001_tool).
	e1 := entries[1]
	if e1.Depth != 1 {
		t.Errorf("e1 depth: expected 1, got %d", e1.Depth)
	}
	if e1.ParentIndex == nil || *e1.ParentIndex != 0 {
		t.Errorf("e1 parent_index: expected 0, got %v", e1.ParentIndex)
	}
	if e1.EntryIndex != 1 {
		t.Errorf("e1 entry_index: expected 1, got %d", e1.EntryIndex)
	}
	if e1.EntryType != ingest.EntryTypeToolUse {
		t.Errorf("e1 type: expected %s, got %s", ingest.EntryTypeToolUse, e1.EntryType)
	}
	if e1.Role != ingest.RoleAssistant {
		t.Errorf("e1 role: expected %s, got %s", ingest.RoleAssistant, e1.Role)
	}
	if !e1.HasToolUse {
		t.Error("e1 has_tool_use: expected true")
	}
	if e1.ToolInput == nil {
		t.Error("e1 tool_input: expected non-nil")
	} else {
		// Should contain the input JSON.
		var inputMap map[string]any
		if err := json.Unmarshal([]byte(*e1.ToolInput), &inputMap); err != nil {
			t.Errorf("e1 tool_input: invalid JSON: %v", err)
		} else if inputMap["file"] != "/tmp/test.go" {
			t.Errorf("e1 tool_input.file: expected /tmp/test.go, got %v", inputMap["file"])
		}
	}
	if e1.EntryID == nil || *e1.EntryID != "part_001_tool" {
		t.Errorf("e1 entry_id: expected %q, got %v", "part_001_tool", e1.EntryID)
	}
	if e1.ToolCallID == nil || *e1.ToolCallID != "part_001_tool" {
		t.Errorf("e1 tool_call_id: expected %q, got %v", "part_001_tool", e1.ToolCallID)
	}
	if e1.ToolNamesCSV == nil || *e1.ToolNamesCSV != "Read" {
		t.Errorf("e1 tool_names_csv: expected %q, got %v", "Read", e1.ToolNamesCSV)
	}
	if e1.Harness != defaults.HarnessOpenCode {
		t.Errorf("e1 provider: expected %s, got %s", defaults.HarnessOpenCode, e1.Harness)
	}
	if e1.PartType == nil || *e1.PartType != "tool_use" {
		t.Errorf("e1.PartType: got %v, want \"tool_use\"", e1.PartType)
	}

	// Entry 2: depth=1 text part (part_002_text).
	e2 := entries[2]
	if e2.Depth != 1 {
		t.Errorf("e2 depth: expected 1, got %d", e2.Depth)
	}
	if e2.ParentIndex == nil || *e2.ParentIndex != 0 {
		t.Errorf("e2 parent_index: expected 0, got %v", e2.ParentIndex)
	}
	if e2.EntryIndex != 2 {
		t.Errorf("e2 entry_index: expected 2, got %d", e2.EntryIndex)
	}
	if e2.EntryType != ingest.EntryTypeText {
		t.Errorf("e2 type: expected %s, got %s", ingest.EntryTypeText, e2.EntryType)
	}
	if e2.ContentPreview == nil || *e2.ContentPreview != "Here is the file content." {
		t.Errorf("e2 preview: expected %q, got %v", "Here is the file content.", e2.ContentPreview)
	}
	if e2.ToolCallID != nil {
		t.Errorf("e2 tool_call_id: expected nil for text part, got %v", e2.ToolCallID)
	}
	if e2.PartType == nil || *e2.PartType != "text" {
		t.Errorf("e2.PartType: got %v, want \"text\"", e2.PartType)
	}

	// Entry 3: depth=1 tool_result part (part_003_result).
	e3 := entries[3]
	if e3.Depth != 1 {
		t.Errorf("e3 depth: expected 1, got %d", e3.Depth)
	}
	if e3.ParentIndex == nil || *e3.ParentIndex != 0 {
		t.Errorf("e3 parent_index: expected 0, got %v", e3.ParentIndex)
	}
	if e3.EntryIndex != 3 {
		t.Errorf("e3 entry_index: expected 3, got %d", e3.EntryIndex)
	}
	if e3.EntryType != ingest.EntryTypeToolResult {
		t.Errorf("e3 type: expected %s, got %s", ingest.EntryTypeToolResult, e3.EntryType)
	}
	if e3.Role != ingest.RoleTool {
		t.Errorf("e3 role: expected %s, got %s", ingest.RoleTool, e3.Role)
	}
	if e3.ToolOutput == nil {
		t.Error("e3 tool_output: expected non-nil")
	} else if *e3.ToolOutput != "file content here" {
		// A JSON string output is unwrapped to its plain text, the same shape
		// the Claude reader stores, so one renderer shows both harnesses.
		t.Errorf("e3 tool_output: expected %q, got %q", "file content here", *e3.ToolOutput)
	}
	if e3.ToolCallID == nil || *e3.ToolCallID != "part_003_result" {
		t.Errorf("e3 tool_call_id: expected %q, got %v", "part_003_result", e3.ToolCallID)
	}
	if e3.PartType == nil || *e3.PartType != "tool_result" {
		t.Errorf("e3.PartType: got %v, want \"tool_result\"", e3.PartType)
	}
}

func TestOpenCodeIndexer_FullDepthNoParts(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewOpenCodeIndexer(fs, ingest.WithOpenCodeFullDepth(true))
	ctx := context.Background()

	sesID := testutil.TestOpenCodeSesID
	session := setupOpenCodeFixture(t, fs, sesID, "proj1")

	// Add a user message with no part directory.
	addOpenCodeMessage(t, fs, sesID, "msg_001abc", "user", 0, 0)

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}

	// Only 1 depth=0 entry, no depth=1 entries.
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	e := entries[0]
	if e.Depth != 0 {
		t.Errorf("depth: expected 0, got %d", e.Depth)
	}
	if e.ParentIndex != nil {
		t.Errorf("parent_index: expected nil, got %v", e.ParentIndex)
	}
	if e.Role != ingest.RoleUser {
		t.Errorf("role: expected %s, got %s", ingest.RoleUser, e.Role)
	}
	if e.EntryType != ingest.EntryTypeText {
		t.Errorf("type: expected %s, got %s", ingest.EntryTypeText, e.EntryType)
	}
}

func TestOpenCodeIndexer_FullDepthEmptyPartDir(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewOpenCodeIndexer(fs, ingest.WithOpenCodeFullDepth(true))
	ctx := context.Background()

	sesID := testutil.TestOpenCodeSesID
	session := setupOpenCodeFixture(t, fs, sesID, "proj1")

	// Add an assistant message.
	addOpenCodeMessage(t, fs, sesID, "msg_001abc", "assistant", 300, 100)

	// Create the part directory but leave it empty.
	partDir := "/opencode-store/storage/part/msg_001abc"
	if err := fs.MkdirAll(partDir, 0755); err != nil {
		t.Fatalf("mkdir part dir: %v", err)
	}

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}

	// Only 1 depth=0 entry (empty part dir produces no depth=1 entries).
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	e := entries[0]
	if e.Depth != 0 {
		t.Errorf("depth: expected 0, got %d", e.Depth)
	}
	if e.Role != ingest.RoleAssistant {
		t.Errorf("role: expected %s, got %s", ingest.RoleAssistant, e.Role)
	}
	// No parts, so no tool_use refinement.
	if e.EntryType != ingest.EntryTypeText {
		t.Errorf("type: expected %s, got %s", ingest.EntryTypeText, e.EntryType)
	}
	if e.HasToolUse {
		t.Error("has_tool_use: expected false (empty part dir)")
	}
}

func TestOpenCodeIndexer_V1Compat(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	// fullDepth=false (default) — same as v1.
	idx := ingest.NewOpenCodeIndexer(fs)
	ctx := context.Background()

	sesID := testutil.TestOpenCodeSesID
	session := setupOpenCodeFixture(t, fs, sesID, "proj1")

	addOpenCodeMessage(t, fs, sesID, "msg_001abc", "assistant", 300, 100)

	// Add parts — these should NOT produce depth=1 entries in v1 mode.
	addOpenCodePartTyped(t, fs, "msg_001abc", "part_001", "tool_use", map[string]any{
		"name":  "Read",
		"input": map[string]any{"file": "/tmp/test.go"},
	})
	addOpenCodePartTyped(t, fs, "msg_001abc", "part_002", "text", map[string]any{
		"text": "Some text.",
	})

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}

	// V1: only 1 depth=0 entry, no depth=1 entries.
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry (v1 compat), got %d", len(entries))
	}

	e := entries[0]
	if e.Depth != 0 {
		t.Errorf("depth: expected 0, got %d", e.Depth)
	}
	// Parts exist, so tool_use refinement still applies.
	if e.EntryType != ingest.EntryTypeToolUse {
		t.Errorf("type: expected %s, got %s", ingest.EntryTypeToolUse, e.EntryType)
	}
	if !e.HasToolUse {
		t.Error("has_tool_use: expected true (parts exist)")
	}
	if e.Harness != defaults.HarnessOpenCode {
		t.Errorf("provider: expected %s, got %s", defaults.HarnessOpenCode, e.Harness)
	}
}

func TestOpenCodeIndexer_ExtraFieldPopulated(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewOpenCodeIndexer(fs)
	ctx := context.Background()

	sesID := testutil.TestOpenCodeSesID
	session := setupOpenCodeFixture(t, fs, sesID, "proj1")

	// Write a message with modelID and reasoning tokens directly (for precise control).
	root := "/opencode-store"
	msgPath := fmt.Sprintf("%s/storage/message/%s/msg_001abc.json", root, sesID)
	msgJSON := fmt.Sprintf(
		`{"id":"msg_001abc","sessionID":%q,"role":"assistant","modelID":"claude-opus-4-6","time":{"created":1700000001000,"completed":1700000002000},"tokens":{"input":500,"output":200,"reasoning":150,"cache_read":1000,"cache_write":250},"content":"thinking deeply"}`,
		sesID,
	)
	if err := fs.WriteFile(msgPath, []byte(msgJSON), 0644); err != nil {
		t.Fatalf("write message: %v", err)
	}

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	e := entries[0]
	if e.Extra == nil {
		t.Fatal("Extra: expected non-nil")
	}

	var extra map[string]any
	if err := json.Unmarshal([]byte(*e.Extra), &extra); err != nil {
		t.Fatalf("Extra: invalid JSON: %v", err)
	}

	// Check tokens_reasoning.
	if tr, ok := extra["tokens_reasoning"]; !ok {
		t.Error("Extra: missing tokens_reasoning")
	} else if int(tr.(float64)) != 150 {
		t.Errorf("Extra tokens_reasoning: expected 150, got %v", tr)
	}

	// Check model_id.
	if mid, ok := extra["model_id"]; !ok {
		t.Error("Extra: missing model_id")
	} else if mid != "claude-opus-4-6" {
		t.Errorf("Extra model_id: expected claude-opus-4-6, got %v", mid)
	}

	// Check cache_read.
	if cr, ok := extra["cache_read"]; !ok {
		t.Error("Extra: missing cache_read")
	} else if int(cr.(float64)) != 1000 {
		t.Errorf("Extra cache_read: expected 1000, got %v", cr)
	}

	// Check cache_write.
	if cw, ok := extra["cache_write"]; !ok {
		t.Error("Extra: missing cache_write")
	} else if int(cw.(float64)) != 250 {
		t.Errorf("Extra cache_write: expected 250, got %v", cw)
	}
}

func TestOpenCodeIndexer_ExtraFieldOmittedWhenEmpty(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewOpenCodeIndexer(fs)
	ctx := context.Background()

	sesID := testutil.TestOpenCodeSesID
	session := setupOpenCodeFixture(t, fs, sesID, "proj1")

	// Message with no modelID and no reasoning/cache tokens.
	addOpenCodeMessage(t, fs, sesID, "msg_001abc", "user", 0, 0)

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	// Extra should be nil when no extra fields are present.
	if entries[0].Extra != nil {
		t.Errorf("Extra: expected nil for message with no extra data, got %q", *entries[0].Extra)
	}
}

func TestOpenCodeIndexer_FullDepthMultipleMessages(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewOpenCodeIndexer(fs, ingest.WithOpenCodeFullDepth(true))
	ctx := context.Background()

	sesID := testutil.TestOpenCodeSesID
	session := setupOpenCodeFixture(t, fs, sesID, "proj1")

	// Message 1: user (no parts).
	addOpenCodeMessage(t, fs, sesID, "msg_001abc", "user", 0, 0)

	// Message 2: assistant with 2 parts.
	addOpenCodeMessage(t, fs, sesID, "msg_002def", "assistant", 500, 200)
	addOpenCodePartTyped(t, fs, "msg_002def", "part_001", "tool_use", map[string]any{
		"name":  "Write",
		"input": map[string]any{"path": "/tmp/out.txt"},
	})
	addOpenCodePartTyped(t, fs, "msg_002def", "part_002", "text", map[string]any{
		"text": "Done writing.",
	})

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}

	// Expect 4 entries: msg_001abc (d=0), msg_002def (d=0), part_001 (d=1), part_002 (d=1).
	if len(entries) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(entries))
	}

	// Entry indices should be sequential: 0, 1, 2, 3.
	for i, e := range entries {
		if e.EntryIndex != i {
			t.Errorf("entry[%d] index: expected %d, got %d", i, i, e.EntryIndex)
		}
	}

	// e0: user message, depth=0.
	if entries[0].Depth != 0 {
		t.Errorf("e0 depth: expected 0, got %d", entries[0].Depth)
	}
	if entries[0].Role != ingest.RoleUser {
		t.Errorf("e0 role: expected %s, got %s", ingest.RoleUser, entries[0].Role)
	}

	// e1: assistant message, depth=0.
	if entries[1].Depth != 0 {
		t.Errorf("e1 depth: expected 0, got %d", entries[1].Depth)
	}
	if entries[1].Role != ingest.RoleAssistant {
		t.Errorf("e1 role: expected %s, got %s", ingest.RoleAssistant, entries[1].Role)
	}

	// e2 & e3: parts, depth=1, parent=1 (the assistant message).
	for _, i := range []int{2, 3} {
		if entries[i].Depth != 1 {
			t.Errorf("e%d depth: expected 1, got %d", i, entries[i].Depth)
		}
		if entries[i].ParentIndex == nil || *entries[i].ParentIndex != 1 {
			t.Errorf("e%d parent_index: expected 1, got %v", i, entries[i].ParentIndex)
		}
	}
}

func TestOpenCodeIndexer_ContentPreviewFromParts(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	// fullDepth=false (v1 mode) — depth=0 entries should still get content_preview from parts.
	idx := ingest.NewOpenCodeIndexer(fs)
	ctx := context.Background()

	sesID := testutil.TestOpenCodeSesID
	session := setupOpenCodeFixture(t, fs, sesID, "proj1")

	// Assistant message with empty content (as OpenCode stores it on disk).
	root := "/opencode-store"
	msgPath := fmt.Sprintf("%s/storage/message/%s/msg_001abc.json", root, sesID)
	msgJSON := fmt.Sprintf(
		`{"id":"msg_001abc","sessionID":%q,"role":"assistant","time":{"created":1700000001000,"completed":1700000002000},"tokens":{"input":300,"output":100},"content":""}`,
		sesID,
	)
	if err := fs.WriteFile(msgPath, []byte(msgJSON), 0644); err != nil {
		t.Fatalf("write message: %v", err)
	}

	// Add part files: first is tool_use (no text), second is text with content.
	addOpenCodePartTyped(t, fs, "msg_001abc", "part_001_tool", "tool_use", map[string]any{
		"name":  "Read",
		"input": map[string]any{"file": "/tmp/test.go"},
	})
	addOpenCodePartTyped(t, fs, "msg_001abc", "part_002_text", "text", map[string]any{
		"text": "Here is the analysis result.",
	})

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}

	// fullDepth=false: only 1 depth=0 entry, no depth=1 entries.
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	e := entries[0]
	if e.Depth != 0 {
		t.Errorf("depth: expected 0, got %d", e.Depth)
	}
	if e.Role != ingest.RoleAssistant {
		t.Errorf("role: expected %s, got %s", ingest.RoleAssistant, e.Role)
	}
	// Parts exist, so type should be tool_use.
	if e.EntryType != ingest.EntryTypeToolUse {
		t.Errorf("type: expected %s (parts exist), got %s", ingest.EntryTypeToolUse, e.EntryType)
	}
	// ContentPreview should come from the first text-type part file.
	if e.ContentPreview == nil {
		t.Fatal("content_preview: expected non-nil (should be extracted from text part file)")
	}
	if *e.ContentPreview != "Here is the analysis result." {
		t.Errorf("content_preview: expected %q, got %q", "Here is the analysis result.", *e.ContentPreview)
	}
}

// TestOpenCodeIndexer_PartDirPathRegression guards against reintroducing a sessionID
// segment in the part directory path. Parts must live at {storageRoot}/part/{msgID}/,
// NOT at {storageRoot}/part/{sessionID}/{msgID}/.
func TestOpenCodeIndexer_PartDirPathRegression(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewOpenCodeIndexer(fs)
	ctx := context.Background()

	sesID := testutil.TestOpenCodeSesID
	session := setupOpenCodeFixture(t, fs, sesID, "proj1")

	addOpenCodeMessage(t, fs, sesID, "msg_001abc", "assistant", 300, 100)

	// Write part files under the CORRECT path: {storageRoot}/part/{msgID}/
	// (no sessionID segment). If the indexer uses the wrong path the parts are
	// not found, countParts returns 0, and HasToolUse stays false.
	addOpenCodePartTyped(t, fs, "msg_001abc", "part_001", "tool_use", map[string]any{
		"name":  "Read",
		"input": map[string]any{"file": "/tmp/test.go"},
	})

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	e := entries[0]
	// Parts are under the correct path → tool_use refinement must fire.
	if !e.HasToolUse {
		t.Error("has_tool_use: expected true — part file at correct path not found (sessionID segment reintroduced?)")
	}
	if e.EntryType != ingest.EntryTypeToolUse {
		t.Errorf("entry_type: expected %s, got %s — part file at correct path not found", ingest.EntryTypeToolUse, e.EntryType)
	}
}

// TestOpenCodeIndexer_FullDepthUserTextSuppression verifies that a text part whose
// content exactly matches the parent's ContentPreview is suppressed (echo removal).
// Given user msg with one text part matching parent content, When fullDepth=true,
// Then only depth-0 parent entry is emitted; no depth-1 echo with role=assistant.
func TestOpenCodeIndexer_FullDepthUserTextSuppression(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewOpenCodeIndexer(fs, ingest.WithOpenCodeFullDepth(true), ingest.WithOpenCodeFullContent(true))
	ctx := context.Background()

	sesID := testutil.TestOpenCodeSesID
	session := setupOpenCodeFixture(t, fs, sesID, "proj1")

	content := "Hello from user"
	// addOpenCodeMessageWithContent sets inline content; the depth=0 entry's ContentPreview
	// will be set to this string. The text part replicates that content exactly.
	addOpenCodeMessageWithContent(t, fs, sesID, "msg_001abc", "user", content)
	addOpenCodePartTyped(t, fs, "msg_001abc", "part_001_echo", "text", map[string]any{
		"text": content, // exact match → should be suppressed
	})

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}

	// Only depth=0 parent; echo suppressed.
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry (echo suppressed), got %d", len(entries))
	}

	e0 := entries[0]
	if e0.Depth != 0 {
		t.Errorf("e0 depth: expected 0, got %d", e0.Depth)
	}
	if e0.Role != ingest.RoleUser {
		t.Errorf("e0 role: expected %s, got %s", ingest.RoleUser, e0.Role)
	}
}

// TestOpenCodeIndexer_FullDepthUserMultiPart verifies that when a user message has a
// matching text part AND a non-matching file part, only the echo text part is suppressed
// while the file part (different type) is kept.
func TestOpenCodeIndexer_FullDepthUserMultiPart(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewOpenCodeIndexer(fs, ingest.WithOpenCodeFullDepth(true), ingest.WithOpenCodeFullContent(true))
	ctx := context.Background()

	sesID := testutil.TestOpenCodeSesID
	session := setupOpenCodeFixture(t, fs, sesID, "proj1")

	content := "Fix this bug"
	addOpenCodeMessageWithContent(t, fs, sesID, "msg_001abc", "user", content)
	// First part: echo text (should be suppressed).
	addOpenCodePartTyped(t, fs, "msg_001abc", "part_001_echo", "text", map[string]any{
		"text": content,
	})
	// Second part: tool_result — different type, kept always.
	addOpenCodePartTyped(t, fs, "msg_001abc", "part_002_result", "tool_result", map[string]any{
		"content": "some output",
	})

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}

	// depth=0 parent + depth=1 tool_result (echo text suppressed).
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries (echo suppressed, tool_result kept), got %d", len(entries))
	}

	e0 := entries[0]
	if e0.Depth != 0 {
		t.Errorf("e0 depth: expected 0, got %d", e0.Depth)
	}
	if e0.Role != ingest.RoleUser {
		t.Errorf("e0 role: expected %s, got %s", ingest.RoleUser, e0.Role)
	}

	e1 := entries[1]
	if e1.Depth != 1 {
		t.Errorf("e1 depth: expected 1, got %d", e1.Depth)
	}
	if e1.EntryType != ingest.EntryTypeToolResult {
		t.Errorf("e1 type: expected %s, got %s", ingest.EntryTypeToolResult, e1.EntryType)
	}
	if e1.Role != ingest.RoleTool {
		t.Errorf("e1 role: expected %s, got %s", ingest.RoleTool, e1.Role)
	}
}

// TestOpenCodeIndexer_FullDepthUserNonDuplicateText verifies that when the text part
// content differs from the parent's ContentPreview, the part is kept (not suppressed)
// and its role matches the parent role.
func TestOpenCodeIndexer_FullDepthUserNonDuplicateText(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewOpenCodeIndexer(fs, ingest.WithOpenCodeFullDepth(true), ingest.WithOpenCodeFullContent(true))
	ctx := context.Background()

	sesID := testutil.TestOpenCodeSesID
	session := setupOpenCodeFixture(t, fs, sesID, "proj1")

	content := "Fix this bug"
	differentContent := "Additional context for the fix"
	addOpenCodeMessageWithContent(t, fs, sesID, "msg_001abc", "user", content)
	// Text part has different content — must not be suppressed.
	addOpenCodePartTyped(t, fs, "msg_001abc", "part_001_text", "text", map[string]any{
		"text": differentContent,
	})

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}

	// depth=0 parent + depth=1 text (different content kept).
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries (non-duplicate text kept), got %d", len(entries))
	}

	e0 := entries[0]
	if e0.Depth != 0 {
		t.Errorf("e0 depth: expected 0, got %d", e0.Depth)
	}
	if e0.Role != ingest.RoleUser {
		t.Errorf("e0 role: expected %s, got %s", ingest.RoleUser, e0.Role)
	}

	e1 := entries[1]
	if e1.Depth != 1 {
		t.Errorf("e1 depth: expected 1, got %d", e1.Depth)
	}
	if e1.EntryType != ingest.EntryTypeText {
		t.Errorf("e1 type: expected %s, got %s", ingest.EntryTypeText, e1.EntryType)
	}
	// Role must match parent role (user), not hardcoded assistant.
	if e1.Role != ingest.RoleUser {
		t.Errorf("e1 role: expected %s (parent role), got %s", ingest.RoleUser, e1.Role)
	}
	if e1.ContentPreview == nil || *e1.ContentPreview != differentContent {
		t.Errorf("e1 content_preview: expected %q, got %v", differentContent, e1.ContentPreview)
	}
}

// TestOpenCodeIndexer_FullDepthUserEmptyParent verifies that when the parent message has
// no inline content, extractPreviewFromParts fills the parent's ContentPreview from the
// text part. Since both parent and part share the same text, the part is suppressed as an
// echo. This is the standard OpenCode case — messages never have inline content.
func TestOpenCodeIndexer_FullDepthUserEmptyParent(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewOpenCodeIndexer(fs, ingest.WithOpenCodeFullDepth(true), ingest.WithOpenCodeFullContent(true))
	ctx := context.Background()

	sesID := testutil.TestOpenCodeSesID
	session := setupOpenCodeFixture(t, fs, sesID, "proj1")

	// Message with null inline content → extractPreviewFromParts fills parent from part.
	addOpenCodeMessageNoContent(t, fs, sesID, "msg_001abc", "user")
	partContent := "Here is some text"
	addOpenCodePartTyped(t, fs, "msg_001abc", "part_001_text", "text", map[string]any{
		"text": partContent,
	})

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}

	// Only depth=0 parent: the text part matches parent preview (both from same part),
	// so it is suppressed as an echo. This is correct for OpenCode.
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry (text part suppressed as echo), got %d", len(entries))
	}
	if entries[0].Depth != 0 {
		t.Errorf("e0 depth: expected 0, got %d", entries[0].Depth)
	}
	if entries[0].ContentPreview == nil || *entries[0].ContentPreview != partContent {
		t.Errorf("e0 content: expected %q, got %v", partContent, entries[0].ContentPreview)
	}
}

// TestOpenCodeIndexer_FullDepthReasoningMapping verifies that a reasoning part
// is mapped to EntryType=thinking, HasThinking=true, and PartType="reasoning".
func TestOpenCodeIndexer_FullDepthReasoningMapping(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewOpenCodeIndexer(fs, ingest.WithOpenCodeFullDepth(true))
	ctx := context.Background()

	sesID := testutil.TestOpenCodeSesID
	session := setupOpenCodeFixture(t, fs, sesID, "proj1")

	// User message (no parts).
	addOpenCodeMessage(t, fs, sesID, "msg_001abc", "user", 0, 0)

	// Assistant message with a reasoning part.
	addOpenCodeMessage(t, fs, sesID, "msg_002def", "assistant", 500, 200)
	addOpenCodePartTyped(t, fs, "msg_002def", "part_001_reasoning", "reasoning", map[string]any{
		"text": "Let me think through this step by step.",
	})

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}

	// Expect: user(d=0), assistant(d=0), reasoning(d=1) = 3 entries.
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	// Find the depth=1 reasoning entry (index 2).
	reasoning := entries[2]
	if reasoning.Depth != 1 {
		t.Errorf("reasoning entry Depth: expected 1, got %d", reasoning.Depth)
	}
	if reasoning.EntryType != ingest.EntryTypeThinking {
		t.Errorf("reasoning entry EntryType: expected %s, got %s", ingest.EntryTypeThinking, reasoning.EntryType)
	}
	if !reasoning.HasThinking {
		t.Error("reasoning entry HasThinking: expected true")
	}
	if reasoning.PartType == nil {
		t.Fatal("reasoning entry PartType: expected non-nil")
	}
	if *reasoning.PartType != "reasoning" {
		t.Errorf("reasoning entry PartType: expected %q, got %q", "reasoning", *reasoning.PartType)
	}
}

// TestOpenCodeIndexer_FullDepthStructuralMarkers verifies that step-start and step-finish
// parts are preserved as depth=1 entries with the correct PartType and nil ContentPreview.
func TestOpenCodeIndexer_FullDepthStructuralMarkers(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewOpenCodeIndexer(fs, ingest.WithOpenCodeFullDepth(true))
	ctx := context.Background()

	sesID := testutil.TestOpenCodeSesID
	session := setupOpenCodeFixture(t, fs, sesID, "proj1")

	// Assistant message with step-start and step-finish parts (no text content).
	addOpenCodeMessage(t, fs, sesID, "msg_001abc", "assistant", 300, 100)
	addOpenCodePartTyped(t, fs, "msg_001abc", "part_001_step_start", "step-start", map[string]any{})
	addOpenCodePartTyped(t, fs, "msg_001abc", "part_002_step_finish", "step-finish", map[string]any{})

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}

	// Expect: assistant(d=0), step-start(d=1), step-finish(d=1) = 3 entries.
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	// Entry 1: depth=1 step-start.
	stepStart := entries[1]
	if stepStart.Depth != 1 {
		t.Errorf("step-start Depth: expected 1, got %d", stepStart.Depth)
	}
	if stepStart.PartType == nil {
		t.Fatal("step-start PartType: expected non-nil")
	}
	if *stepStart.PartType != "step-start" {
		t.Errorf("step-start PartType: expected %q, got %q", "step-start", *stepStart.PartType)
	}
	if stepStart.ContentPreview != nil {
		t.Errorf("step-start ContentPreview: expected nil (no text), got %q", *stepStart.ContentPreview)
	}

	// Entry 2: depth=1 step-finish.
	stepFinish := entries[2]
	if stepFinish.Depth != 1 {
		t.Errorf("step-finish Depth: expected 1, got %d", stepFinish.Depth)
	}
	if stepFinish.PartType == nil {
		t.Fatal("step-finish PartType: expected non-nil")
	}
	if *stepFinish.PartType != "step-finish" {
		t.Errorf("step-finish PartType: expected %q, got %q", "step-finish", *stepFinish.PartType)
	}
	if stepFinish.ContentPreview != nil {
		t.Errorf("step-finish ContentPreview: expected nil (no text), got %q", *stepFinish.ContentPreview)
	}
}
