package harnesslayout_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/harnesslayout"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

var kiroTestSecrets = []string{
	"kiro-ide fixture prompt",
	"workspace-session fixture prompt",
	"legacy-chat fixture prompt",
	"execution fixture prompt",
	"kiro-cli fixture prompt",
	"sqlite fixture prompt",
	"fixture assistant answer",
	"fixture tool output",
}

func TestKiroCapturesUnifiedSessionDirectories(t *testing.T) {
	report := kiroTestRun(t, harnesslayout.ToolKiro, filepath.Join("testdata", "kiro", "sessions"))
	if len(report.Roots) != 1 || report.Roots[0].Sessions != 1 || len(report.Failures) != 0 {
		t.Fatalf("roots = %+v, failures = %+v", report.Roots, report.Failures)
	}
	capture := kiroTestCapture(t, report, "827f466c2f6ca420/sess_5b0e7f52-3c1d-4a8e-9f60-1d2c3b4a5e6f")
	kiroTestMetadata(t, capture.Metadata, harnesslayout.Metadata{
		SessionID:     "sess_5b0e7f52-3c1d-4a8e-9f60-1d2c3b4a5e6f",
		Title:         "Add a health check endpoint",
		ProjectPath:   "/work/demo-app",
		StartedAt:     time.Date(2026, 7, 14, 13, 39, 0, 0, time.UTC),
		UpdatedAt:     time.Date(2026, 7, 14, 13, 41, 30, 0, time.UTC),
		Models:        []string{"claude-sonnet-4.5"},
		UserTurns:     2,
		AssistantMsgs: 2,
		ToolCalls:     1,
	})

	messages := kiroTestShape(t, capture, "messages")
	if messages.Records != 11 || messages.Malformed != 1 {
		t.Fatalf("messages records=%d malformed=%d", messages.Records, messages.Malformed)
	}
	kiroTestKinds(t, messages, map[string]int{
		"session_start": 1, "user": 2, "turn_start": 1, "assistant:Reasoning": 1, "assistant:Say": 1,
		"tool_call": 1, "tool_result": 1, "session_metadata": 1, "usage_summary": 1, "turn_end": 1,
	})
	kiroTestFields(t, messages, "$.timestamp", "$.payload.type", "$.payload.toolCallId", "$.payload.promptTurnSummaries[].usedTools[]")
	session := kiroTestShape(t, capture, "session")
	kiroTestKinds(t, session, map[string]int{"in_progress": 1})
	kiroTestFields(t, session, "$.id", "$.modelId", "$.workspacePaths[]", "$.createdAt", "$.lastModifiedAt")
	kiroTestNoContent(t, report)
}

func TestKiroCapturesEveryGlobalStorageGeneration(t *testing.T) {
	report := kiroTestRun(t, harnesslayout.ToolKiro, filepath.Join("testdata", "kiro", "kiroagent"))
	if len(report.Roots) != 1 || report.Roots[0].Sessions != 5 {
		t.Fatalf("roots = %+v", report.Roots)
	}
	if len(report.Failures) != 1 || !strings.HasSuffix(report.Failures[0].Session, "exec-truncated.chat") {
		t.Fatalf("failures = %+v, want only the truncated .chat file", report.Failures)
	}
	for _, capture := range report.Captures {
		if strings.HasPrefix(capture.Session, "dev_data/") {
			t.Fatalf("dev_data was captured as a session: %s", capture.Session)
		}
	}

	workspace := kiroTestCapture(t, report, "workspace-sessions/L3dvcmsvZGVtby1hcHA_/7c1e2f30-4a5b-4c6d-8e7f-90a1b2c3d4e5.json")
	kiroTestMetadata(t, workspace.Metadata, harnesslayout.Metadata{
		SessionID:     "7c1e2f30-4a5b-4c6d-8e7f-90a1b2c3d4e5",
		Title:         "Explain the retry policy",
		ProjectPath:   "/work/demo-app",
		Models:        []string{"claude-sonnet-4.5"},
		UserTurns:     2,
		AssistantMsgs: 2,
	})
	history := kiroTestShape(t, workspace, "workspace-session")
	kiroTestKinds(t, history, map[string]int{"user": 2, "assistant": 2})
	kiroTestFields(t, history, "$.history[].message.role", "$.history[].message.content[].type", "$.history[].executionId")
	if index := kiroTestShape(t, workspace, "workspace-sessions-index"); index.Records != 1 {
		t.Fatalf("workspace-sessions-index records = %d", index.Records)
	}
	kiroTestFields(t, kiroTestShape(t, workspace, "migration-marker"), "$.migratedAt", "$.v2SessionId")

	chat := kiroTestCapture(t, report, "4b911e9f2016fd74f00661e38e16644c/exec-legacy-0001.chat")
	kiroTestMetadata(t, chat.Metadata, harnesslayout.Metadata{
		SessionID:     "exec-legacy-0001",
		StartedAt:     time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt:     time.Date(2026, 1, 1, 0, 0, 10, 0, time.UTC),
		Models:        []string{"claude-haiku-4.5"},
		UserTurns:     1,
		AssistantMsgs: 3,
		ToolCalls:     2,
	})
	kiroTestKinds(t, kiroTestShape(t, chat, "legacy-chat"), map[string]int{"human": 2, "bot": 3, "tool": 1})

	execution := kiroTestCapture(t, report, "4b911e9f2016fd74f00661e38e16644c/414d1636299d2b9e4ce7e17fb11f63e9/c3470c42f00272c6ec2a29cdeeb97fe5")
	kiroTestMetadata(t, execution.Metadata, harnesslayout.Metadata{
		SessionID:     "7c1e2f30-4a5b-4c6d-8e7f-90a1b2c3d4e5",
		StartedAt:     time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC),
		UpdatedAt:     time.Date(2026, 7, 14, 12, 1, 0, 0, time.UTC),
		UserTurns:     1,
		AssistantMsgs: 1,
		ToolCalls:     1,
	})
	actions := kiroTestShape(t, execution, "execution")
	kiroTestKinds(t, actions, map[string]int{"readFile": 1, "say": 1})
	kiroTestFields(t, actions, "$.chatSessionId", "$.input.data.userPrompt", "$.actions[].actionState", "$.usageSummary[].usage")

	index := kiroTestCapture(t, report, "4b911e9f2016fd74f00661e38e16644c/b9eea8458b711b31b5e6aa03f546621e")
	kiroTestKinds(t, kiroTestShape(t, index, "execution-index"), map[string]int{"chat-agent": 1})
	if !index.Metadata.StartedAt.Equal(time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("execution-index startedAt = %v", index.Metadata.StartedAt)
	}
	kiroTestNoContent(t, report)
}

func TestKiroCLICapturesJSONLSessionsAndConversationDatabase(t *testing.T) {
	dataDir := t.TempDir()
	database := filepath.Join(dataDir, "data.sqlite3")
	kiroTestWriteDatabase(t, database)
	before, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}

	report := kiroTestRun(t, harnesslayout.ToolKiroCLI, filepath.Join("testdata", "kiro", "sessions", "cli"), dataDir)
	if len(report.Roots) != 2 || report.Roots[0].Sessions != 1 || report.Roots[1].Sessions != 3 || len(report.Failures) != 0 {
		t.Fatalf("roots = %+v, failures = %+v", report.Roots, report.Failures)
	}

	jsonl := kiroTestCapture(t, report, "0d9c8b7a-1111-4222-8333-944455556666")
	kiroTestMetadata(t, jsonl.Metadata, harnesslayout.Metadata{
		SessionID:     "0d9c8b7a-1111-4222-8333-944455556666",
		Title:         "Rename the config loader",
		ProjectPath:   "/work/cli-project",
		StartedAt:     time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC),
		UpdatedAt:     time.Date(2026, 3, 2, 9, 5, 0, 0, time.UTC),
		Models:        []string{"claude-sonnet-4"},
		UserTurns:     2,
		AssistantMsgs: 3,
		ToolCalls:     1,
	})
	transcript := kiroTestShape(t, jsonl, "session-transcript")
	kiroTestKinds(t, transcript, map[string]int{"Prompt": 2, "AssistantMessage": 3, "ToolResults": 1, "Clear": 1})
	kiroTestFields(t, transcript, "$.data.content[].kind", "$.data.content[].data.toolUseId", "$.data.meta.timestamp")
	kiroTestFields(t, kiroTestShape(t, jsonl, "session-header"),
		"$.session_state.rts_model_state.model_info.model_id", "$.session_state.conversation_metadata.user_turn_metadatas[].metering_usage[].unit")

	current := kiroTestCapture(t, report, "data.sqlite3/conversations_v2/1")
	kiroTestMetadata(t, current.Metadata, harnesslayout.Metadata{
		SessionID:     "9a8b7c6d-0000-4000-8000-00000000000a",
		ProjectPath:   "/work/sqlite-project",
		StartedAt:     time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC),
		UpdatedAt:     time.Date(2026, 2, 1, 10, 10, 0, 0, time.UTC),
		Models:        []string{"claude-sonnet-4", "claude-sonnet-4-internal"},
		UserTurns:     2,
		AssistantMsgs: 3,
		ToolCalls:     2,
	})
	kiroTestFields(t, kiroTestShape(t, current, "conversation-row"),
		"$.conversations_v2.key", "$.conversations_v2.conversation_id", "$.conversations_v2.created_at", "$.conversations_v2.updated_at", "$.conversations_v2.value")
	value := kiroTestShape(t, current, "conversation-value")
	kiroTestKinds(t, value, map[string]int{
		"user:Prompt": 2, "user:ToolUseResults": 1, "assistant:ToolUse": 1, "assistant:Response": 2,
	})
	kiroTestFields(t, value, "$.history[].assistant.ToolUse.tool_uses[].orig_name", "$.history[].request_metadata.model_id", "$.model_info.model_id")

	legacy := kiroTestCapture(t, report, "data.sqlite3/conversations/1")
	if legacy.Metadata.SessionID != "5e4d3c2b-0000-4000-8000-00000000000b" || legacy.Metadata.ProjectPath != "/work/amazon-q-project" || legacy.Metadata.UserTurns != 1 {
		t.Fatalf("amazon q conversation metadata = %+v", legacy.Metadata)
	}

	broken := kiroTestCapture(t, report, "data.sqlite3/conversations_v2/2")
	if value := kiroTestShape(t, broken, "conversation-value"); value.Malformed != 1 || value.Records != 0 {
		t.Fatalf("malformed value shape = %+v", value)
	}

	after, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("capture modified the Kiro CLI database")
	}
	kiroTestNoContent(t, report)
}

func TestKiroCLISkipsDatabaseWithoutOperatingSystemDirectory(t *testing.T) {
	dataDir := t.TempDir()
	kiroTestWriteDatabase(t, filepath.Join(dataDir, "data.sqlite3"))
	layout, _ := harnesslayout.Lookup(harnesslayout.ToolKiroCLI)
	open := func(dir string) harnesslayout.Source { return harnesslayout.Source{FS: os.DirFS(dir)} }
	report, err := harnesslayout.Run(context.Background(), layout, []string{dataDir}, open, 0)
	if err != nil || len(report.Captures) != 0 || report.Roots[0].Error != "" {
		t.Fatalf("report = %+v, err = %v", report, err)
	}
}

func TestKiroLayoutsResolveDocumentedRoots(t *testing.T) {
	env := harnesslayout.Env{GOOS: harnesslayout.OSLinux, Home: "/h", ConfigDir: "/h/.config"}
	ide, _ := harnesslayout.Lookup(harnesslayout.ToolKiro)
	if got := env.Resolve(ide); !slices.Equal(got, []string{
		"/h/.kiro/sessions",
		"/h/.config/Kiro/User/globalStorage/kiro.kiroagent",
		"/h/.kiro-server/data/User/globalStorage/kiro.kiroagent",
	}) {
		t.Fatalf("kiro linux roots = %v", got)
	}
	darwin := harnesslayout.Env{GOOS: harnesslayout.OSDarwin, Home: "/Users/u", ConfigDir: "/Users/u/Library/Application Support"}
	cli, _ := harnesslayout.Lookup(harnesslayout.ToolKiroCLI)
	if got := darwin.Resolve(cli); !slices.Equal(got, []string{
		"/Users/u/.kiro/sessions/cli",
		"/Users/u/Library/Application Support/kiro-cli",
		"/Users/u/Library/Application Support/amazon-q",
	}) {
		t.Fatalf("kiro-cli darwin roots = %v", got)
	}
	windows := harnesslayout.Env{GOOS: harnesslayout.OSWindows, Home: "C:/Users/u", ConfigDir: "C:/Users/u/AppData/Roaming"}
	got := windows.Resolve(cli)
	for i, want := range []string{
		"C:/Users/u/.kiro/sessions/cli",
		"C:/Users/u/AppData/Local/kiro-cli",
		"C:/Users/u/AppData/Roaming/kiro-cli",
		"C:/Users/u/AppData/Local/amazon-q",
		"C:/Users/u/AppData/Roaming/amazon-q",
	} {
		if i >= len(got) || filepath.ToSlash(got[i]) != want {
			t.Fatalf("kiro-cli windows roots = %v, want Local before Roaming for each product", got)
		}
	}
}

func TestKiroCLIReadsTopLevelRecordTimestamp(t *testing.T) {
	root := t.TempDir()
	transcript := `{"version":"v1","kind":"Prompt","timestamp":"2026-03-04T05:06:07Z","data":{"content":[{"kind":"text","data":"kiro-cli fixture prompt"}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(root, "1a2b3c4d-1111-4222-8333-944455556666.jsonl"), []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	capture := kiroTestCapture(t, kiroTestRun(t, harnesslayout.ToolKiroCLI, root), "1a2b3c4d-1111-4222-8333-944455556666")
	want := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	if !capture.Metadata.StartedAt.Equal(want) || capture.Metadata.UserTurns != 1 {
		t.Fatalf("metadata = %+v, want the top-level timestamp and one user turn", capture.Metadata)
	}
}

func kiroTestRun(t *testing.T, tool harnesslayout.Tool, roots ...string) harnesslayout.Report {
	t.Helper()
	layout, ok := harnesslayout.Lookup(tool)
	if !ok {
		t.Fatalf("%s is not registered", tool)
	}
	report, err := harnesslayout.Run(context.Background(), layout, roots, harnesslayout.OSSource, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range report.Roots {
		if !root.Present || root.Error != "" {
			t.Fatalf("root %+v", root)
		}
	}
	return report
}

func kiroTestCapture(t *testing.T, report harnesslayout.Report, session string) harnesslayout.Capture {
	t.Helper()
	for _, capture := range report.Captures {
		if capture.Session == session {
			return capture
		}
	}
	sessions := make([]string, 0, len(report.Captures))
	for _, capture := range report.Captures {
		sessions = append(sessions, capture.Session)
	}
	t.Fatalf("no capture %q in %v", session, sessions)
	return harnesslayout.Capture{}
}

func kiroTestShape(t *testing.T, capture harnesslayout.Capture, artifact string) harnesslayout.ArtifactShape {
	t.Helper()
	for _, shape := range capture.Shapes {
		if shape.Artifact == artifact {
			return shape
		}
	}
	t.Fatalf("%s has no %q artifact", capture.Session, artifact)
	return harnesslayout.ArtifactShape{}
}

func kiroTestMetadata(t *testing.T, got, want harnesslayout.Metadata) {
	t.Helper()
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Fatalf("metadata\n got %s\nwant %s", gotJSON, wantJSON)
	}
}

func kiroTestKinds(t *testing.T, shape harnesslayout.ArtifactShape, want map[string]int) {
	t.Helper()
	if len(shape.Kinds) != len(want) {
		t.Fatalf("%s kinds = %v, want %v", shape.Artifact, shape.Kinds, want)
	}
	for kind, count := range want {
		if shape.Kinds[kind] != count {
			t.Fatalf("%s kinds = %v, want %v", shape.Artifact, shape.Kinds, want)
		}
	}
}

func kiroTestFields(t *testing.T, shape harnesslayout.ArtifactShape, paths ...string) {
	t.Helper()
	for _, want := range paths {
		if !slices.ContainsFunc(shape.Fields, func(field harnesslayout.FieldShape) bool { return field.Path == want }) {
			t.Fatalf("%s has no field %s", shape.Artifact, want)
		}
	}
}

func kiroTestNoContent(t *testing.T, report harnesslayout.Report) {
	t.Helper()
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range kiroTestSecrets {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatalf("report contains transcript text %q", secret)
		}
	}
}

// kiroTestWriteDatabase builds a data.sqlite3 with the Kiro CLI
// conversations_v2 table and the Amazon Q Developer CLI conversations table
// it succeeded.
func kiroTestWriteDatabase(t *testing.T, path string) {
	t.Helper()
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	current := `{
		"conversation_id": "9a8b7c6d-0000-4000-8000-00000000000a",
		"next_message": null,
		"history": [
			{
				"user": {
					"additional_context": "",
					"env_context": {"env_state": {"operating_system": "linux", "current_working_directory": "/work/sqlite-project", "environment_variables": []}},
					"content": {"Prompt": {"prompt": "sqlite fixture prompt that must never leak"}},
					"timestamp": "2026-02-01T10:01:00+00:00",
					"images": null
				},
				"assistant": {"ToolUse": {"message_id": "a1", "content": "fixture assistant answer", "tool_uses": [
					{"id": "t1", "name": "fs_read", "orig_name": "fs_read", "args": {"path": "a"}, "orig_args": {"path": "a"}},
					{"id": "t2", "name": "execute_bash", "orig_name": "execute_bash", "args": {}, "orig_args": {}}
				]}},
				"request_metadata": {"request_id": "r1", "message_id": "a1", "request_start_timestamp_ms": 1769940060000, "stream_end_timestamp_ms": 1769940065000, "model_id": "claude-sonnet-4-internal", "tool_use_ids_and_names": [["t1", "fs_read"]]}
			},
			{
				"user": {"additional_context": "", "env_context": {"env_state": null}, "content": {"ToolUseResults": {"tool_use_results": [{"tool_use_id": "t1", "content": [{"Text": "fixture tool output"}], "status": "Success"}]}}, "timestamp": null, "images": null},
				"assistant": {"Response": {"message_id": "a2", "content": "fixture follow-up"}},
				"request_metadata": null
			},
			{
				"user": {"additional_context": "", "env_context": {"env_state": null}, "content": {"Prompt": {"prompt": "second sqlite prompt"}}, "timestamp": "2026-02-01T10:05:00+00:00", "images": null},
				"assistant": {"Response": {"message_id": "a3", "content": "second answer"}}
			}
		],
		"valid_history_range": [0, 3],
		"transcript": ["> sqlite fixture prompt that must never leak"],
		"tools": {},
		"context_manager": null,
		"context_message_length": null,
		"latest_summary": null,
		"model_info": {"model_name": "claude-sonnet-4", "model_id": "claude-sonnet-4", "context_window_tokens": 200000},
		"file_line_tracker": {},
		"checkpoint_manager": null,
		"mcp_enabled": true
	}`
	legacy := `{"conversation_id": "5e4d3c2b-0000-4000-8000-00000000000b", "history": [{"user": {"content": {"Prompt": {"prompt": "sqlite fixture prompt from amazon q"}}, "timestamp": null}, "assistant": {"Response": {"message_id": null, "content": "ok"}}}], "model": "claude-3-7-sonnet"}`
	statements := []struct {
		query string
		args  []any
	}{
		{query: "CREATE TABLE conversations (key TEXT PRIMARY KEY, value TEXT)"},
		{query: "CREATE TABLE conversations_v2 (key TEXT NOT NULL, conversation_id TEXT NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, value TEXT NOT NULL, PRIMARY KEY (key, conversation_id))"},
		{query: "INSERT INTO conversations (key, value) VALUES (?, ?)", args: []any{"/work/amazon-q-project", legacy}},
		{query: "INSERT INTO conversations_v2 VALUES (?, ?, ?, ?, ?)", args: []any{"/work/sqlite-project", "9a8b7c6d-0000-4000-8000-00000000000a", 1769940000000, 1769940600000, current}},
		{query: "INSERT INTO conversations_v2 VALUES (?, ?, ?, ?, ?)", args: []any{"/work/sqlite-project", "9a8b7c6d-0000-4000-8000-00000000000c", 1769940000000, 1769940000000, `{"history": [`}},
	}
	for _, statement := range statements {
		if err := sqlitex.Execute(conn, statement.query, &sqlitex.ExecOptions{Args: statement.args}); err != nil {
			t.Fatal(err)
		}
	}
}
