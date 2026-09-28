package harnesslayout_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"

	"github.com/peasant-labs/peasant/internal/harnesslayout"
)

const (
	devinACPSession     = "0b1d2c3e-0000-4000-8000-00000000a001"
	devinSecondSession  = "0b1d2c3e-0000-4000-8000-00000000a002"
	devinCascadeSession = "0b1d2c3e-0000-4000-8000-00000000c001"
	devinStatePath      = "User/globalStorage/state.vscdb"
)

var devinFixtureRoots = []string{
	filepath.Join("testdata", "devin-desktop", "config", "Devin"),
	filepath.Join("testdata", "devin-desktop", "home", ".codeium", "windsurf"),
}

func devinLayout(t *testing.T) harnesslayout.Layout {
	t.Helper()
	tool, err := harnesslayout.NewTool("devin-desktop")
	if err != nil {
		t.Fatal(err)
	}
	layout, ok := harnesslayout.Lookup(tool)
	if !ok || tool != harnesslayout.ToolDevinDesktop {
		t.Fatalf("devin-desktop layout not registered")
	}
	return layout
}

func writeDevinStateDB(t *testing.T, root, stateJSON string) {
	t.Helper()
	dbPath := filepath.Join(root, filepath.FromSlash(devinStatePath))
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	conn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := sqlitex.ExecuteScript(conn, `CREATE TABLE ItemTable (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB);`, nil); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		"codeium.windsurf": stateJSON,
		`secret://{"extensionId":"codeium.windsurf","key":"fixture"}`: "FIXTURE-SECRET-VALUE",
		"workbench.fixture.state":                                     `{"FIXTURE-WORKBENCH-KEY":1}`,
	} {
		if err := sqlitex.Execute(conn, `INSERT INTO ItemTable (key, value) VALUES (?, CAST(? AS BLOB))`, &sqlitex.ExecOptions{Args: []any{key, value}}); err != nil {
			t.Fatal(err)
		}
	}
}

func devinFields(shape harnesslayout.ArtifactShape) map[string]harnesslayout.FieldShape {
	fields := map[string]harnesslayout.FieldShape{}
	for _, field := range shape.Fields {
		fields[field.Path] = field
	}
	return fields
}

func TestDevinDesktopCapturesACPStreamsCascadeInventoryAndStateIndex(t *testing.T) {
	layout := devinLayout(t)
	stateRoot := t.TempDir()
	writeDevinStateDB(t, stateRoot, `{
		"windsurf.state.cachedActiveTrajectory:fixture-ws-a": "RklYVFVSRS1BQ1RJVkUtVFJBSkVDVE9SWQ==",
		"windsurf.state.cachedActiveTrajectory:fixture-ws-b": "RklYVFVSRS1BQ1RJVkUtVFJBSkVDVE9SWQ==",
		"windsurf.state.cachedTrajectorySummaries:fixture-ws-a": "RklYVFVSRS1TVU1NQVJZ",
		"windsurf.fixtureFlag": true,
		"windsurf.fixtureCount": 3
	}`)
	paths := append(append([]string{}, devinFixtureRoots...), stateRoot)

	report, err := harnesslayout.Run(context.Background(), layout, paths, harnesslayout.OSSource, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Failures) != 0 {
		t.Fatalf("failures = %+v", report.Failures)
	}
	for i, want := range []int{2, 1, 1} {
		if root := report.Roots[i]; !root.Present || root.Sessions != want || root.Error != "" {
			t.Fatalf("root %d = %+v, want %d sessions", i, root, want)
		}
	}
	captures := map[string]harnesslayout.Capture{}
	for _, capture := range report.Captures {
		captures[capture.Session] = capture
	}
	if len(captures) != 4 {
		t.Fatalf("captures = %+v", report.Captures)
	}

	acp := captures[devinACPSession]
	meta := acp.Metadata
	if acp.Tool != harnesslayout.ToolDevinDesktop || meta.SessionID != devinACPSession || meta.Title != "Fixture widget rename" {
		t.Fatalf("acp metadata = %+v", meta)
	}
	if meta.UserTurns != 2 || meta.AssistantMsgs != 2 || meta.ToolCalls != 1 {
		t.Fatalf("acp counts = %+v", meta)
	}
	if len(meta.Models) != 1 || meta.Models[0] != "fixture-model-1" {
		t.Fatalf("acp models = %v", meta.Models)
	}
	if !meta.StartedAt.Equal(time.Date(2026, 6, 10, 9, 0, 0, 0, time.UTC)) || !meta.UpdatedAt.Equal(time.Date(2026, 6, 10, 9, 5, 0, 0, time.UTC)) {
		t.Fatalf("acp time range = %v..%v", meta.StartedAt, meta.UpdatedAt)
	}
	shape := acp.Shapes[0]
	if shape.Format != harnesslayout.FormatJSONL || shape.Role != harnesslayout.RoleTranscript || shape.Records != 11 || shape.Malformed != 1 {
		t.Fatalf("acp shape = %+v", shape)
	}
	wantKinds := map[string]int{
		"available_commands_update": 1, "user_message_chunk": 2, "session_info_update": 1, "agent_thought_chunk": 1,
		"agent_message_chunk": 3, "tool_call": 1, "tool_call_update": 1, "usage_update": 1,
	}
	if len(shape.Kinds) != len(wantKinds) {
		t.Fatalf("acp kinds = %v", shape.Kinds)
	}
	for kind, n := range wantKinds {
		if shape.Kinds[kind] != n {
			t.Fatalf("acp kinds = %v, want %s=%d", shape.Kinds, kind, n)
		}
	}
	fields := devinFields(shape)
	for _, path := range []string{"$.providerId", "$.notification.content.text", "$.notification._meta.cognition.ai/inputTokens", "$.notification.content[].content.type"} {
		if _, ok := fields[path]; !ok {
			t.Fatalf("acp fields lack %s: %v", path, shape.Fields)
		}
	}
	if second := captures[devinSecondSession].Metadata; second.UserTurns != 1 || second.AssistantMsgs != 1 || second.Title != "" {
		t.Fatalf("second session metadata = %+v", second)
	}

	cascade := captures[devinCascadeSession]
	if cascade.Metadata.SessionID != devinCascadeSession || cascade.Metadata.UpdatedAt.IsZero() {
		t.Fatalf("cascade metadata = %+v", cascade.Metadata)
	}
	if shape := cascade.Shapes[0]; shape.Format != harnesslayout.FormatText || shape.Records != 1 || shape.Kinds["opaque-under-64KiB"] != 1 || len(shape.Fields) != 0 {
		t.Fatalf("cascade shape = %+v", shape)
	}

	state := captures[devinStatePath].Shapes[0]
	if state.Format != harnesslayout.FormatSQLite || state.Role != harnesslayout.RoleIndex || state.Records != 5 || state.Malformed != 0 {
		t.Fatalf("state shape = %+v", state)
	}
	if state.Kinds["windsurf.state.cachedActiveTrajectory:*"] != 2 || state.Kinds["windsurf.state.cachedTrajectorySummaries:*"] != 1 {
		t.Fatalf("state kinds = %v", state.Kinds)
	}
	stateFields := devinFields(state)
	for path, typ := range map[string]harnesslayout.JSONType{
		"$.ItemTable.key":   harnesslayout.JSONString,
		"$.ItemTable.value": harnesslayout.JSONString,
		"$.codeium.windsurf.windsurf.state.cachedActiveTrajectory:*": harnesslayout.JSONString,
		"$.codeium.windsurf.windsurf.fixtureFlag":                    harnesslayout.JSONBool,
		"$.codeium.windsurf.windsurf.fixtureCount":                   harnesslayout.JSONNumber,
	} {
		if field, ok := stateFields[path]; !ok || len(field.Types) != 1 || field.Types[0] != typ {
			t.Fatalf("state field %s = %+v, want %s", path, field, typ)
		}
	}

	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{
		"FIXTURE-USER-PROMPT", "FIXTURE-AGENT", "FIXTURE-TOOL-OUTPUT", "FIXTURE-OPAQUE-TRAJECTORY", "synthetic fixture command",
		"RklYVFVSRS", "fixture-ws-a", "FIXTURE-SECRET-VALUE", "secret://", "FIXTURE-WORKBENCH-KEY", "Read fixture file",
	} {
		if strings.Contains(string(encoded), content) {
			t.Fatalf("report contains %q: %s", content, encoded)
		}
	}
	shapeOnly, err := json.Marshal(report.ShapeOnly())
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"Fixture widget rename", devinACPSession, devinCascadeSession, "2026-06-10", "testdata"} {
		if strings.Contains(string(shapeOnly), private) {
			t.Fatalf("shape-only report contains %q", private)
		}
	}
	if !strings.Contains(string(shapeOnly), "fixture-model-1") {
		t.Fatalf("shape-only report dropped the model: %s", shapeOnly)
	}
}

func TestDevinDesktopToleratesMissingAndMalformedStores(t *testing.T) {
	layout := devinLayout(t)

	malformedJSON := t.TempDir()
	writeDevinStateDB(t, malformedJSON, `{"windsurf.state.cachedActiveTrajectory:fixture-ws-a": `)
	notSQLite := t.TempDir()
	if err := os.MkdirAll(filepath.Join(notSQLite, "User", "globalStorage"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(notSQLite, filepath.FromSlash(devinStatePath)), []byte("FIXTURE-NOT-A-DATABASE"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := t.TempDir()
	missing := filepath.Join(empty, "absent")

	report, err := harnesslayout.Run(context.Background(), layout, []string{malformedJSON, notSQLite, empty, missing}, harnesslayout.OSSource, 0)
	if err != nil {
		t.Fatal(err)
	}
	if report.Roots[2].Sessions != 0 || !report.Roots[2].Present || report.Roots[3].Present || report.Roots[3].Error != "" {
		t.Fatalf("roots = %+v", report.Roots)
	}
	if len(report.Captures) != 1 || report.Captures[0].Shapes[0].Malformed != 1 || report.Captures[0].Shapes[0].Records != 0 {
		t.Fatalf("captures = %+v", report.Captures)
	}
	if len(report.Failures) != 1 || report.Failures[0].Session != devinStatePath {
		t.Fatalf("failures = %+v", report.Failures)
	}

	inMemory := fstest.MapFS{devinStatePath: {Data: []byte("FIXTURE")}}
	open := func(string) harnesslayout.Source { return harnesslayout.Source{FS: inMemory} }
	report, err = harnesslayout.Run(context.Background(), layout, []string{"/memory"}, open, 0)
	if err != nil || len(report.Failures) != 1 || len(report.Captures) != 0 {
		t.Fatalf("in-memory state database: %+v, %v", report, err)
	}
}
