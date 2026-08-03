package ingest_test

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// ---------------------------------------------------------------------------
// Claude indexer: ToolKind classification tests
// ---------------------------------------------------------------------------

// TestClaudeIndexer_ToolKind_ReadTool verifies that a tool_use block with
// tool name "Read" produces ToolKind=read on the depth=1 entry when full-depth
// decomposition is enabled.
func TestClaudeIndexer_ToolKind_ReadTool(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewClaudeIndexer(fs, ingest.WithClaudeFullDepth(true))
	ctx := context.Background()

	// Fixture: assistant message with a single Read tool_use block.
	session := makeClaudeSession(t, fs, testutil.TestSessionUUID,
		`{"type":"assistant","uuid":"a1","timestamp":1708531200000,"message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_read1","name":"Read","input":{"file_path":"/src/main.go"}}]}}`,
	)

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}

	// Expect 2 entries: 1 depth=0 (message summary) + 1 depth=1 (tool_use).
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	toolEntry := entries[1]
	if toolEntry.Depth != 1 {
		t.Errorf("entry[1] Depth: expected 1, got %d", toolEntry.Depth)
	}
	if toolEntry.EntryType != ingest.EntryTypeToolUse {
		t.Errorf("entry[1] EntryType: expected %s, got %s", ingest.EntryTypeToolUse, toolEntry.EntryType)
	}
	if !toolEntry.HasToolUse {
		t.Error("entry[1] HasToolUse: expected true")
	}
	if toolEntry.ToolCallID == nil || *toolEntry.ToolCallID != "toolu_read1" {
		t.Errorf("entry[1] ToolCallID: expected %q, got %v", "toolu_read1", toolEntry.ToolCallID)
	}

	if toolEntry.ToolKind == nil {
		t.Fatal("entry[1] ToolKind: expected non-nil, got nil")
	}
	wantKind := schema.ToolCallKindRead
	if *toolEntry.ToolKind != wantKind {
		t.Errorf("entry[1] ToolKind: expected %s, got %s", wantKind, *toolEntry.ToolKind)
	}
}

// TestClaudeIndexer_ToolKind_EditTool verifies that a tool named "Edit"
// produces ToolKind=edit.
func TestClaudeIndexer_ToolKind_EditTool(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewClaudeIndexer(fs, ingest.WithClaudeFullDepth(true))
	ctx := context.Background()

	session := makeClaudeSession(t, fs, testutil.TestSessionUUID,
		`{"type":"assistant","uuid":"a1","timestamp":1708531200000,"message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_edit1","name":"Edit","input":{"file_path":"/src/main.go","old_text":"foo","new_text":"bar"}}]}}`,
	)

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}

	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	toolEntry := entries[1]
	if toolEntry.ToolKind == nil {
		t.Fatal("entry[1] ToolKind: expected non-nil, got nil")
	}
	wantKind := schema.ToolCallKindEdit
	if *toolEntry.ToolKind != wantKind {
		t.Errorf("entry[1] ToolKind: expected %s, got %s", wantKind, *toolEntry.ToolKind)
	}
}

// TestClaudeIndexer_ToolKind_BashTool verifies that a tool named "Bash"
// (shell execution) produces ToolKind=execute.
func TestClaudeIndexer_ToolKind_BashTool(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewClaudeIndexer(fs, ingest.WithClaudeFullDepth(true))
	ctx := context.Background()

	session := makeClaudeSession(t, fs, testutil.TestSessionUUID,
		`{"type":"assistant","uuid":"a1","timestamp":1708531200000,"message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_bash1","name":"Bash","input":{"command":"go test ./..."}}]}}`,
	)

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}

	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	toolEntry := entries[1]
	if toolEntry.ToolKind == nil {
		t.Fatal("entry[1] ToolKind: expected non-nil, got nil")
	}
	wantKind := schema.ToolCallKindExecute
	if *toolEntry.ToolKind != wantKind {
		t.Errorf("entry[1] ToolKind: expected %s, got %s", wantKind, *toolEntry.ToolKind)
	}
}

// TestClaudeIndexer_ToolKind_GrepTool verifies that a tool named "Grep"
// produces ToolKind=search.
func TestClaudeIndexer_ToolKind_GrepTool(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewClaudeIndexer(fs, ingest.WithClaudeFullDepth(true))
	ctx := context.Background()

	session := makeClaudeSession(t, fs, testutil.TestSessionUUID,
		`{"type":"assistant","uuid":"a1","timestamp":1708531200000,"message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_grep1","name":"Grep","input":{"pattern":"TODO","path":"."}}]}}`,
	)

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}

	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	toolEntry := entries[1]
	if toolEntry.ToolKind == nil {
		t.Fatal("entry[1] ToolKind: expected non-nil, got nil")
	}
	wantKind := schema.ToolCallKindSearch
	if *toolEntry.ToolKind != wantKind {
		t.Errorf("entry[1] ToolKind: expected %s, got %s", wantKind, *toolEntry.ToolKind)
	}
}

// TestClaudeIndexer_ToolKind_DepthZeroNoToolKind verifies that the depth=0
// summary entry for a message with tool_use blocks does NOT have ToolKind set
// (ToolKind is only meaningful on depth=1 tool_use entries).
func TestClaudeIndexer_ToolKind_DepthZeroNoToolKind(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewClaudeIndexer(fs, ingest.WithClaudeFullDepth(true))
	ctx := context.Background()

	session := makeClaudeSession(t, fs, testutil.TestSessionUUID,
		`{"type":"assistant","uuid":"a1","timestamp":1708531200000,"message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_r1","name":"Read","input":{}}]}}`,
	)

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}

	if len(entries) < 1 {
		t.Fatal("expected at least 1 entry")
	}

	// Depth=0 entry should NOT have ToolKind.
	parent := entries[0]
	if parent.Depth != 0 {
		t.Errorf("entry[0] Depth: expected 0, got %d", parent.Depth)
	}
	if parent.ToolKind != nil {
		t.Errorf("entry[0] ToolKind: expected nil for depth=0 message, got %s", *parent.ToolKind)
	}
}

// ---------------------------------------------------------------------------
// OpenCode indexer: ToolKind classification tests
// ---------------------------------------------------------------------------

// TestOpenCodeIndexer_ToolKind_ReadTool verifies that an OpenCode tool_use
// part with tool name "Read" produces ToolKind=read on the depth=1 entry.
func TestOpenCodeIndexer_ToolKind_ReadTool(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewOpenCodeIndexer(fs, ingest.WithOpenCodeFullDepth(true))
	ctx := context.Background()

	sesID := testutil.TestOpenCodeSesID
	session := setupOpenCodeFixture(t, fs, sesID, "proj1")

	// Add an assistant message with tool use.
	addOpenCodeMessage(t, fs, sesID, "msg_001abc", "assistant", 500, 200)

	// Add a Read tool_use part.
	addOpenCodePartTyped(t, fs, "msg_001abc", "part_001_tool", "tool_use", map[string]any{
		"name":  "Read",
		"input": map[string]any{"file": "/src/main.go"},
	})

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}

	// Find the depth=1 tool_use entry.
	var toolEntry *schema.SessionEntry
	for i := range entries {
		if entries[i].Depth == 1 && entries[i].EntryType == ingest.EntryTypeToolUse {
			toolEntry = &entries[i]
			break
		}
	}
	if toolEntry == nil {
		t.Fatal("expected a depth=1 tool_use entry")
	}
	if toolEntry.Harness != defaults.HarnessOpenCode {
		t.Errorf("tool entry provider: expected %s, got %s", defaults.HarnessOpenCode, toolEntry.Harness)
	}

	if toolEntry.ToolKind == nil {
		t.Fatal("tool entry ToolKind: expected non-nil, got nil")
	}
	wantKind := schema.ToolCallKindRead
	if *toolEntry.ToolKind != wantKind {
		t.Errorf("tool entry ToolKind: expected %s, got %s", wantKind, *toolEntry.ToolKind)
	}
}

// TestOpenCodeIndexer_ToolKind_BashExecute verifies that an OpenCode tool_use
// part with tool name "Bash" produces ToolKind=execute.
func TestOpenCodeIndexer_ToolKind_BashExecute(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewOpenCodeIndexer(fs, ingest.WithOpenCodeFullDepth(true))
	ctx := context.Background()

	sesID := testutil.TestOpenCodeSesID
	session := setupOpenCodeFixture(t, fs, sesID, "proj1")
	addOpenCodeMessage(t, fs, sesID, "msg_002abc", "assistant", 300, 100)
	addOpenCodePartTyped(t, fs, "msg_002abc", "part_002_bash", "tool_use", map[string]any{
		"name":  "Bash",
		"input": map[string]any{"command": "make build"},
	})

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}

	var toolEntry *schema.SessionEntry
	for i := range entries {
		if entries[i].Depth == 1 && entries[i].EntryType == ingest.EntryTypeToolUse {
			toolEntry = &entries[i]
			break
		}
	}
	if toolEntry == nil {
		t.Fatal("expected a depth=1 tool_use entry")
	}

	if toolEntry.ToolKind == nil {
		t.Fatal("tool entry ToolKind: expected non-nil, got nil")
	}
	wantKind := schema.ToolCallKindExecute
	if *toolEntry.ToolKind != wantKind {
		t.Errorf("tool entry ToolKind: expected %s, got %s", wantKind, *toolEntry.ToolKind)
	}
}

// ---------------------------------------------------------------------------
// Claude indexer: StopReason tests (depth=0 message)
// ---------------------------------------------------------------------------

// TestClaudeIndexer_StopReason_EndTurn verifies that a message with
// stop_reason in the JSONL produces a StopReason on the depth=0 entry.
func TestClaudeIndexer_StopReason_EndTurn(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewClaudeIndexer(fs)
	ctx := context.Background()

	// Fixture: assistant message with stop_reason field.
	session := makeClaudeSession(t, fs, testutil.TestSessionUUID,
		`{"type":"assistant","uuid":"a1","timestamp":1708531200000,"stop_reason":"end_turn","message":{"role":"assistant","content":[{"type":"text","text":"Done."}]}}`,
	)

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}

	if len(entries) < 1 {
		t.Fatal("expected at least 1 entry")
	}

	e := entries[0]
	if e.StopReason == nil {
		t.Fatal("entry[0] StopReason: expected non-nil, got nil")
	}
	wantReason := schema.StopReasonEndTurn
	if *e.StopReason != wantReason {
		t.Errorf("entry[0] StopReason: expected %s, got %s", wantReason, *e.StopReason)
	}
}
