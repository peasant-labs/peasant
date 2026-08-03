package ingest_test

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// TestClaudeIndexer_FullDepthDecomposition verifies that with fullDepth=true,
// content blocks are decomposed into depth=1 child entries.
func TestClaudeIndexer_FullDepthDecomposition(t *testing.T) {
	t.Parallel()

	// JSONL line with 3 content blocks: thinking + text + tool_use.
	jsonlLine := `{"type":"assistant","uuid":"uuid-1","timestamp":1708531200000,"message":{"role":"assistant","content":[{"type":"thinking","text":"Let me analyze this..."},{"type":"text","text":"I'll help you with that."},{"type":"tool_use","id":"toolu_abc","name":"Read","input":{"file_path":"/src/main.go"}}]}}`

	mfs := testutil.NewMemFS()
	mfs.WriteFile("/test/session.jsonl", []byte(jsonlLine), 0644)

	idx := ingest.NewClaudeIndexer(mfs, ingest.WithClaudeFullDepth(true))
	session := ingest.DiscoveredSession{
		SessionID:    ingest.SessionID(testutil.TestSessionUUID),
		Harness:      defaults.HarnessClaudeCode,
		SourceFormat: ingest.SourceFormatJSONL,
		SourcePath:   "/test/session.jsonl",
	}

	entries, err := idx.IndexTranscript(context.Background(), session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}

	// Expect: 1 depth=0 (message summary) + 3 depth=1 (thinking, text, tool_use) = 4 entries.
	if len(entries) != 4 {
		t.Fatalf("expected 4 entries (1 depth=0 + 3 depth=1), got %d", len(entries))
	}

	// Verify depth=0 parent entry.
	parent := entries[0]
	if parent.Depth != 0 {
		t.Errorf("entry[0] Depth: expected 0, got %d", parent.Depth)
	}
	if parent.ParentIndex != nil {
		t.Errorf("entry[0] ParentIndex: expected nil, got %v", parent.ParentIndex)
	}
	if !parent.HasThinking {
		t.Error("entry[0] HasThinking: expected true")
	}
	// Depth=0 entries must NOT have PartType set.
	if parent.PartType != nil {
		t.Errorf("entry[0] PartType: expected nil for depth=0, got %q", *parent.PartType)
	}

	// Verify depth=1 thinking entry.
	thinking := entries[1]
	if thinking.Depth != 1 {
		t.Errorf("entry[1] Depth: expected 1, got %d", thinking.Depth)
	}
	if thinking.ParentIndex == nil || *thinking.ParentIndex != 0 {
		t.Errorf("entry[1] ParentIndex: expected 0, got %v", thinking.ParentIndex)
	}
	if thinking.EntryType != ingest.EntryTypeThinking {
		t.Errorf("entry[1] EntryType: expected %s, got %s", ingest.EntryTypeThinking, thinking.EntryType)
	}
	if !thinking.HasThinking {
		t.Error("entry[1] HasThinking: expected true")
	}
	if thinking.ContentPreview == nil || *thinking.ContentPreview != "Let me analyze this..." {
		t.Errorf("entry[1] ContentPreview: got %v", thinking.ContentPreview)
	}
	if thinking.PartType == nil || *thinking.PartType != "thinking" {
		t.Errorf("thinking entry PartType: got %v, want \"thinking\"", thinking.PartType)
	}

	// Verify depth=1 text entry.
	text := entries[2]
	if text.Depth != 1 {
		t.Errorf("entry[2] Depth: expected 1, got %d", text.Depth)
	}
	if text.EntryType != ingest.EntryTypeText {
		t.Errorf("entry[2] EntryType: expected %s, got %s", ingest.EntryTypeText, text.EntryType)
	}
	if text.ContentPreview == nil || *text.ContentPreview != "I'll help you with that." {
		t.Errorf("entry[2] ContentPreview: got %v", text.ContentPreview)
	}
	if text.PartType == nil || *text.PartType != "text" {
		t.Errorf("text entry PartType: got %v, want \"text\"", text.PartType)
	}

	// Verify depth=1 tool_use entry.
	toolUse := entries[3]
	if toolUse.Depth != 1 {
		t.Errorf("entry[3] Depth: expected 1, got %d", toolUse.Depth)
	}
	if toolUse.EntryType != ingest.EntryTypeToolUse {
		t.Errorf("entry[3] EntryType: expected %s, got %s", ingest.EntryTypeToolUse, toolUse.EntryType)
	}
	if !toolUse.HasToolUse {
		t.Error("entry[3] HasToolUse: expected true")
	}
	if toolUse.ToolNamesCSV == nil || *toolUse.ToolNamesCSV != "Read" {
		t.Errorf("entry[3] ToolNamesCSV: got %v", toolUse.ToolNamesCSV)
	}
	if toolUse.ToolCallID == nil || *toolUse.ToolCallID != "toolu_abc" {
		t.Errorf("entry[3] ToolCallID: got %v", toolUse.ToolCallID)
	}
	if toolUse.ToolInput == nil {
		t.Error("entry[3] ToolInput: expected non-nil")
	}
	if toolUse.PartType == nil || *toolUse.PartType != "tool_use" {
		t.Errorf("tool_use entry PartType: got %v, want \"tool_use\"", toolUse.PartType)
	}
}

// TestClaudeIndexer_FullDepthV1Compat verifies that fullDepth=false
// produces identical output to the v1 indexer (no depth=1 entries).
func TestClaudeIndexer_FullDepthV1Compat(t *testing.T) {
	t.Parallel()

	jsonlLine := `{"type":"assistant","uuid":"uuid-1","timestamp":1708531200000,"message":{"role":"assistant","content":[{"type":"thinking","text":"thinking..."},{"type":"text","text":"response"},{"type":"tool_use","name":"Edit","input":{}}]}}`

	mfs := testutil.NewMemFS()
	mfs.WriteFile("/test/session.jsonl", []byte(jsonlLine), 0644)

	session := ingest.DiscoveredSession{
		SessionID:    ingest.SessionID(testutil.TestSessionUUID),
		Harness:      defaults.HarnessClaudeCode,
		SourceFormat: ingest.SourceFormatJSONL,
		SourcePath:   "/test/session.jsonl",
	}

	// Without fullDepth (v1 compat).
	v1Idx := ingest.NewClaudeIndexer(mfs)
	v1Entries, err := v1Idx.IndexTranscript(context.Background(), session)
	if err != nil {
		t.Fatalf("v1 IndexTranscript: %v", err)
	}

	// With fullDepth=false explicitly.
	v1ExplicitIdx := ingest.NewClaudeIndexer(mfs, ingest.WithClaudeFullDepth(false))
	v1ExplicitEntries, err := v1ExplicitIdx.IndexTranscript(context.Background(), session)
	if err != nil {
		t.Fatalf("v1 explicit IndexTranscript: %v", err)
	}

	// Both should produce exactly 1 entry (depth=0 only).
	if len(v1Entries) != 1 {
		t.Errorf("v1 entries: expected 1, got %d", len(v1Entries))
	}
	if len(v1ExplicitEntries) != 1 {
		t.Errorf("v1 explicit entries: expected 1, got %d", len(v1ExplicitEntries))
	}

	// All entries should be depth=0.
	for i, e := range v1Entries {
		if e.Depth != 0 {
			t.Errorf("v1 entry[%d] Depth: expected 0, got %d", i, e.Depth)
		}
	}
}

// TestClaudeIndexer_FullDepthEmptyContent verifies that an empty content array
// produces only the depth=0 entry (no depth=1 children).
func TestClaudeIndexer_FullDepthEmptyContent(t *testing.T) {
	t.Parallel()

	jsonlLine := `{"type":"assistant","uuid":"uuid-1","timestamp":1708531200000,"message":{"role":"assistant","content":[]}}`

	mfs := testutil.NewMemFS()
	mfs.WriteFile("/test/session.jsonl", []byte(jsonlLine), 0644)

	idx := ingest.NewClaudeIndexer(mfs, ingest.WithClaudeFullDepth(true))
	session := ingest.DiscoveredSession{
		SessionID:    ingest.SessionID(testutil.TestSessionUUID),
		Harness:      defaults.HarnessClaudeCode,
		SourceFormat: ingest.SourceFormatJSONL,
		SourcePath:   "/test/session.jsonl",
	}

	entries, err := idx.IndexTranscript(context.Background(), session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry (depth=0 only), got %d", len(entries))
	}
	if entries[0].Depth != 0 {
		t.Errorf("entry[0] Depth: expected 0, got %d", entries[0].Depth)
	}
}

// TestClaudeIndexer_FullDepthUserMessage verifies that user messages
// (plain string content) don't generate depth=1 entries.
func TestClaudeIndexer_FullDepthUserMessage(t *testing.T) {
	t.Parallel()

	jsonlLine := `{"type":"human","uuid":"uuid-1","timestamp":1708531200000,"message":{"role":"user","content":"Fix the bug please"}}`

	mfs := testutil.NewMemFS()
	mfs.WriteFile("/test/session.jsonl", []byte(jsonlLine), 0644)

	idx := ingest.NewClaudeIndexer(mfs, ingest.WithClaudeFullDepth(true))
	session := ingest.DiscoveredSession{
		SessionID:    ingest.SessionID(testutil.TestSessionUUID),
		Harness:      defaults.HarnessClaudeCode,
		SourceFormat: ingest.SourceFormatJSONL,
		SourcePath:   "/test/session.jsonl",
	}

	entries, err := idx.IndexTranscript(context.Background(), session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}

	// User message with string content → no depth=1 entries.
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry (user message, no decomposition), got %d", len(entries))
	}
	if entries[0].Depth != 0 {
		t.Errorf("entry[0] Depth: expected 0, got %d", entries[0].Depth)
	}
}

// TestClaudeIndexer_F1StringTimestamp verifies that parseIndexTimestamp
// handles string-encoded integer timestamps (e.g., "1708531200000").
func TestClaudeIndexer_F1StringTimestamp(t *testing.T) {
	t.Parallel()

	// Timestamp as a quoted string number, not a bare integer.
	jsonlLine := `{"type":"assistant","uuid":"uuid-1","timestamp":"1708531200000","message":{"role":"assistant","content":"hello"}}`

	mfs := testutil.NewMemFS()
	mfs.WriteFile("/test/session.jsonl", []byte(jsonlLine), 0644)

	idx := ingest.NewClaudeIndexer(mfs)
	session := ingest.DiscoveredSession{
		SessionID:    ingest.SessionID(testutil.TestSessionUUID),
		Harness:      defaults.HarnessClaudeCode,
		SourceFormat: ingest.SourceFormatJSONL,
		SourcePath:   "/test/session.jsonl",
	}

	entries, err := idx.IndexTranscript(context.Background(), session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	if entries[0].TimestampMs == nil {
		t.Fatal("TimestampMs should not be nil for string-encoded integer timestamp")
	}
	if *entries[0].TimestampMs != 1708531200000 {
		t.Errorf("TimestampMs: expected 1708531200000, got %d", *entries[0].TimestampMs)
	}
}

// TestClaudeIndexer_FullDepthToolResult verifies tool_result decomposition
// with error flag and output content.
func TestClaudeIndexer_FullDepthToolResult(t *testing.T) {
	t.Parallel()

	jsonlLine := `{"type":"tool","uuid":"uuid-2","timestamp":1708531260000,"message":{"role":"tool","content":[{"type":"tool_result","tool_use_id":"toolu_xyz","content":"file contents here","is_error":false}]}}`

	mfs := testutil.NewMemFS()
	mfs.WriteFile("/test/session.jsonl", []byte(jsonlLine), 0644)

	idx := ingest.NewClaudeIndexer(mfs, ingest.WithClaudeFullDepth(true))
	session := ingest.DiscoveredSession{
		SessionID:    ingest.SessionID(testutil.TestSessionUUID),
		Harness:      defaults.HarnessClaudeCode,
		SourceFormat: ingest.SourceFormatJSONL,
		SourcePath:   "/test/session.jsonl",
	}

	entries, err := idx.IndexTranscript(context.Background(), session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}

	// 1 depth=0 + 1 depth=1 tool_result = 2 entries.
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	toolResult := entries[1]
	if toolResult.Depth != 1 {
		t.Errorf("entry[1] Depth: expected 1, got %d", toolResult.Depth)
	}
	if toolResult.EntryType != ingest.EntryTypeToolResult {
		t.Errorf("entry[1] EntryType: expected %s, got %s", ingest.EntryTypeToolResult, toolResult.EntryType)
	}
	if toolResult.ToolCallID == nil || *toolResult.ToolCallID != "toolu_xyz" {
		t.Errorf("entry[1] ToolCallID: got %v", toolResult.ToolCallID)
	}
	if toolResult.ToolOutput == nil || *toolResult.ToolOutput != "file contents here" {
		t.Errorf("entry[1] ToolOutput: got %v", toolResult.ToolOutput)
	}
	if toolResult.IsError {
		t.Error("entry[1] IsError: expected false")
	}
}

// TestClaudeIndexer_SystemContentExtraction verifies that system JSONL entries
// with top-level content (no message field) produce a non-NULL content_preview.
func TestClaudeIndexer_SystemContentExtraction(t *testing.T) {
	t.Parallel()

	// Real Claude JSONL system entry shape: content at top level, no message field.
	jsonlLine := `{"type":"system","content":"You are a helpful coding assistant. Follow the user's instructions carefully.","uuid":"sys-001","timestamp":1708531100000}`

	mfs := testutil.NewMemFS()
	mfs.WriteFile("/test/session.jsonl", []byte(jsonlLine), 0644)

	idx := ingest.NewClaudeIndexer(mfs, ingest.WithClaudeFullDepth(true))
	session := ingest.DiscoveredSession{
		SessionID:    ingest.SessionID(testutil.TestSessionUUID),
		Harness:      defaults.HarnessClaudeCode,
		SourceFormat: ingest.SourceFormatJSONL,
		SourcePath:   "/test/session.jsonl",
	}

	entries, err := idx.IndexTranscript(context.Background(), session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	sysEntry := entries[0]
	if sysEntry.EntryType != ingest.EntryTypeSystem {
		t.Errorf("EntryType: expected %s, got %s", ingest.EntryTypeSystem, sysEntry.EntryType)
	}
	if sysEntry.Role != ingest.RoleSystem {
		t.Errorf("Role: expected %s, got %s", ingest.RoleSystem, sysEntry.Role)
	}
	if sysEntry.ContentPreview == nil {
		t.Fatal("ContentPreview should not be nil for system entry with top-level content")
	}
	if *sysEntry.ContentPreview != "You are a helpful coding assistant. Follow the user's instructions carefully." {
		t.Errorf("ContentPreview: got %q", *sysEntry.ContentPreview)
	}
}

// TestClaudeIndexer_SystemContentBlockArray verifies system entries with
// array-of-blocks content (less common but valid per Claude API).
func TestClaudeIndexer_SystemContentBlockArray(t *testing.T) {
	t.Parallel()

	jsonlLine := `{"type":"system","content":[{"type":"text","text":"System prompt as block array."}],"uuid":"sys-002","timestamp":1708531100000}`

	mfs := testutil.NewMemFS()
	mfs.WriteFile("/test/session.jsonl", []byte(jsonlLine), 0644)

	idx := ingest.NewClaudeIndexer(mfs, ingest.WithClaudeFullDepth(true))
	session := ingest.DiscoveredSession{
		SessionID:    ingest.SessionID(testutil.TestSessionUUID),
		Harness:      defaults.HarnessClaudeCode,
		SourceFormat: ingest.SourceFormatJSONL,
		SourcePath:   "/test/session.jsonl",
	}

	entries, err := idx.IndexTranscript(context.Background(), session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	if entries[0].ContentPreview == nil {
		t.Fatal("ContentPreview should not be nil for system entry with block-array content")
	}
	if *entries[0].ContentPreview != "System prompt as block array." {
		t.Errorf("ContentPreview: got %q", *entries[0].ContentPreview)
	}
}

// TestClaudeIndexer_ToolResultLinksToToolUse verifies that tool_use and tool_result
// depth=1 entries share the same ToolCallID (the tool_use_id from the wire format).
func TestClaudeIndexer_ToolResultLinksToToolUse(t *testing.T) {
	t.Parallel()

	// Assistant message with tool_use, followed by tool message with tool_result.
	assistantLine := `{"type":"assistant","uuid":"uuid-a","timestamp":1708531200000,"message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_01ABC","name":"Read","input":{"file_path":"/src/main.go"}}]}}`
	toolLine := `{"type":"tool","uuid":"uuid-t","parentUuid":"uuid-a","timestamp":1708531201000,"message":{"role":"tool","content":[{"type":"tool_result","tool_use_id":"toolu_01ABC","content":"package main\n\nfunc main() {}"}]}}`

	mfs := testutil.NewMemFS()
	mfs.WriteFile("/test/session.jsonl", []byte(assistantLine+"\n"+toolLine), 0644)

	idx := ingest.NewClaudeIndexer(mfs, ingest.WithClaudeFullDepth(true))
	session := ingest.DiscoveredSession{
		SessionID:    ingest.SessionID(testutil.TestSessionUUID),
		Harness:      defaults.HarnessClaudeCode,
		SourceFormat: ingest.SourceFormatJSONL,
		SourcePath:   "/test/session.jsonl",
	}

	entries, err := idx.IndexTranscript(context.Background(), session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}

	// 2 depth=0 + 1 depth=1 tool_use + 1 depth=1 tool_result = 4 entries.
	if len(entries) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(entries))
	}

	toolUse := entries[1] // depth=1 tool_use under assistant
	toolRes := entries[3] // depth=1 tool_result under tool

	if toolUse.EntryType != ingest.EntryTypeToolUse {
		t.Errorf("tool_use EntryType: expected %s, got %s", ingest.EntryTypeToolUse, toolUse.EntryType)
	}
	if toolRes.EntryType != ingest.EntryTypeToolResult {
		t.Errorf("tool_result EntryType: expected %s, got %s", ingest.EntryTypeToolResult, toolRes.EntryType)
	}

	// Both should reference the same ToolCallID.
	if toolUse.ToolCallID == nil {
		t.Fatal("tool_use ToolCallID should not be nil")
	}
	if toolRes.ToolCallID == nil {
		t.Fatal("tool_result ToolCallID should not be nil")
	}
	if *toolUse.ToolCallID != *toolRes.ToolCallID {
		t.Errorf("ToolCallID mismatch: tool_use=%q, tool_result=%q", *toolUse.ToolCallID, *toolRes.ToolCallID)
	}
	if *toolUse.ToolCallID != "toolu_01ABC" {
		t.Errorf("ToolCallID: expected toolu_01ABC, got %q", *toolUse.ToolCallID)
	}
}
