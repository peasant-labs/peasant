package ingest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
)

func makeClaudeSession(t *testing.T, fs *testutil.MemFS, sessionID string, lines ...string) ingest.DiscoveredSession {
	t.Helper()
	path := fmt.Sprintf("/transcripts/%s.jsonl", sessionID)
	content := strings.Join(lines, "\n") + "\n"
	if err := fs.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	sid, err := ingest.NewSessionID(sessionID)
	if err != nil {
		t.Fatalf("NewSessionID(%q): %v", sessionID, err)
	}
	return ingest.DiscoveredSession{
		SessionID:    sid,
		Harness:      defaults.HarnessClaudeCode,
		SourcePath:   ingest.ResolvedPath(path),
		SourceFormat: ingest.SourceFormatJSONL,
	}
}

func TestClaudeIndexer_BasicUserAssistant(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewClaudeIndexer(fs)
	ctx := context.Background()

	session := makeClaudeSession(t, fs, testutil.TestSessionUUID,
		`{"type":"user","uuid":"u1","timestamp":"2025-01-15T10:00:00Z","message":{"role":"user","content":"Help me fix this bug"}}`,
		`{"type":"assistant","uuid":"a1","timestamp":"2025-01-15T10:00:05Z","message":{"role":"assistant","content":[{"type":"text","text":"I'll take a look at the code."}],"usage":{"input_tokens":100,"output_tokens":50}}}`,
	)

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	// Entry 0: user message.
	e0 := entries[0]
	if e0.EntryIndex != 0 {
		t.Errorf("e0 index: expected 0, got %d", e0.EntryIndex)
	}
	if e0.Role != ingest.RoleUser {
		t.Errorf("e0 role: expected %s, got %s", ingest.RoleUser, e0.Role)
	}
	if e0.EntryType != ingest.EntryTypeText {
		t.Errorf("e0 type: expected %s, got %s", ingest.EntryTypeText, e0.EntryType)
	}
	if e0.ContentPreview == nil || *e0.ContentPreview != "Help me fix this bug" {
		t.Errorf("e0 preview: expected %q, got %v", "Help me fix this bug", e0.ContentPreview)
	}
	if e0.EntryID == nil || *e0.EntryID != "u1" {
		t.Errorf("e0 entry_id: expected %q, got %v", "u1", e0.EntryID)
	}
	if e0.TimestampMs == nil {
		t.Error("e0 timestamp_ms: expected non-nil")
	}

	// Entry 1: assistant message.
	e1 := entries[1]
	if e1.Role != ingest.RoleAssistant {
		t.Errorf("e1 role: expected %s, got %s", ingest.RoleAssistant, e1.Role)
	}
	if e1.ContentPreview == nil || *e1.ContentPreview != "I'll take a look at the code." {
		t.Errorf("e1 preview: expected %q, got %v", "I'll take a look at the code.", e1.ContentPreview)
	}
	if e1.TokensIn == nil || *e1.TokensIn != 100 {
		t.Errorf("e1 tokens_in: expected 100, got %v", e1.TokensIn)
	}
	if e1.TokensOut == nil || *e1.TokensOut != 50 {
		t.Errorf("e1 tokens_out: expected 50, got %v", e1.TokensOut)
	}
}

func TestClaudeIndexer_ToolUseDetection(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewClaudeIndexer(fs)
	ctx := context.Background()

	session := makeClaudeSession(t, fs, testutil.TestSessionUUID,
		`{"type":"assistant","uuid":"a1","message":{"role":"assistant","content":[{"type":"thinking","text":"Let me read the file first"},{"type":"tool_use","name":"Read","input":{}},{"type":"tool_use","name":"Grep","input":{}}]}}`,
	)

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	e := entries[0]
	if e.EntryType != ingest.EntryTypeToolUse {
		t.Errorf("type: expected %s, got %s", ingest.EntryTypeToolUse, e.EntryType)
	}
	if !e.HasToolUse {
		t.Error("has_tool_use: expected true")
	}
	if !e.HasThinking {
		t.Error("has_thinking: expected true")
	}
	if e.ToolNamesCSV == nil || *e.ToolNamesCSV != "Read,Grep" {
		t.Errorf("tool_names_csv: expected %q, got %v", "Read,Grep", e.ToolNamesCSV)
	}
}

func TestClaudeIndexer_ToolResultError(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewClaudeIndexer(fs)
	ctx := context.Background()

	session := makeClaudeSession(t, fs, testutil.TestSessionUUID,
		`{"type":"user","message":{"role":"tool","content":[{"type":"tool_result","is_error":true,"content":"File not found"}]}}`,
	)

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	e := entries[0]
	if !e.IsError {
		t.Error("is_error: expected true")
	}
	if e.ContentPreview == nil || *e.ContentPreview != "File not found" {
		t.Errorf("preview: expected %q, got %v", "File not found", e.ContentPreview)
	}
}

func TestClaudeIndexer_MalformedLineSkip(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewClaudeIndexer(fs)
	ctx := context.Background()

	session := makeClaudeSession(t, fs, testutil.TestSessionUUID,
		`not valid json`,
		`{"type":"user","uuid":"u1","message":{"role":"user","content":"valid line"}}`,
		`{broken json`,
	)

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	// Malformed lines are skipped but still counted for entry_index.
	// Entry at index 1 should be the valid line.
	if len(entries) != 1 {
		t.Fatalf("expected 1 valid entry (2 malformed skipped), got %d", len(entries))
	}
	if entries[0].EntryIndex != 1 {
		t.Errorf("valid entry index: expected 1, got %d", entries[0].EntryIndex)
	}
}

func TestClaudeIndexer_EmptyFile(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewClaudeIndexer(fs)
	ctx := context.Background()

	sid, _ := ingest.NewSessionID(testutil.TestSessionUUID)
	path := "/transcripts/empty.jsonl"
	_ = fs.WriteFile(path, []byte(""), 0644)
	session := ingest.DiscoveredSession{
		SessionID:  sid,
		SourcePath: ingest.ResolvedPath(path),
	}

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries for empty file, got %d", len(entries))
	}
}

func TestClaudeIndexer_TimestampFormats(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewClaudeIndexer(fs)
	ctx := context.Background()

	session := makeClaudeSession(t, fs, testutil.TestSessionUUID,
		// ISO 8601 string timestamp.
		`{"type":"user","timestamp":"2025-01-15T10:00:00Z","message":{"role":"user","content":"test1"}}`,
		// Integer timestamp (unix ms).
		`{"type":"user","timestamp":1705312800000,"message":{"role":"user","content":"test2"}}`,
		// No timestamp.
		`{"type":"user","message":{"role":"user","content":"test3"}}`,
	)

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	// Entry 0: ISO string → should parse to non-nil.
	if entries[0].TimestampMs == nil {
		t.Error("entry[0] timestamp_ms: expected non-nil for ISO timestamp")
	}

	// Entry 1: integer → should parse to non-nil.
	if entries[1].TimestampMs == nil {
		t.Error("entry[1] timestamp_ms: expected non-nil for integer timestamp")
	} else if *entries[1].TimestampMs != 1705312800000 {
		t.Errorf("entry[1] timestamp_ms: expected 1705312800000, got %d", *entries[1].TimestampMs)
	}

	// Entry 2: missing → nil.
	if entries[2].TimestampMs != nil {
		t.Errorf("entry[2] timestamp_ms: expected nil, got %v", entries[2].TimestampMs)
	}
}

func TestClaudeIndexer_PreviewTruncation(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewClaudeIndexer(fs)
	ctx := context.Background()

	// Create a user message with content > 2000 chars to test truncation.
	longContent := strings.Repeat("A", 2500)
	line := fmt.Sprintf(`{"type":"user","message":{"role":"user","content":%q}}`, longContent)
	session := makeClaudeSession(t, fs, testutil.TestSessionUUID, line)

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	if entries[0].ContentPreview == nil {
		t.Fatal("content_preview: expected non-nil")
	}
	if len(*entries[0].ContentPreview) != defaults.ContentPreviewLimit {
		t.Errorf("content_preview length: expected %d, got %d", defaults.ContentPreviewLimit, len(*entries[0].ContentPreview))
	}
}

func TestClaudeIndexer_ThinkingOnlyEntry(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewClaudeIndexer(fs)
	ctx := context.Background()

	session := makeClaudeSession(t, fs, testutil.TestSessionUUID,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","text":"Let me think about this..."}]}}`,
	)

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	e := entries[0]
	if !e.HasThinking {
		t.Error("has_thinking: expected true")
	}
	if e.EntryType != ingest.EntryTypeThinking {
		t.Errorf("type: expected %s, got %s", ingest.EntryTypeThinking, e.EntryType)
	}
}

// Claude transcripts put thinking trace text in a "thinking" field, not "text".
// Older fixtures and historical transcripts use "text"; production now uses
// "thinking". Verify both formats populate ContentPreview so the depth=0 entry
// survives the UI's "no content + no tools" filter.
func TestClaudeIndexer_ThinkingFieldPopulatesPreview(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewClaudeIndexer(fs, ingest.WithClaudeFullDepth(true))
	ctx := context.Background()

	session := makeClaudeSession(t, fs, testutil.TestSessionUUID,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"Let me analyze this step by step."}]}}`,
	)

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries (depth=0 parent + depth=1 thinking), got %d", len(entries))
	}

	parent := entries[0]
	if !parent.HasThinking {
		t.Error("parent has_thinking: expected true")
	}
	if parent.ContentPreview == nil || *parent.ContentPreview == "" {
		t.Error("parent content_preview: expected thinking text, got empty")
	}

	child := entries[1]
	if child.Depth != 1 {
		t.Errorf("child depth: expected 1, got %d", child.Depth)
	}
	if !child.HasThinking {
		t.Error("child has_thinking: expected true")
	}
	if child.ContentPreview == nil || *child.ContentPreview == "" {
		t.Error("child content_preview: expected thinking text, got empty")
	}
}

func TestClaudeIndexer_RawByteLength(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewClaudeIndexer(fs)
	ctx := context.Background()

	line := `{"type":"user","message":{"role":"user","content":"hello"}}`
	session := makeClaudeSession(t, fs, testutil.TestSessionUUID, line)

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	if entries[0].RawByteLength == nil {
		t.Fatal("raw_byte_length: expected non-nil")
	}
	if *entries[0].RawByteLength != len(line) {
		t.Errorf("raw_byte_length: expected %d, got %d", len(line), *entries[0].RawByteLength)
	}
}

func TestClaudeIndexer_MixedContent(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewClaudeIndexer(fs)
	ctx := context.Background()

	// A single assistant message with all three content block types:
	// thinking, text, and tool_use.
	session := makeClaudeSession(t, fs, testutil.TestSessionUUID,
		`{"type":"assistant","uuid":"a1","timestamp":"2025-01-15T10:00:00Z","message":{"role":"assistant","content":[{"type":"thinking","text":"I need to read the file before modifying it."},{"type":"text","text":"Let me check that file for you."},{"type":"tool_use","name":"Edit","input":{"file":"main.go"}}],"usage":{"input_tokens":200,"output_tokens":80}}}`,
	)

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	e := entries[0]

	// Thinking block detected.
	if !e.HasThinking {
		t.Error("has_thinking: expected true")
	}

	// Tool use block detected.
	if !e.HasToolUse {
		t.Error("has_tool_use: expected true")
	}

	// Tool name captured in CSV.
	if e.ToolNamesCSV == nil || *e.ToolNamesCSV != "Edit" {
		t.Errorf("tool_names_csv: expected %q, got %v", "Edit", e.ToolNamesCSV)
	}

	// Entry type refined to tool_use (tool_use takes priority over thinking).
	if e.EntryType != ingest.EntryTypeToolUse {
		t.Errorf("type: expected %s, got %s", ingest.EntryTypeToolUse, e.EntryType)
	}

	// Role is assistant.
	if e.Role != ingest.RoleAssistant {
		t.Errorf("role: expected %s, got %s", ingest.RoleAssistant, e.Role)
	}

	// Content preview taken from text block.
	if e.ContentPreview == nil || *e.ContentPreview != "Let me check that file for you." {
		t.Errorf("preview: expected %q, got %v", "Let me check that file for you.", e.ContentPreview)
	}

	// Tokens from usage.
	if e.TokensIn == nil || *e.TokensIn != 200 {
		t.Errorf("tokens_in: expected 200, got %v", e.TokensIn)
	}
	if e.TokensOut == nil || *e.TokensOut != 80 {
		t.Errorf("tokens_out: expected 80, got %v", e.TokensOut)
	}
}

func TestClaudeIndexer_ProviderSet(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewClaudeIndexer(fs)
	ctx := context.Background()

	session := makeClaudeSession(t, fs, testutil.TestSessionUUID,
		`{"type":"user","message":{"role":"user","content":"test"}}`,
	)

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Harness != defaults.HarnessClaudeCode {
		t.Errorf("provider: expected %s, got %s", defaults.HarnessClaudeCode, entries[0].Harness)
	}
}

// TestClaudeIndexer_SystemReminderReclassified verifies that a user-role entry
// whose content is entirely a <system-reminder> block is reclassified to
// role=system and entry_type=system.
func TestClaudeIndexer_SystemReminderReclassified(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewClaudeIndexer(fs)
	ctx := context.Background()

	content := ingest.TagSystemReminder + "You are a helpful assistant. Follow these instructions." + ingest.TagSystemReminderClose
	line := fmt.Sprintf(`{"type":"user","uuid":"u1","message":{"role":"user","content":%q}}`, content)
	session := makeClaudeSession(t, fs, testutil.TestSessionUUID, line)

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Role != ingest.RoleSystem {
		t.Errorf("role: expected %s, got %s", ingest.RoleSystem, e.Role)
	}
	if e.EntryType != ingest.EntryTypeSystem {
		t.Errorf("entry_type: expected %s, got %s", ingest.EntryTypeSystem, e.EntryType)
	}
}

// TestClaudeIndexer_CompactionMessageReclassified verifies that a user-role entry
// whose content is a Claude Code compaction/context-continuation message is
// reclassified to role=system and entry_type=system.
func TestClaudeIndexer_CompactionMessageReclassified(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewClaudeIndexer(fs)
	ctx := context.Background()

	content := ingest.CompactionPrefix + "\n\nAnalysis:\nThe agent was implementing tests..."
	line := fmt.Sprintf(`{"type":"user","uuid":"u1","message":{"role":"user","content":%q}}`, content)
	session := makeClaudeSession(t, fs, testutil.TestSessionUUID, line)

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Role != ingest.RoleSystem {
		t.Errorf("role: expected %s (compaction→system), got %s", ingest.RoleSystem, e.Role)
	}
	if e.EntryType != ingest.EntryTypeSystem {
		t.Errorf("entry_type: expected %s, got %s", ingest.EntryTypeSystem, e.EntryType)
	}
}

// TestClaudeIndexer_TaskNotificationReclassified verifies that a user-role entry
// whose content is entirely a <task-notification> block is reclassified to
// role=system and entry_type=system.
func TestClaudeIndexer_TaskNotificationReclassified(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewClaudeIndexer(fs)
	ctx := context.Background()

	content := ingest.TagTaskNotification + "Task completed successfully." + ingest.TagTaskNotificationClose
	line := fmt.Sprintf(`{"type":"user","uuid":"u1","message":{"role":"user","content":%q}}`, content)
	session := makeClaudeSession(t, fs, testutil.TestSessionUUID, line)

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Role != ingest.RoleSystem {
		t.Errorf("role: expected %s, got %s", ingest.RoleSystem, e.Role)
	}
	if e.EntryType != ingest.EntryTypeSystem {
		t.Errorf("entry_type: expected %s, got %s", ingest.EntryTypeSystem, e.EntryType)
	}
}

// TestClaudeIndexer_BuiltinExitCommandReclassified verifies that a user-role entry
// whose content is <command-name>/exit</command-name> (a BuiltinCommand) is
// reclassified to role=system and entry_type=system.
func TestClaudeIndexer_BuiltinExitCommandReclassified(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewClaudeIndexer(fs)
	ctx := context.Background()

	content := "<command-name>/exit</command-name>"
	line := fmt.Sprintf(`{"type":"user","uuid":"u1","message":{"role":"user","content":%q}}`, content)
	session := makeClaudeSession(t, fs, testutil.TestSessionUUID, line)

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Role != ingest.RoleSystem {
		t.Errorf("role: expected %s, got %s", ingest.RoleSystem, e.Role)
	}
	if e.EntryType != ingest.EntryTypeSystem {
		t.Errorf("entry_type: expected %s, got %s", ingest.EntryTypeSystem, e.EntryType)
	}
	// No command metadata in Extra — builtins are not skill invocations.
	if e.Extra != nil {
		t.Errorf("extra: expected nil for builtin command, got %q", *e.Extra)
	}
}

// TestClaudeIndexer_BuiltinCompactCommandReclassified verifies that /compact
// (another BuiltinCommand) is reclassified to role=system.
func TestClaudeIndexer_BuiltinCompactCommandReclassified(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewClaudeIndexer(fs)
	ctx := context.Background()

	content := "<command-name>/compact</command-name>"
	line := fmt.Sprintf(`{"type":"user","uuid":"u1","message":{"role":"user","content":%q}}`, content)
	session := makeClaudeSession(t, fs, testutil.TestSessionUUID, line)

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Role != ingest.RoleSystem {
		t.Errorf("role: expected %s, got %s", ingest.RoleSystem, e.Role)
	}
	if e.EntryType != ingest.EntryTypeSystem {
		t.Errorf("entry_type: expected %s, got %s", ingest.EntryTypeSystem, e.EntryType)
	}
}

// TestClaudeIndexer_SkillInvocationWithArgs verifies that a user-role entry
// with <command-name>/aura:epoch</command-name><command-args>Fix the web UI</command-args>
// stays role=user and has command metadata in Extra JSON.
func TestClaudeIndexer_SkillInvocationWithArgs(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewClaudeIndexer(fs)
	ctx := context.Background()

	content := "<command-name>/aura:epoch</command-name><command-args>Fix the web UI</command-args>"
	line := fmt.Sprintf(`{"type":"user","uuid":"u1","message":{"role":"user","content":%q}}`, content)
	session := makeClaudeSession(t, fs, testutil.TestSessionUUID, line)

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	// Skill invocations stay role=user.
	if e.Role != ingest.RoleUser {
		t.Errorf("role: expected %s, got %s", ingest.RoleUser, e.Role)
	}
	if e.EntryType != ingest.EntryTypeText {
		t.Errorf("entry_type: expected %s, got %s", ingest.EntryTypeText, e.EntryType)
	}
	// Command metadata must be present in Extra.
	if e.Extra == nil {
		t.Fatal("extra: expected non-nil for skill invocation")
	}
	var extra map[string]any
	if err := json.Unmarshal([]byte(*e.Extra), &extra); err != nil {
		t.Fatalf("extra: invalid JSON: %v", err)
	}
	if got, ok := extra["command_name"].(string); !ok || got != "/aura:epoch" {
		t.Errorf("extra.command_name: expected %q, got %v", "/aura:epoch", extra["command_name"])
	}
	if got, ok := extra["command_args"].(string); !ok || got != "Fix the web UI" {
		t.Errorf("extra.command_args: expected %q, got %v", "Fix the web UI", extra["command_args"])
	}
}

// TestClaudeIndexer_SkillInvocationNoArgs verifies that a skill invocation without
// <command-args> stays role=user and has command_name in Extra but no command_args key.
func TestClaudeIndexer_SkillInvocationNoArgs(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewClaudeIndexer(fs)
	ctx := context.Background()

	content := "<command-name>/aura:epoch</command-name>"
	line := fmt.Sprintf(`{"type":"user","uuid":"u1","message":{"role":"user","content":%q}}`, content)
	session := makeClaudeSession(t, fs, testutil.TestSessionUUID, line)

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Role != ingest.RoleUser {
		t.Errorf("role: expected %s, got %s", ingest.RoleUser, e.Role)
	}
	if e.Extra == nil {
		t.Fatal("extra: expected non-nil for skill invocation")
	}
	var extra map[string]any
	if err := json.Unmarshal([]byte(*e.Extra), &extra); err != nil {
		t.Fatalf("extra: invalid JSON: %v", err)
	}
	if got, ok := extra["command_name"].(string); !ok || got != "/aura:epoch" {
		t.Errorf("extra.command_name: expected %q, got %v", "/aura:epoch", extra["command_name"])
	}
	// No command_args key when absent.
	if _, present := extra["command_args"]; present {
		t.Errorf("extra.command_args: expected absent, but key is present with value %v", extra["command_args"])
	}
}

// TestClaudeIndexer_MixedSystemReminderStaysUser verifies that a user-role entry
// whose content starts with a <system-reminder> tag but also contains additional
// user text after the closing tag is NOT reclassified to role=system. Only content
// that is ENTIRELY wrapped in the tag should be reclassified.
func TestClaudeIndexer_MixedSystemReminderStaysUser(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewClaudeIndexer(fs)
	ctx := context.Background()

	// Mixed content: system-reminder block followed by actual user text.
	content := ingest.TagSystemReminder + "The task tools are loaded." + ingest.TagSystemReminderClose + "\nSome actual user text here"
	line := fmt.Sprintf(`{"type":"user","uuid":"u1","message":{"role":"user","content":%q}}`, content)
	session := makeClaudeSession(t, fs, testutil.TestSessionUUID, line)

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	// Mixed content must stay as role=user — NOT reclassified to system.
	if e.Role != ingest.RoleUser {
		t.Errorf("role: expected %s (mixed content must not be reclassified), got %s", ingest.RoleUser, e.Role)
	}
	if e.EntryType != ingest.EntryTypeText {
		t.Errorf("entry_type: expected %s (mixed content must not be reclassified), got %s", ingest.EntryTypeText, e.EntryType)
	}
}

// TestClaudeIndexer_TaskNotificationWithTrailingHarnessTextReclassified verifies
// that a <task-notification> entry with Claude Code's appended harness text
// (e.g. "Read the output file to retrieve the result: /tmp/...") after the
// closing tag is reclassified to role=system. The trailing text is harness-
// generated, not user content, so the whole entry is system-injected.
func TestClaudeIndexer_TaskNotificationWithTrailingHarnessTextReclassified(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewClaudeIndexer(fs)
	ctx := context.Background()

	content := ingest.TagTaskNotification + "\n<task-id>blnj364z8</task-id>\n<task-title>Fix the bug</task-title>\n" + ingest.TagTaskNotificationClose + "\nRead the output file to retrieve the result: /tmp/out-123.txt"
	line := fmt.Sprintf(`{"type":"user","uuid":"u1","message":{"role":"user","content":%q}}`, content)
	session := makeClaudeSession(t, fs, testutil.TestSessionUUID, line)

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	// Entry with trailing harness text must be reclassified to role=system.
	if e.Role != ingest.RoleSystem {
		t.Errorf("role: expected %s (task-notification with trailing harness text must be reclassified), got %s", ingest.RoleSystem, e.Role)
	}
	if e.EntryType != ingest.EntryTypeSystem {
		t.Errorf("entry_type: expected %s, got %s", ingest.EntryTypeSystem, e.EntryType)
	}
}

// TestClaudeIndexer_SkillBodyInjectionReclassified verifies that the expanded
// skill content entry emitted by Claude Code when a user invokes a skill is
// reclassified to role=system. These entries start with "Base directory for
// this skill:" and are harness-injected, not user-authored.
func TestClaudeIndexer_SkillBodyInjectionReclassified(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewClaudeIndexer(fs)
	ctx := context.Background()

	content := ingest.PrefixSkillBody + " /home/user/.claude/plugins/aura/skills\n\n# Epoch Orchestrator\n\nThis skill orchestrates the full 12-phase audit-trail workflow..."
	line := fmt.Sprintf(`{"type":"user","uuid":"u1","message":{"role":"user","content":%q}}`, content)
	session := makeClaudeSession(t, fs, testutil.TestSessionUUID, line)

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	// Skill body injection must be reclassified to role=system.
	if e.Role != ingest.RoleSystem {
		t.Errorf("role: expected %s (skill body injection must be reclassified), got %s", ingest.RoleSystem, e.Role)
	}
	if e.EntryType != ingest.EntryTypeSystem {
		t.Errorf("entry_type: expected %s, got %s", ingest.EntryTypeSystem, e.EntryType)
	}
}

// TestClaudeIndexer_RegularUserTextUnchanged verifies that regular user text
// is NOT reclassified and has no Extra command metadata.
func TestClaudeIndexer_RegularUserTextUnchanged(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewClaudeIndexer(fs)
	ctx := context.Background()

	session := makeClaudeSession(t, fs, testutil.TestSessionUUID,
		`{"type":"user","uuid":"u1","message":{"role":"user","content":"Help me refactor this function"}}`,
	)

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Role != ingest.RoleUser {
		t.Errorf("role: expected %s, got %s", ingest.RoleUser, e.Role)
	}
	if e.EntryType != ingest.EntryTypeText {
		t.Errorf("entry_type: expected %s, got %s", ingest.EntryTypeText, e.EntryType)
	}
	if e.Extra != nil {
		t.Errorf("extra: expected nil for regular user text, got %q", *e.Extra)
	}
}

// TestClaudeIndexer_IsMetaReclassified verifies that Claude Code's own `isMeta`
// marker reclassifies a user-role entry to role=system, whatever its content.
//
// The three shapes below all reached the viewer as human prompts before isMeta
// was read: they carry no <system-reminder>/<task-notification> wrapper and do
// not start with "Base directory for this skill:", so every content heuristic
// misses them. In one real 919-line session, 33 of 66 "user" turns were these.
func TestClaudeIndexer_IsMetaReclassified(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		content string
	}{
		{
			name:    "image coordinate note for an image the agent rendered",
			content: "[Image: original 2880x1960, displayed at 2000x1361. Multiply coordinates by 1.44 to map to original image.]",
		},
		{
			name:    "skill body with no Base-directory prefix",
			content: "Draw as the engineer who has to live with the decision, not as a decorator.",
		},
		{
			name:    "usage limit notice",
			content: "Your claude.ai usage limit has reset. Continue the task you were working on.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fs := testutil.NewMemFS()
			idx := ingest.NewClaudeIndexer(fs)
			ctx := context.Background()

			line := fmt.Sprintf(`{"type":"user","uuid":"u1","isMeta":true,"message":{"role":"user","content":%q}}`, tc.content)
			session := makeClaudeSession(t, fs, testutil.TestSessionUUID, line)

			entries, err := idx.IndexTranscript(ctx, session)
			if err != nil {
				t.Fatalf("IndexTranscript: %v", err)
			}
			if len(entries) != 1 {
				t.Fatalf("expected 1 entry, got %d", len(entries))
			}
			e := entries[0]
			if e.Role != ingest.RoleSystem {
				t.Errorf("role: expected %s (isMeta entries are harness-injected), got %s", ingest.RoleSystem, e.Role)
			}
			if e.EntryType != ingest.EntryTypeSystem {
				t.Errorf("entry_type: expected %s, got %s", ingest.EntryTypeSystem, e.EntryType)
			}
		})
	}
}

// TestClaudeIndexer_IsMetaNotSetStaysUser is the guard against over-filtering:
// an entry the human actually typed carries no isMeta (or an explicit false),
// and text that merely resembles a harness note must still arrive as a user turn.
func TestClaudeIndexer_IsMetaNotSetStaysUser(t *testing.T) {
	t.Parallel()
	// A human quoting the harness's own image note back at the agent.
	const content = "[Image: original 100x100] — why does this keep showing up as my message?"
	cases := []struct {
		name string
		line string
	}{
		{
			name: "isMeta absent",
			line: fmt.Sprintf(`{"type":"user","uuid":"u1","message":{"role":"user","content":%q}}`, content),
		},
		{
			name: "isMeta explicitly false",
			line: fmt.Sprintf(`{"type":"user","uuid":"u1","isMeta":false,"message":{"role":"user","content":%q}}`, content),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fs := testutil.NewMemFS()
			idx := ingest.NewClaudeIndexer(fs)
			session := makeClaudeSession(t, fs, testutil.TestSessionUUID, tc.line)

			entries, err := idx.IndexTranscript(context.Background(), session)
			if err != nil {
				t.Fatalf("IndexTranscript: %v", err)
			}
			if len(entries) != 1 {
				t.Fatalf("expected 1 entry, got %d", len(entries))
			}
			if entries[0].Role != ingest.RoleUser {
				t.Errorf("role: expected %s (isMeta not set means the human typed it), got %s", ingest.RoleUser, entries[0].Role)
			}
		})
	}
}
