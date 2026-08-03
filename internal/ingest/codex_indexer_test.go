package ingest_test

import (
	"context"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// --- helpers ---

// indexCodexFixture runs the CodexIndexer over the default fixture and returns
// the resulting entries. Centralises the indexer setup so each test focuses on
// observable outcomes rather than boilerplate.
func indexCodexFixture(t *testing.T, opts codexFixtureOpts) []schema.SessionEntry {
	t.Helper()
	mfs := testutil.NewMemFS()
	const base = "/home/test/.codex/sessions"
	path := codexRolloutPath(base, "2026", "05", "29", "2026-05-29T21-14-24", opts.sessionID)
	if err := mfs.WriteFile(path, codexRolloutJSONL(opts), 0644); err != nil {
		t.Fatalf("seed rollout: %v", err)
	}
	sid, err := ingest.NewSessionID(opts.sessionID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	ds := ingest.DiscoveredSession{
		SessionID:    sid,
		Harness:      ingest.HarnessCodex,
		SourcePath:   ingest.ResolvedPath(path),
		SourceFormat: ingest.SourceFormatJSONL,
	}
	idx := ingest.NewCodexIndexer(mfs)
	entries, err := idx.IndexTranscript(context.Background(), ds)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	return entries
}

// indexCodexMultiAgentFixture runs the CodexIndexer over the real-shaped
// multi-agent rollout (codexMultiAgentRolloutJSONL) — leading developer-role
// bootstrap messages, injected user-role bootstrap content, a real human
// message, an `exec` tool call, and a `wait_agent` coordination tool call.
func indexCodexMultiAgentFixture(t *testing.T, opts codexFixtureOpts) []schema.SessionEntry {
	t.Helper()
	mfs := testutil.NewMemFS()
	const base = "/home/test/.codex/sessions"
	path := codexRolloutPath(base, "2026", "05", "29", "2026-05-29T21-14-24", opts.sessionID)
	if err := mfs.WriteFile(path, codexMultiAgentRolloutJSONL(opts), 0644); err != nil {
		t.Fatalf("seed rollout: %v", err)
	}
	sid, err := ingest.NewSessionID(opts.sessionID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	ds := ingest.DiscoveredSession{
		SessionID:    sid,
		Harness:      ingest.HarnessCodex,
		SourcePath:   ingest.ResolvedPath(path),
		SourceFormat: ingest.SourceFormatJSONL,
	}
	idx := ingest.NewCodexIndexer(mfs)
	entries, err := idx.IndexTranscript(context.Background(), ds)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	return entries
}

// findEntry returns the first entry matching the predicate, or fails the test.
func findEntry(t *testing.T, entries []schema.SessionEntry, pred func(schema.SessionEntry) bool, desc string) schema.SessionEntry {
	t.Helper()
	for _, e := range entries {
		if pred(e) {
			return e
		}
	}
	t.Fatalf("no entry matched %s (got %d entries)", desc, len(entries))
	return schema.SessionEntry{}
}

// --- tests ---

// TestCodexIndexer_ImplementsInterface guards the TranscriptIndexer contract at
// compile time. The indexer must satisfy the interface or the pipeline won't
// accept it in WithIndexers.
func TestCodexIndexer_ImplementsInterface(t *testing.T) {
	var _ ingest.TranscriptIndexer = (*ingest.CodexIndexer)(nil)
}

// TestCodexIndexer_DispatchTable verifies the full event-type vocabulary
// produces the right (EntryType, Role, flags) for every response_item variant.
// One test guards the entire dispatch so the count + classification of entries
// is asserted together.
func TestCodexIndexer_DispatchTable(t *testing.T) {
	entries := indexCodexFixture(t, defaultCodexFixtureOpts())

	// The default fixture produces these response_items, in order:
	//   1. message  role=user                → text / user
	//   2. reasoning                          → thinking / assistant (HasThinking)
	//   3. message  role=assistant            → text / assistant
	//   4. function_call exec_command         → tool_use / assistant
	//   5. function_call_output (matches #4)  → tool_result / tool
	//   6. custom_tool_call apply_patch       → tool_use / assistant
	//   7. custom_tool_call_output (matches #6) → tool_result / tool
	// session_meta / turn_context / event_msg lines are filtered out.
	if len(entries) != 7 {
		t.Fatalf("entry count: got %d, want 7 (one per response_item, session_meta/turn_context/event_msg skipped)", len(entries))
	}

	want := []struct {
		entryType ingest.EntryType
		role      ingest.Role
	}{
		{ingest.EntryTypeText, ingest.RoleUser},
		{ingest.EntryTypeThinking, ingest.RoleAssistant},
		{ingest.EntryTypeText, ingest.RoleAssistant},
		{ingest.EntryTypeToolUse, ingest.RoleAssistant},
		{ingest.EntryTypeToolResult, ingest.RoleTool},
		{ingest.EntryTypeToolUse, ingest.RoleAssistant},
		{ingest.EntryTypeToolResult, ingest.RoleTool},
	}
	for i, w := range want {
		if entries[i].EntryType != w.entryType || entries[i].Role != w.role {
			t.Errorf("entries[%d]: got (%s, %s), want (%s, %s)",
				i, entries[i].EntryType, entries[i].Role, w.entryType, w.role)
		}
		if entries[i].EntryIndex != i {
			t.Errorf("entries[%d].EntryIndex: got %d, want %d", i, entries[i].EntryIndex, i)
		}
		if entries[i].Harness != ingest.HarnessCodex {
			t.Errorf("entries[%d].Harness: got %q, want %q", i, entries[i].Harness, ingest.HarnessCodex)
		}
		if entries[i].Depth != 0 {
			t.Errorf("entries[%d].Depth: got %d, want 0 (Codex events are atomic; no depth=1 decomposition)", i, entries[i].Depth)
		}
	}
}

// TestCodexIndexer_ToolCallLinkage verifies that function_call /
// function_call_output pairs share their call_id, and same for custom_tool
// calls. Pairing is what lets the session_detail UI render tool input next to
// its output.
func TestCodexIndexer_ToolCallLinkage(t *testing.T) {
	entries := indexCodexFixture(t, defaultCodexFixtureOpts())

	funcCall := findEntry(t, entries, func(e schema.SessionEntry) bool {
		return e.EntryType == ingest.EntryTypeToolUse &&
			e.ToolNamesCSV != nil && *e.ToolNamesCSV == "exec_command"
	}, "function_call exec_command")
	funcOut := findEntry(t, entries, func(e schema.SessionEntry) bool {
		return e.EntryType == ingest.EntryTypeToolResult &&
			e.ToolCallID != nil && *e.ToolCallID == "call_test_aaa"
	}, "function_call_output for call_test_aaa")
	if funcCall.ToolCallID == nil || *funcCall.ToolCallID != "call_test_aaa" {
		t.Errorf("function_call ToolCallID: got %v, want call_test_aaa", funcCall.ToolCallID)
	}
	if *funcCall.ToolCallID != *funcOut.ToolCallID {
		t.Errorf("function_call/function_call_output call_id mismatch: %q vs %q",
			*funcCall.ToolCallID, *funcOut.ToolCallID)
	}

	customCall := findEntry(t, entries, func(e schema.SessionEntry) bool {
		return e.EntryType == ingest.EntryTypeToolUse &&
			e.ToolNamesCSV != nil && *e.ToolNamesCSV == "apply_patch"
	}, "custom_tool_call apply_patch")
	customOut := findEntry(t, entries, func(e schema.SessionEntry) bool {
		return e.EntryType == ingest.EntryTypeToolResult &&
			e.ToolCallID != nil && *e.ToolCallID == "call_test_bbb"
	}, "custom_tool_call_output for call_test_bbb")
	if *customCall.ToolCallID != *customOut.ToolCallID {
		t.Errorf("custom_tool_call/custom_tool_call_output call_id mismatch: %q vs %q",
			*customCall.ToolCallID, *customOut.ToolCallID)
	}
}

// TestCodexIndexer_ToolKindClassification verifies that the shared
// classifyToolKind switch now recognises Codex's tool names (exec_command →
// execute, apply_patch → edit). A miss means the tool kind would silently fall
// to "other" and break per-kind analytics for Codex sessions.
func TestCodexIndexer_ToolKindClassification(t *testing.T) {
	entries := indexCodexFixture(t, defaultCodexFixtureOpts())

	execCall := findEntry(t, entries, func(e schema.SessionEntry) bool {
		return e.ToolNamesCSV != nil && *e.ToolNamesCSV == "exec_command"
	}, "exec_command tool_use")
	if execCall.ToolKind == nil || *execCall.ToolKind != schema.ToolCallKindExecute {
		t.Errorf("exec_command ToolKind: got %v, want %q", execCall.ToolKind, schema.ToolCallKindExecute)
	}

	patchCall := findEntry(t, entries, func(e schema.SessionEntry) bool {
		return e.ToolNamesCSV != nil && *e.ToolNamesCSV == "apply_patch"
	}, "apply_patch tool_use")
	if patchCall.ToolKind == nil || *patchCall.ToolKind != schema.ToolCallKindEdit {
		t.Errorf("apply_patch ToolKind: got %v, want %q", patchCall.ToolKind, schema.ToolCallKindEdit)
	}
}

// TestCodexIndexer_ToolInputPreserved verifies that function_call.arguments
// (a JSON string) and custom_tool_call.input (a raw string) both survive as
// ToolInput. The session_detail UI surfaces this so users can see what each
// tool was invoked with.
func TestCodexIndexer_ToolInputPreserved(t *testing.T) {
	entries := indexCodexFixture(t, defaultCodexFixtureOpts())

	funcCall := findEntry(t, entries, func(e schema.SessionEntry) bool {
		return e.ToolNamesCSV != nil && *e.ToolNamesCSV == "exec_command"
	}, "exec_command tool_use")
	if funcCall.ToolInput == nil || !strings.Contains(*funcCall.ToolInput, `"cmd"`) {
		t.Errorf(`function_call ToolInput: got %v, want JSON containing "cmd"`, funcCall.ToolInput)
	}

	customCall := findEntry(t, entries, func(e schema.SessionEntry) bool {
		return e.ToolNamesCSV != nil && *e.ToolNamesCSV == "apply_patch"
	}, "apply_patch tool_use")
	if customCall.ToolInput == nil || !strings.Contains(*customCall.ToolInput, "b/file.txt") {
		t.Errorf("custom_tool_call ToolInput: got %v, want raw patch text", customCall.ToolInput)
	}
}

// TestCodexIndexer_ReasoningHidden verifies that response_item.reasoning
// produces a thinking entry with no content body (Codex's encrypted_content is
// opaque) and HasThinking=true, matching how Claude treats its hidden thinking
// blocks.
func TestCodexIndexer_ReasoningHidden(t *testing.T) {
	entries := indexCodexFixture(t, defaultCodexFixtureOpts())

	reasoning := findEntry(t, entries, func(e schema.SessionEntry) bool {
		return e.EntryType == ingest.EntryTypeThinking
	}, "reasoning entry")
	if !reasoning.HasThinking {
		t.Error("reasoning entry: HasThinking should be true")
	}
	if reasoning.ContentPreview != nil {
		t.Errorf("reasoning entry: ContentPreview must be nil (encrypted_content is opaque), got %q", *reasoning.ContentPreview)
	}
	if reasoning.Role != ingest.RoleAssistant {
		t.Errorf("reasoning entry: Role should be assistant, got %q", reasoning.Role)
	}
}

// TestCodexIndexer_TextContentExtracted verifies that user and assistant
// message text survives into ContentPreview (truncated to ContentPreviewLimit
// by default). Without this, the dashboard's preview column and the
// session_detail turn body would be empty for Codex sessions.
func TestCodexIndexer_TextContentExtracted(t *testing.T) {
	entries := indexCodexFixture(t, defaultCodexFixtureOpts())

	userMsg := findEntry(t, entries, func(e schema.SessionEntry) bool {
		return e.EntryType == ingest.EntryTypeText && e.Role == ingest.RoleUser
	}, "user message")
	if userMsg.ContentPreview == nil || *userMsg.ContentPreview != "first prompt" {
		t.Errorf("user message preview: got %v, want %q", userMsg.ContentPreview, "first prompt")
	}

	assistantMsg := findEntry(t, entries, func(e schema.SessionEntry) bool {
		return e.EntryType == ingest.EntryTypeText && e.Role == ingest.RoleAssistant
	}, "assistant message")
	if assistantMsg.ContentPreview == nil || *assistantMsg.ContentPreview != "first response" {
		t.Errorf("assistant message preview: got %v, want %q", assistantMsg.ContentPreview, "first response")
	}
}

// TestCodexIndexer_EmptyRollout verifies an empty rollout doesn't crash and
// returns nil entries (not an error). The pipeline treats indexing as
// best-effort, so an empty rollout should produce zero entries without
// blocking the rest of the EXTRACT+WRITE → DB INSERT chain.
func TestCodexIndexer_EmptyRollout(t *testing.T) {
	mfs := testutil.NewMemFS()
	const path = "/home/test/.codex/sessions/2026/05/29/rollout-2026-05-29T21-14-24-" + testutil.TestSessionUUID + ".jsonl"
	if err := mfs.WriteFile(path, []byte(""), 0644); err != nil {
		t.Fatalf("seed empty: %v", err)
	}
	sid, _ := ingest.NewSessionID(testutil.TestSessionUUID)
	ds := ingest.DiscoveredSession{
		SessionID:    sid,
		Harness:      ingest.HarnessCodex,
		SourcePath:   ingest.ResolvedPath(path),
		SourceFormat: ingest.SourceFormatJSONL,
	}
	idx := ingest.NewCodexIndexer(mfs)
	entries, err := idx.IndexTranscript(context.Background(), ds)
	if err != nil {
		t.Fatalf("IndexTranscript on empty: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("empty rollout: expected 0 entries, got %d", len(entries))
	}
}

// TestCodexIndexer_MalformedLineSkipped verifies that a single bad line does
// not abort indexing — the remaining valid lines still produce entries. This
// matches Claude's behavior and protects against partial corruption.
func TestCodexIndexer_MalformedLineSkipped(t *testing.T) {
	mfs := testutil.NewMemFS()
	opts := defaultCodexFixtureOpts()
	good := codexRolloutJSONL(opts)
	// Inject a non-JSON line in the middle.
	corrupted := []byte(string(good) + "this is not json\n")
	const path = "/home/test/.codex/sessions/2026/05/29/rollout-2026-05-29T21-14-24-" + testutil.TestSessionUUID + ".jsonl"
	if err := mfs.WriteFile(path, corrupted, 0644); err != nil {
		t.Fatalf("seed corrupted: %v", err)
	}
	sid, _ := ingest.NewSessionID(opts.sessionID)
	ds := ingest.DiscoveredSession{
		SessionID:    sid,
		Harness:      ingest.HarnessCodex,
		SourcePath:   ingest.ResolvedPath(path),
		SourceFormat: ingest.SourceFormatJSONL,
	}
	idx := ingest.NewCodexIndexer(mfs)
	entries, err := idx.IndexTranscript(context.Background(), ds)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 7 {
		t.Errorf("malformed line should be skipped: got %d entries, want 7 from valid lines", len(entries))
	}
}

// TestCodexIndexer_ExecToolClassifiedAsExecute guards Codex's actual shell-tool
// name, `exec`. A fixture that exercises only `exec_command` misses this path
// and causes shell calls to appear as "other", making the tool filter useless.
func TestCodexIndexer_ExecToolClassifiedAsExecute(t *testing.T) {
	entries := indexCodexMultiAgentFixture(t, defaultCodexFixtureOpts())

	execCall := findEntry(t, entries, func(e schema.SessionEntry) bool {
		return e.ToolNamesCSV != nil && *e.ToolNamesCSV == "exec"
	}, "exec tool_use")
	if execCall.ToolKind == nil || *execCall.ToolKind != schema.ToolCallKindExecute {
		t.Errorf("exec ToolKind: got %v, want %q", execCall.ToolKind, schema.ToolCallKindExecute)
	}
}

// TestCodexIndexer_MultiAgentCoordinationToolsStayOther documents (does not
// silently decide) the current classification of ALL EIGHT documented
// Codex multi-agent coordination tools (codexMultiAgentCoordinationToolNames:
// wait_agent, send_message, followup_task, list_agents, wait, spawn_agent,
// interrupt_agent, request_user_input): every one falls to ToolCallKindOther
// today, same as before this fix. Whether they deserve a dedicated wire
// category requires a new schema.ToolCallKind
// enum value + the cross-repo contract ceremony) — this test asserts the
// FULL set, not a single representative, so a PARTIAL migration (some names
// re-categorized, others left behind) fails here just as loudly as leaving
// all eight unmapped would pass today; a single-name guard would miss that.
func TestCodexIndexer_MultiAgentCoordinationToolsStayOther(t *testing.T) {
	entries := indexCodexMultiAgentFixture(t, defaultCodexFixtureOpts())

	for _, name := range codexMultiAgentCoordinationToolNames {
		call := findEntry(t, entries, func(e schema.SessionEntry) bool {
			return e.ToolNamesCSV != nil && *e.ToolNamesCSV == name
		}, name+" tool_use")
		if call.ToolKind == nil || *call.ToolKind != schema.ToolCallKindOther {
			t.Errorf("%s ToolKind: got %v, want %q (pinned pending the category design decision)", name, call.ToolKind, schema.ToolCallKindOther)
		}
	}
}

// TestCodexIndexer_DeveloperRoleBootstrapMessagesAreSystemNotUser guards
// peasant's attribution layer when the user-turn rail encounters leading
// bootstrap messages. Peasant's indexer tags Codex's `developer`-role
// bootstrap messages (permissions instructions, persona, multi_agent_mode) as
// RoleSystem/EntryTypeSystem, never RoleUser. The
// actual miscount lives one layer up, in the fairtrade adapter's task-
// boundary/user-turn derivation (src/ui/transcript/analytics.js computeTasks),
// which does not distinguish "role==='user'" from session-bootstrap content a
// human never typed. That is a fairtrade-repo fix, out of scope
// here. This test exists so peasant's attribution layer — which IS correct
// today — cannot silently regress into misclassifying a developer message as
// RoleUser and making the fairtrade-side bug even worse.
func TestCodexIndexer_DeveloperRoleBootstrapMessagesAreSystemNotUser(t *testing.T) {
	entries := indexCodexMultiAgentFixture(t, defaultCodexFixtureOpts())

	var developerEntries int
	for _, e := range entries {
		if e.Depth != 0 || e.EntryType != schema.EntryTypeSystem {
			continue
		}
		if e.Role != schema.RoleSystem {
			t.Errorf("entry %d: developer-role bootstrap message got Role %q, want %q (must never leak into RoleUser)", e.EntryIndex, e.Role, schema.RoleSystem)
		}
		developerEntries++
	}
	if developerEntries != 3 {
		t.Fatalf("developer-role bootstrap messages: got %d system entries, want 3 (permissions, persona, multi_agent_mode)", developerEntries)
	}

	// The real first human message ("Targeted") must be RoleUser — distinct
	// from the injected AGENTS.md/skill-file "user"-role bootstrap content
	// that precedes it (peasant does not currently distinguish those from a
	// real human ask; that is the same fairtrade-side boundary question, not
	// asserted here).
	humanMsg := findEntry(t, entries, func(e schema.SessionEntry) bool {
		return e.ContentPreview != nil && *e.ContentPreview == "Targeted"
	}, "the real human message")
	if humanMsg.Role != schema.RoleUser {
		t.Errorf("real human message Role: got %q, want %q", humanMsg.Role, schema.RoleUser)
	}
}
