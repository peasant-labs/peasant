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

// setupOpenCodeFixture creates a minimal OpenCode file tree in MemFS and returns
// the DiscoveredSession pointing at the session JSON.
func setupOpenCodeFixture(t *testing.T, fs *testutil.MemFS, sessionID, projectHash string) ingest.DiscoveredSession {
	t.Helper()
	root := "/opencode-store"

	// Session JSON — under storage/session/.
	sesPath := fmt.Sprintf("%s/storage/session/%s/%s.json", root, projectHash, sessionID)
	sesJSON := fmt.Sprintf(`{"id":%q,"version":"0.1.0","projectID":%q,"directory":"/home/test/project","time":{"created":1700000000000,"updated":1700000060000}}`, sessionID, projectHash)
	if err := fs.WriteFile(sesPath, []byte(sesJSON), 0644); err != nil {
		t.Fatalf("write session: %v", err)
	}

	sid, err := ingest.NewSessionID(sessionID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	return ingest.DiscoveredSession{
		SessionID:    sid,
		Harness:      defaults.HarnessOpenCode,
		SourcePath:   ingest.ResolvedPath(sesPath),
		SourceFormat: ingest.SourceFormatJSON,
		OriginalRoot: ingest.ResolvedPath(root),
	}
}

func addOpenCodeMessage(t *testing.T, fs *testutil.MemFS, sessionID, msgID, role string, tokensIn, tokensOut int) {
	t.Helper()
	root := "/opencode-store"
	msgPath := fmt.Sprintf("%s/storage/message/%s/%s.json", root, sessionID, msgID)
	tokensJSON := ""
	if tokensIn > 0 || tokensOut > 0 {
		tokensJSON = fmt.Sprintf(`,"tokens":{"input":%d,"output":%d}`, tokensIn, tokensOut)
	}
	msgJSON := fmt.Sprintf(`{"id":%q,"sessionID":%q,"role":%q,"time":{"created":1700000001000,"completed":1700000002000}%s,"content":"Hello from %s"}`, msgID, sessionID, role, tokensJSON, role)
	if err := fs.WriteFile(msgPath, []byte(msgJSON), 0644); err != nil {
		t.Fatalf("write message: %v", err)
	}
}

func addOpenCodePart(t *testing.T, fs *testutil.MemFS, msgID, partID string) {
	t.Helper()
	root := "/opencode-store"
	partPath := fmt.Sprintf("%s/storage/part/%s/%s.json", root, msgID, partID)
	partJSON := fmt.Sprintf(`{"id":%q,"type":"tool_use","name":"Read"}`, partID)
	if err := fs.WriteFile(partPath, []byte(partJSON), 0644); err != nil {
		t.Fatalf("write part: %v", err)
	}
}

func TestOpenCodeIndexer_BasicMessages(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewOpenCodeIndexer(fs)
	ctx := context.Background()

	sesID := testutil.TestOpenCodeSesID
	session := setupOpenCodeFixture(t, fs, sesID, "proj1")

	addOpenCodeMessage(t, fs, sesID, "msg_001abc", "user", 0, 0)
	addOpenCodeMessage(t, fs, sesID, "msg_002def", "assistant", 500, 200)

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	// Entry 0 (alphabetically first: msg_001abc).
	e0 := entries[0]
	if e0.Role != ingest.RoleUser {
		t.Errorf("e0 role: expected %s, got %s", ingest.RoleUser, e0.Role)
	}
	if e0.EntryType != ingest.EntryTypeText {
		t.Errorf("e0 type: expected %s, got %s", ingest.EntryTypeText, e0.EntryType)
	}
	if e0.Harness != defaults.HarnessOpenCode {
		t.Errorf("e0 provider: expected %s, got %s", defaults.HarnessOpenCode, e0.Harness)
	}
	if e0.ContentPreview == nil || *e0.ContentPreview != "Hello from user" {
		t.Errorf("e0 preview: expected %q, got %v", "Hello from user", e0.ContentPreview)
	}
	if e0.EntryID == nil || *e0.EntryID != "msg_001abc" {
		t.Errorf("e0 entry_id: expected %q, got %v", "msg_001abc", e0.EntryID)
	}

	// Entry 1 (msg_002def).
	e1 := entries[1]
	if e1.Role != ingest.RoleAssistant {
		t.Errorf("e1 role: expected %s, got %s", ingest.RoleAssistant, e1.Role)
	}
	if e1.TokensIn == nil || *e1.TokensIn != 500 {
		t.Errorf("e1 tokens_in: expected 500, got %v", e1.TokensIn)
	}
	if e1.TokensOut == nil || *e1.TokensOut != 200 {
		t.Errorf("e1 tokens_out: expected 200, got %v", e1.TokensOut)
	}
}

func TestOpenCodeIndexer_ToolUseWithParts(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewOpenCodeIndexer(fs)
	ctx := context.Background()

	sesID := testutil.TestOpenCodeSesID
	session := setupOpenCodeFixture(t, fs, sesID, "proj1")

	addOpenCodeMessage(t, fs, sesID, "msg_001abc", "assistant", 300, 100)
	addOpenCodePart(t, fs, "msg_001abc", "part_001")
	addOpenCodePart(t, fs, "msg_001abc", "part_002")

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	e := entries[0]
	if !e.HasToolUse {
		t.Error("has_tool_use: expected true (parts exist)")
	}
	if e.EntryType != ingest.EntryTypeToolUse {
		t.Errorf("type: expected %s, got %s", ingest.EntryTypeToolUse, e.EntryType)
	}
}

func TestOpenCodeIndexer_NoMessageDir(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewOpenCodeIndexer(fs)
	ctx := context.Background()

	// Session exists but no message directory.
	sesID := testutil.TestOpenCodeSesID
	session := setupOpenCodeFixture(t, fs, sesID, "proj1")

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: unexpected error: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(entries))
	}
}

func TestOpenCodeIndexer_TimestampExtraction(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewOpenCodeIndexer(fs)
	ctx := context.Background()

	sesID := testutil.TestOpenCodeSesID
	session := setupOpenCodeFixture(t, fs, sesID, "proj1")
	addOpenCodeMessage(t, fs, sesID, "msg_001abc", "user", 0, 0)

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	// Message fixture has time.created = 1700000001000.
	if entries[0].TimestampMs == nil {
		t.Fatal("timestamp_ms: expected non-nil")
	}
	if *entries[0].TimestampMs != 1700000001000 {
		t.Errorf("timestamp_ms: expected 1700000001000, got %d", *entries[0].TimestampMs)
	}
}

func TestOpenCodeIndexer_OrderByFilename(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewOpenCodeIndexer(fs)
	ctx := context.Background()

	sesID := testutil.TestOpenCodeSesID
	session := setupOpenCodeFixture(t, fs, sesID, "proj1")

	// Add messages out of alphabetical order.
	addOpenCodeMessage(t, fs, sesID, "msg_003ghi", "assistant", 0, 0)
	addOpenCodeMessage(t, fs, sesID, "msg_001abc", "user", 0, 0)
	addOpenCodeMessage(t, fs, sesID, "msg_002def", "assistant", 0, 0)

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	// Should be ordered alphabetically by filename.
	expectedIDs := []string{"msg_001abc", "msg_002def", "msg_003ghi"}
	for i, eid := range expectedIDs {
		if entries[i].EntryID == nil || *entries[i].EntryID != eid {
			t.Errorf("entry[%d] id: expected %q, got %v", i, eid, entries[i].EntryID)
		}
		if entries[i].EntryIndex != i {
			t.Errorf("entry[%d] index: expected %d, got %d", i, i, entries[i].EntryIndex)
		}
	}
}

func TestOpenCodeIndexer_MalformedJSON(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewOpenCodeIndexer(fs)
	ctx := context.Background()

	sesID := testutil.TestOpenCodeSesID
	session := setupOpenCodeFixture(t, fs, sesID, "proj1")

	// Add one valid message.
	addOpenCodeMessage(t, fs, sesID, "msg_001abc", "user", 0, 0)

	// Write a malformed JSON file directly to the message directory.
	malformedPath := fmt.Sprintf("/opencode-store/storage/message/%s/msg_002bad.json", sesID)
	if err := fs.WriteFile(malformedPath, []byte(`{this is not valid json`), 0644); err != nil {
		t.Fatalf("write malformed message: %v", err)
	}

	// Add another valid message after the malformed one (alphabetically).
	addOpenCodeMessage(t, fs, sesID, "msg_003ghi", "assistant", 100, 50)

	// The indexer must not panic and must gracefully skip the malformed entry.
	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: unexpected error: %v", err)
	}

	// Only the two valid messages should be returned.
	if len(entries) != 2 {
		t.Fatalf("expected 2 valid entries (1 malformed skipped), got %d", len(entries))
	}

	// Verify the valid entries are present and correctly ordered.
	if entries[0].EntryID == nil || *entries[0].EntryID != "msg_001abc" {
		t.Errorf("entry[0] id: expected %q, got %v", "msg_001abc", entries[0].EntryID)
	}
	if entries[0].Role != ingest.RoleUser {
		t.Errorf("entry[0] role: expected %s, got %s", ingest.RoleUser, entries[0].Role)
	}

	if entries[1].EntryID == nil || *entries[1].EntryID != "msg_003ghi" {
		t.Errorf("entry[1] id: expected %q, got %v", "msg_003ghi", entries[1].EntryID)
	}
	if entries[1].Role != ingest.RoleAssistant {
		t.Errorf("entry[1] role: expected %s, got %s", ingest.RoleAssistant, entries[1].Role)
	}
	if entries[1].Harness != defaults.HarnessOpenCode {
		t.Errorf("entry[1] provider: expected %s, got %s", defaults.HarnessOpenCode, entries[1].Harness)
	}
}

func TestOpenCodeIndexer_RawByteLength(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewOpenCodeIndexer(fs)
	ctx := context.Background()

	sesID := testutil.TestOpenCodeSesID
	session := setupOpenCodeFixture(t, fs, sesID, "proj1")
	addOpenCodeMessage(t, fs, sesID, "msg_001abc", "user", 0, 0)

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
	if *entries[0].RawByteLength <= 0 {
		t.Errorf("raw_byte_length: expected > 0, got %d", *entries[0].RawByteLength)
	}
}

// addOpenCodeSkillPart writes a part file representing a skill tool invocation.
// Part shape: {"id": partID, "type": "tool", "tool": "skill", "state": {"input": {"name": skillName}}}
func addOpenCodeSkillPart(t *testing.T, fs *testutil.MemFS, msgID, partID, skillName string) {
	t.Helper()
	root := "/opencode-store"
	partData := map[string]any{
		"id":   partID,
		"type": "tool",
		"tool": "skill",
		"state": map[string]any{
			"input": map[string]any{
				"name": skillName,
			},
		},
	}
	b, err := json.Marshal(partData)
	if err != nil {
		t.Fatalf("marshal skill part: %v", err)
	}
	partPath := fmt.Sprintf("%s/storage/part/%s/%s.json", root, msgID, partID)
	if err := fs.WriteFile(partPath, b, 0644); err != nil {
		t.Fatalf("write skill part: %v", err)
	}
}

// addOpenCodeSystemPart writes a part file with the given structural system type.
// Supported types: "compaction", "subtask", "agent".
func addOpenCodeSystemPart(t *testing.T, fs *testutil.MemFS, msgID, partID, partType string) {
	t.Helper()
	root := "/opencode-store"
	partData := map[string]any{
		"id":   partID,
		"type": partType,
	}
	b, err := json.Marshal(partData)
	if err != nil {
		t.Fatalf("marshal system part: %v", err)
	}
	partPath := fmt.Sprintf("%s/storage/part/%s/%s.json", root, msgID, partID)
	if err := fs.WriteFile(partPath, b, 0644); err != nil {
		t.Fatalf("write system part: %v", err)
	}
}

// TestOpenCodeIndexer_SkillPartStaysUser verifies that a message with a skill tool
// part (type="tool", tool="skill") stays role=user and carries command_name in Extra.
func TestOpenCodeIndexer_SkillPartStaysUser(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewOpenCodeIndexer(fs)
	ctx := context.Background()

	sesID := testutil.TestOpenCodeSesID
	session := setupOpenCodeFixture(t, fs, sesID, "proj1")

	// User message with a skill part.
	addOpenCodeMessage(t, fs, sesID, "msg_001abc", "user", 0, 0)
	addOpenCodeSkillPart(t, fs, "msg_001abc", "part_001", "epoch")

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
		t.Errorf("role: expected %s (skill stays user), got %s", ingest.RoleUser, e.Role)
	}
	if e.EntryType != ingest.EntryTypeText {
		t.Errorf("entry_type: expected %s, got %s", ingest.EntryTypeText, e.EntryType)
	}
	// command_name must be in Extra.
	if e.Extra == nil {
		t.Fatal("Extra: expected non-nil for skill invocation")
	}
	var extra map[string]any
	if err := json.Unmarshal([]byte(*e.Extra), &extra); err != nil {
		t.Fatalf("Extra: invalid JSON: %v", err)
	}
	if cn, ok := extra["command_name"]; !ok {
		t.Error("Extra: missing command_name")
	} else if cn != "/epoch" {
		t.Errorf("Extra command_name: expected %q, got %v", "/epoch", cn)
	}
}

// TestOpenCodeIndexer_SkillPartWithNamespaceSlash verifies skill names that
// already contain a slash (e.g. "aura:epoch") are correctly prefixed.
func TestOpenCodeIndexer_SkillPartNamespaced(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewOpenCodeIndexer(fs)
	ctx := context.Background()

	sesID := testutil.TestOpenCodeSesID
	session := setupOpenCodeFixture(t, fs, sesID, "proj1")

	addOpenCodeMessage(t, fs, sesID, "msg_001abc", "user", 0, 0)
	addOpenCodeSkillPart(t, fs, "msg_001abc", "part_001", "aura:epoch")

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
		t.Fatal("Extra: expected non-nil")
	}
	var extra map[string]any
	if err := json.Unmarshal([]byte(*e.Extra), &extra); err != nil {
		t.Fatalf("Extra: invalid JSON: %v", err)
	}
	if cn := extra["command_name"]; cn != "/aura:epoch" {
		t.Errorf("Extra command_name: expected %q, got %v", "/aura:epoch", cn)
	}
}

// TestOpenCodeIndexer_CompactionPartBecomesSystem verifies that a message with a
// part of type="compaction" is reclassified to role=system, entry_type=system.
func TestOpenCodeIndexer_CompactionPartBecomesSystem(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewOpenCodeIndexer(fs)
	ctx := context.Background()

	sesID := testutil.TestOpenCodeSesID
	session := setupOpenCodeFixture(t, fs, sesID, "proj1")

	// Assistant message that turns out to be a compaction.
	addOpenCodeMessage(t, fs, sesID, "msg_001abc", "assistant", 300, 100)
	addOpenCodeSystemPart(t, fs, "msg_001abc", "part_001", "compaction")

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

// TestOpenCodeIndexer_SubtaskPartBecomesSystem verifies type="subtask" reclassification.
func TestOpenCodeIndexer_SubtaskPartBecomesSystem(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewOpenCodeIndexer(fs)
	ctx := context.Background()

	sesID := testutil.TestOpenCodeSesID
	session := setupOpenCodeFixture(t, fs, sesID, "proj1")

	addOpenCodeMessage(t, fs, sesID, "msg_001abc", "assistant", 100, 50)
	addOpenCodeSystemPart(t, fs, "msg_001abc", "part_001", "subtask")

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	e := entries[0]
	if e.Role != ingest.RoleSystem {
		t.Errorf("role: expected %s (subtask→system), got %s", ingest.RoleSystem, e.Role)
	}
	if e.EntryType != ingest.EntryTypeSystem {
		t.Errorf("entry_type: expected %s, got %s", ingest.EntryTypeSystem, e.EntryType)
	}
}

// TestOpenCodeIndexer_AgentPartBecomesSystem verifies type="agent" reclassification.
func TestOpenCodeIndexer_AgentPartBecomesSystem(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewOpenCodeIndexer(fs)
	ctx := context.Background()

	sesID := testutil.TestOpenCodeSesID
	session := setupOpenCodeFixture(t, fs, sesID, "proj1")

	addOpenCodeMessage(t, fs, sesID, "msg_001abc", "assistant", 200, 80)
	addOpenCodeSystemPart(t, fs, "msg_001abc", "part_001", "agent")

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	e := entries[0]
	if e.Role != ingest.RoleSystem {
		t.Errorf("role: expected %s (agent→system), got %s", ingest.RoleSystem, e.Role)
	}
	if e.EntryType != ingest.EntryTypeSystem {
		t.Errorf("entry_type: expected %s, got %s", ingest.EntryTypeSystem, e.EntryType)
	}
}

// TestOpenCodeIndexer_RegularToolPartNoReclassification verifies that a regular
// tool part (type="tool", tool="bash") does not trigger system reclassification.
func TestOpenCodeIndexer_RegularToolPartNoReclassification(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewOpenCodeIndexer(fs)
	ctx := context.Background()

	sesID := testutil.TestOpenCodeSesID
	session := setupOpenCodeFixture(t, fs, sesID, "proj1")

	addOpenCodeMessage(t, fs, sesID, "msg_001abc", "assistant", 300, 100)

	// Regular tool part — bash tool, not skill.
	root := "/opencode-store"
	partData := map[string]any{
		"id":   "part_001",
		"type": "tool",
		"tool": "bash",
	}
	b, err := json.Marshal(partData)
	if err != nil {
		t.Fatalf("marshal bash part: %v", err)
	}
	partPath := fmt.Sprintf("%s/storage/part/msg_001abc/part_001.json", root)
	if err := fs.WriteFile(partPath, b, 0644); err != nil {
		t.Fatalf("write bash part: %v", err)
	}

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	e := entries[0]
	// Regular tool parts should NOT reclassify.
	if e.Role == ingest.RoleSystem {
		t.Errorf("role: expected non-system for regular tool part, got %s", e.Role)
	}
	// Extra should be nil (no skill metadata).
	if e.Extra != nil {
		t.Errorf("Extra: expected nil for regular tool part, got %q", *e.Extra)
	}
}

// TestOpenCodeIndexer_SystemPartTakesPrecedenceOverSkill verifies that when a
// message has both a system-structural part and a skill part, system wins.
func TestOpenCodeIndexer_SystemPartTakesPrecedenceOverSkill(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewOpenCodeIndexer(fs)
	ctx := context.Background()

	sesID := testutil.TestOpenCodeSesID
	session := setupOpenCodeFixture(t, fs, sesID, "proj1")

	addOpenCodeMessage(t, fs, sesID, "msg_001abc", "assistant", 100, 50)
	// Skill part first, then system part.
	addOpenCodeSkillPart(t, fs, "msg_001abc", "part_001", "aura:epoch")
	addOpenCodeSystemPart(t, fs, "msg_001abc", "part_002", "compaction")

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	e := entries[0]
	// System takes precedence.
	if e.Role != ingest.RoleSystem {
		t.Errorf("role: expected %s (system takes precedence), got %s", ingest.RoleSystem, e.Role)
	}
	if e.EntryType != ingest.EntryTypeSystem {
		t.Errorf("entry_type: expected %s, got %s", ingest.EntryTypeSystem, e.EntryType)
	}
}
