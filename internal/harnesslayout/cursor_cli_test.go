package harnesslayout_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/harnesslayout"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

const (
	cliChatID       = "3f1c2b7a-1111-4a4a-9b9b-000000000001"
	cliPlainChatID  = "3f1c2b7a-2222-4a4a-9b9b-000000000002"
	cliBrokenChatID = "3f1c2b7a-3333-4a4a-9b9b-000000000003"
	cliACPID        = "3f1c2b7a-4444-4a4a-9b9b-000000000004"
	cliBucket       = "0123456789abcdef0123456789abcdef"
	cliSecretKey    = "c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00"
	cliSecretText   = "invented prompt that must not leak"
	cliPrivatePath  = "/invented/private/file.txt"
)

// writeCursorDB creates a SQLite database and runs each statement with its
// arguments.
func writeCursorDB(t *testing.T, path string, statements ...cursorStatement) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite|sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, statement := range statements {
		if err := sqlitex.Execute(conn, statement.query, &sqlitex.ExecOptions{Args: statement.args}); err != nil {
			t.Fatalf("%s: %v", statement.query, err)
		}
	}
}

type cursorStatement struct {
	query string
	args  []any
}

func sqlStmt(query string, args ...any) cursorStatement {
	return cursorStatement{query: query, args: args}
}

func writeCursorFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

var cursorCLISchema = []cursorStatement{
	sqlStmt(`CREATE TABLE blobs (id TEXT PRIMARY KEY, data BLOB)`),
	sqlStmt(`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT)`),
}

func cursorCLIFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	agent := `{"agentId":"` + cliChatID + `","latestRootBlobId":"ab12","name":"New Agent","mode":"default",` +
		`"isRunEverything":false,"createdAt":1767225600000,"lastUsedModel":"model-cli-a","blobEncryptionKey":"` + cliSecretKey + `"}`
	blobs := []string{
		`{"role":"system","content":"invented system prompt"}`,
		`{"role":"user","content":[{"type":"text","text":"<user_info>invented environment</user_info>"}]}`,
		`{"role":"user","content":[{"type":"text","text":"<user_query>` + cliSecretText + `</user_query>"}],"createdAt":1767225601000}`,
		`{"role":"assistant","content":[{"type":"text","text":"invented answer"},` +
			`{"type":"tool-call","toolCallId":"call_1","toolName":"Read","args":{"path":"` + cliPrivatePath + `","` + cliPrivatePath + `":1}}],` +
			`"createdAt":1767225602000,"providerOptions":{"cursor":{"modelName":"model-cli-b"}}}`,
		`{"role":"tool","content":[{"type":"tool-result","toolCallId":"call_1","toolName":"Read","result":"invented file body"}]}`,
	}
	statements := append([]cursorStatement{}, cursorCLISchema...)
	statements = append(statements, sqlStmt(`INSERT INTO meta (key, value) VALUES ('0', ?)`, hex.EncodeToString([]byte(agent))))
	for i, blob := range blobs {
		statements = append(statements, sqlStmt(`INSERT INTO blobs (id, data) VALUES (?, ?)`, "blob-"+string(rune('a'+i)), []byte(blob)))
	}
	statements = append(statements, sqlStmt(`INSERT INTO blobs (id, data) VALUES ('graph', ?)`, []byte{0x0a, 0x20, 0x01, 0x02, 0x03}))
	chat := filepath.Join(root, "chats", cliBucket, cliChatID)
	writeCursorDB(t, filepath.Join(chat, "store.db"), statements...)
	writeCursorFile(t, filepath.Join(chat, "meta.json"),
		`{"schemaVersion":1,"createdAtMs":1767225600000,"updatedAtMs":1767225700000,"hasConversation":true,"title":"Invented chat title","cwd":"/invented/workspace"}`)
	writeCursorFile(t, filepath.Join(chat, "prompt_history.json"), `["`+cliSecretText+`","second invented prompt"]`)

	plain := filepath.Join(root, "chats", cliBucket, cliPlainChatID)
	writeCursorDB(t, filepath.Join(plain, "store.db"), append(append([]cursorStatement{}, cursorCLISchema...),
		sqlStmt(`INSERT INTO meta (key, value) VALUES ('0', ?)`, `{"agentId":"`+cliPlainChatID+`","name":"Plain agent name","workspacePath":"/invented/plain"}`),
		sqlStmt(`INSERT INTO meta (key, value) VALUES ('1', 'not hex and not json')`))...)

	writeCursorFile(t, filepath.Join(root, "chats", cliBucket, cliBrokenChatID, "store.db"), "this is not a SQLite database")

	acp := filepath.Join(root, "acp-sessions", cliACPID)
	writeCursorDB(t, filepath.Join(acp, "store.db"), append(append([]cursorStatement{}, cursorCLISchema...),
		sqlStmt(`INSERT INTO blobs (id, data) VALUES ('m', ?)`, []byte(`{"role":"user","content":"invented acp prompt"}`)))...)
	writeCursorFile(t, filepath.Join(acp, "meta.json"), `{"schemaVersion":1,"cwd":"/invented/acp","title":"Invented ACP title"}`)
	return root
}

func runCursorLayout(t *testing.T, tool harnesslayout.Tool, root string) (harnesslayout.Report, string) {
	t.Helper()
	layout, ok := harnesslayout.Lookup(tool)
	if !ok {
		t.Fatalf("%s is not registered", tool)
	}
	report, err := harnesslayout.Run(context.Background(), layout, []string{root}, harnesslayout.OSSource, 0)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	return report, string(encoded)
}

func captureBySession(report harnesslayout.Report) map[string]harnesslayout.Capture {
	out := map[string]harnesslayout.Capture{}
	for _, capture := range report.Captures {
		out[capture.Session] = capture
	}
	return out
}

func fieldPaths(shape harnesslayout.ArtifactShape) map[string]harnesslayout.FieldShape {
	out := map[string]harnesslayout.FieldShape{}
	for _, field := range shape.Fields {
		out[field.Path] = field
	}
	return out
}

func TestCursorCLICapturesChatStoreShapeAndMetadata(t *testing.T) {
	t.Parallel()
	report, encoded := runCursorLayout(t, harnesslayout.ToolCursorCLI, cursorCLIFixture(t))

	if len(report.Roots) != 1 || !report.Roots[0].Present || report.Roots[0].Sessions != 4 {
		t.Fatalf("roots = %+v", report.Roots)
	}
	if len(report.Failures) != 1 || report.Failures[0].Session != cliBrokenChatID {
		t.Fatalf("failures = %+v, want only the corrupt store", report.Failures)
	}
	captures := captureBySession(report)

	chat := captures[cliChatID]
	meta := chat.Metadata
	if meta.SessionID != cliChatID || meta.Title != "Invented chat title" || meta.ProjectPath != "/invented/workspace" {
		t.Fatalf("identity = %+v", meta)
	}
	if meta.UserTurns != 1 || meta.AssistantMsgs != 1 || meta.ToolCalls != 1 {
		t.Fatalf("counts = %+v, want the injected preamble excluded", meta)
	}
	if strings.Join(meta.Models, ",") != "model-cli-a,model-cli-b" {
		t.Fatalf("models = %v", meta.Models)
	}
	if !meta.StartedAt.Equal(time.UnixMilli(1767225600000)) || !meta.UpdatedAt.Equal(time.UnixMilli(1767225700000)) {
		t.Fatalf("time range = %v..%v", meta.StartedAt, meta.UpdatedAt)
	}

	if len(chat.Shapes) != 3 {
		t.Fatalf("shapes = %d, want store, meta.json, and prompt history", len(chat.Shapes))
	}
	store := chat.Shapes[0]
	want := map[string]int{"meta": 1, "system": 1, "user": 2, "assistant": 1, "tool": 1, "binary": 1}
	for kind, count := range want {
		if store.Kinds[kind] != count {
			t.Fatalf("store kinds = %v, want %v", store.Kinds, want)
		}
	}
	if store.Artifact != "chat-store" || store.Format != harnesslayout.FormatSQLite || store.Records != 7 || store.Malformed != 0 {
		t.Fatalf("store = %+v", store)
	}
	fields := fieldPaths(store)
	for _, path := range []string{
		"$.meta.key", "$.meta.value", "$.blobs.id", "$.blobs.data",
		"$.meta.value.agentId", "$.meta.value.blobEncryptionKey", "$.meta.value.latestRootBlobId",
		"$.blobs.data.content[].type", "$.blobs.data.content[].args.path", "$.blobs.data.content[].args.*",
		"$.blobs.data.providerOptions.cursor.modelName",
	} {
		if _, ok := fields[path]; !ok {
			t.Errorf("store shape lacks %s", path)
		}
	}
	if prompts := chat.Shapes[2]; prompts.Artifact != "prompt-history" || prompts.Records != 2 {
		t.Fatalf("prompt history = %+v", prompts)
	}

	plain := captures[cliPlainChatID]
	if plain.Metadata.Title != "Plain agent name" || plain.Metadata.ProjectPath != "/invented/plain" || plain.Shapes[0].Malformed != 1 {
		t.Fatalf("plain-JSON agent record = %+v", plain)
	}
	acp := captures[cliACPID]
	if acp.Shapes[0].Artifact != "acp-store" || acp.Metadata.Title != "Invented ACP title" || acp.Metadata.UserTurns != 1 {
		t.Fatalf("ACP session = %+v", acp)
	}

	for _, private := range []string{cliSecretKey, cliSecretText, cliPrivatePath, "invented answer", "invented file body", "invented environment"} {
		if strings.Contains(encoded, private) {
			t.Fatalf("report contains %q", private)
		}
	}
	shapeOnly, err := json.Marshal(report.ShapeOnly())
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"Invented chat title", "/invented/workspace", cliChatID, cliBucket} {
		if strings.Contains(string(shapeOnly), private) {
			t.Fatalf("shape-only report contains %q", private)
		}
	}
}

func TestCursorCLIToleratesAMissingRootAndAnEmptyStore(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeCursorDB(t, filepath.Join(root, "chats", cliBucket, cliChatID, "store.db"), sqlStmt(`CREATE TABLE unrelated (x)`))
	report, _ := runCursorLayout(t, harnesslayout.ToolCursorCLI, root)
	if len(report.Captures) != 1 || report.Captures[0].Shapes[0].Records != 0 || len(report.Failures) != 0 {
		t.Fatalf("empty store report = %+v", report)
	}

	report, _ = runCursorLayout(t, harnesslayout.ToolCursorCLI, filepath.Join(root, "absent"))
	if report.Roots[0].Present || len(report.Captures) != 0 {
		t.Fatalf("missing root report = %+v", report)
	}
}
