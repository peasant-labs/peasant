package harnesslayout_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/harnesslayout"
)

const (
	ideHeaderComposer = "c1"
	idePrefixSibling  = "c10"
	ideInlineComposer = "c2"
	ideBrokenComposer = "c3"
	ideWorkspaceHash  = "00112233445566778899aabbccddeeff"
	ideHeaderHash     = "ffeeddccbbaa99887766554433221100"
	ideSecretToken    = "invented-access-token-value"
	ideSecretText     = "invented bubble text that must not leak"
	idePrivateURI     = "file:///invented/private/code.go"
)

func cursorIDEFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	composer := `{"_v":10,"composerId":"c1","name":"Invented composer","createdAt":1767225600000,"lastUpdatedAt":1767225900000,` +
		`"unifiedMode":"agent","modelConfig":{"modelName":"model-ide-a"},` +
		`"fullConversationHeadersOnly":[{"bubbleId":"b1","type":1},{"bubbleId":"b2","type":2},{"bubbleId":"b3","type":2}],` +
		`"codeBlockData":{"` + idePrivateURI + `":{"invented":true}}}`
	bubbles := map[string]string{
		"b1": `{"_v":3,"type":1,"bubbleId":"b1","text":"` + ideSecretText + `","createdAt":"2026-01-01T00:01:00Z"}`,
		"b2": `{"_v":3,"type":2,"bubbleId":"b2","text":"invented answer","modelInfo":{"modelName":"model-ide-b"},` +
			`"tokenCount":{"inputTokens":2,"outputTokens":5},"createdAt":"2026-01-01T00:02:00Z"}`,
		"b3": `{"_v":3,"type":2,"bubbleId":"b3","capabilityType":15,"toolFormerData":{"tool":40,"name":"read_file_v2",` +
			`"toolCallId":"toolu_1","params":"{\"targetFile\":\"/invented/x\"}","status":"completed","result":"invented tool output"}}`,
	}
	inline := `{"_v":2,"composerId":"c2","name":"Inline composer","createdAt":1767225600000,"modelConfig":{"modelName":"default"},` +
		`"conversation":[{"type":1,"text":"invented"},{"type":2,"text":"invented"},{"type":2,"text":"invented"}]}`
	header := `{"type":"head","composerId":"c1","name":"Header name","workspaceIdentifier":{"uri":{"fsPath":"/invented/header-project"}}}`

	statements := []cursorStatement{
		sqlStmt(`CREATE TABLE ItemTable (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`),
		sqlStmt(`CREATE TABLE cursorDiskKV (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`),
		sqlStmt(`CREATE TABLE composerHeaders (composerId TEXT PRIMARY KEY, workspaceId TEXT, createdAt INTEGER, lastUpdatedAt INTEGER, isArchived INTEGER, value TEXT)`),
		sqlStmt(`INSERT INTO ItemTable VALUES ('cursorAuth/accessToken', ?)`, ideSecretToken),
		sqlStmt(`INSERT INTO cursorDiskKV VALUES ('cursorAuth/refreshToken', ?)`, ideSecretToken),
		sqlStmt(`INSERT INTO cursorDiskKV VALUES ('composerData:c1', ?)`, composer),
		sqlStmt(`INSERT INTO cursorDiskKV VALUES ('composerData:c2', ?)`, inline),
		sqlStmt(`INSERT INTO cursorDiskKV VALUES ('composerData:c3', '{not json')`),
		sqlStmt(`INSERT INTO cursorDiskKV VALUES ('composerData:c10', '{"name":"Sibling"}')`),
		sqlStmt(`INSERT INTO cursorDiskKV VALUES ('bubbleId:c10:x', '{"type":1,"text":"sibling"}')`),
		sqlStmt(`INSERT INTO cursorDiskKV VALUES ('checkpointId:c1:k1', '{}')`),
		sqlStmt(`INSERT INTO cursorDiskKV VALUES ('checkpointId:c10:k1', '{}')`),
		sqlStmt(`INSERT INTO cursorDiskKV VALUES ('composerVirtualRowHeights:c1', '{}')`),
		sqlStmt(`INSERT INTO cursorDiskKV VALUES ('agentKv:blob:abc', ?)`, ideSecretText),
		sqlStmt(`INSERT INTO composerHeaders VALUES ('c1', ?, 1767225600000, 1767225900000, 0, ?)`, ideHeaderHash, header),
	}
	for id, bubble := range bubbles {
		statements = append(statements, sqlStmt(`INSERT INTO cursorDiskKV VALUES (?, ?)`, "bubbleId:c1:"+id, bubble))
	}
	user := filepath.Join(root, "Cursor", "User")
	writeCursorDB(t, filepath.Join(user, "globalStorage", "state.vscdb"), statements...)

	workspace := filepath.Join(user, "workspaceStorage", ideWorkspaceHash)
	writeCursorDB(t, filepath.Join(workspace, "state.vscdb"),
		sqlStmt(`CREATE TABLE ItemTable (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`),
		sqlStmt(`INSERT INTO ItemTable VALUES ('cursorAuth/accessToken', ?)`, ideSecretToken),
		sqlStmt(`INSERT INTO ItemTable VALUES ('composer.composerData', ?)`,
			`{"allComposers":[{"composerId":"c2","name":"Inline composer","createdAt":1767225600000}],"selectedComposerIds":["c2"]}`))
	writeCursorFile(t, filepath.Join(workspace, "workspace.json"), `{"folder":"file:///invented/inline-project"}`)
	writeCursorFile(t, filepath.Join(user, "workspaceStorage", ideHeaderHash, "workspace.json"), `{"folder":"file:///invented/unused"}`)
	return user
}

func TestCursorIDEStateCapturesComposersAndBubbles(t *testing.T) {
	t.Parallel()
	report, encoded := runCursorLayout(t, harnesslayout.ToolCursorIDEState, cursorIDEFixture(t))

	if len(report.Roots) != 1 || report.Roots[0].Sessions != 4 || len(report.Failures) != 0 {
		t.Fatalf("roots = %+v failures = %+v", report.Roots, report.Failures)
	}
	captures := captureBySession(report)

	c1 := captures[ideHeaderComposer]
	meta := c1.Metadata
	if meta.Title != "Invented composer" || meta.ProjectPath != "/invented/header-project" {
		t.Fatalf("identity = %+v", meta)
	}
	if meta.UserTurns != 1 || meta.AssistantMsgs != 2 || meta.ToolCalls != 1 {
		t.Fatalf("counts = %+v, want the c10 sibling excluded", meta)
	}
	if strings.Join(meta.Models, ",") != "model-ide-a,model-ide-b" {
		t.Fatalf("models = %v", meta.Models)
	}
	if !meta.StartedAt.Equal(time.UnixMilli(1767225600000)) || !meta.UpdatedAt.Equal(time.UnixMilli(1767225900000)) {
		t.Fatalf("time range = %v..%v", meta.StartedAt, meta.UpdatedAt)
	}
	global := c1.Shapes[0]
	want := map[string]int{"composerData": 1, "bubble:user": 1, "bubble:assistant": 2, "checkpointId": 1, "composerVirtualRowHeights": 1, "composerHeaders": 1}
	for kind, count := range want {
		if global.Kinds[kind] != count {
			t.Fatalf("global kinds = %v, want %v", global.Kinds, want)
		}
	}
	fields := fieldPaths(global)
	for _, path := range []string{
		"$.cursorDiskKV.key", "$.cursorDiskKV.value", "$.composerHeaders.workspaceId", "$.composerHeaders.createdAt",
		"$.cursorDiskKV.composerData.unifiedMode", "$.cursorDiskKV.composerData.fullConversationHeadersOnly[].bubbleId",
		"$.cursorDiskKV.composerData.codeBlockData.*.invented", "$.cursorDiskKV.bubbleId.toolFormerData.name",
		"$.cursorDiskKV.bubbleId.tokenCount.inputTokens", "$.composerHeaders.value.workspaceIdentifier.uri.fsPath",
	} {
		if _, ok := fields[path]; !ok {
			t.Errorf("global shape lacks %s", path)
		}
	}
	if created := fields["$.composerHeaders.createdAt"]; created.Types[0] != harnesslayout.JSONNumber {
		t.Fatalf("INTEGER column type = %v", created.Types)
	}
	if len(c1.Shapes) != 2 || c1.Shapes[1].Artifact != "workspace-folder" {
		t.Fatalf("c1 shapes = %+v, want the header workspace's folder", c1.Shapes)
	}

	c2 := captures[ideInlineComposer]
	if c2.Metadata.ProjectPath != "/invented/inline-project" || c2.Metadata.UserTurns != 1 || c2.Metadata.AssistantMsgs != 2 || len(c2.Metadata.Models) != 0 {
		t.Fatalf("inline composer = %+v", c2.Metadata)
	}
	if len(c2.Shapes) != 3 || c2.Shapes[1].Artifact != "workspace-state" || c2.Shapes[1].Kinds["composer.composerData"] != 1 {
		t.Fatalf("inline composer shapes = %+v", c2.Shapes)
	}
	if broken := captures[ideBrokenComposer]; broken.Shapes[0].Malformed != 1 || broken.Shapes[0].Records != 0 {
		t.Fatalf("malformed composer = %+v", broken.Shapes[0])
	}

	for _, private := range []string{ideSecretToken, "cursorAuth", ideSecretText, idePrivateURI, "invented tool output", "invented answer", "/invented/x"} {
		if strings.Contains(encoded, private) {
			t.Fatalf("report contains %q", private)
		}
	}
	shapeOnly, err := json.Marshal(report.ShapeOnly())
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"Invented composer", "/invented/header-project", "/invented/inline-project", ideWorkspaceHash} {
		if strings.Contains(string(shapeOnly), private) {
			t.Fatalf("shape-only report contains %q", private)
		}
	}
}

func TestCursorIDEStateToleratesMissingGlobalStore(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeCursorFile(t, filepath.Join(root, "workspaceStorage", ideWorkspaceHash, "workspace.json"), `{"folder":"file:///invented"}`)
	report, _ := runCursorLayout(t, harnesslayout.ToolCursorIDEState, root)
	if !report.Roots[0].Present || report.Roots[0].Sessions != 0 || report.Roots[0].Error != "" {
		t.Fatalf("report = %+v", report)
	}

	writeCursorDB(t, filepath.Join(root, "globalStorage", "state.vscdb"), sqlStmt(`CREATE TABLE ItemTable (key TEXT, value BLOB)`))
	report, _ = runCursorLayout(t, harnesslayout.ToolCursorIDEState, root)
	if report.Roots[0].Sessions != 0 || report.Roots[0].Error != "" {
		t.Fatalf("store without cursorDiskKV = %+v", report.Roots)
	}
}
