package harnesslayout_test

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/harnesslayout"
)

const (
	clineGlobalStorageRoot = "testdata/cline/vscode-globalstorage"
	clineDataRoot          = "testdata/cline/cline-data"
	// Every fixture string that stands for transcript content carries this marker.
	clineTranscriptMarker = "SYNTHETIC-"
)

func runCline(t *testing.T, roots ...string) harnesslayout.Report {
	t.Helper()
	layout, ok := harnesslayout.Lookup(harnesslayout.ToolCline)
	if !ok {
		t.Fatal("cline layout is not registered")
	}
	report, err := harnesslayout.Run(context.Background(), layout, roots, harnesslayout.OSSource, 0)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func clineCapture(t *testing.T, report harnesslayout.Report, session string) harnesslayout.Capture {
	t.Helper()
	for _, capture := range report.Captures {
		if capture.Session == session {
			return capture
		}
	}
	t.Fatalf("no capture for %s in %+v", session, report.Captures)
	return harnesslayout.Capture{}
}

func clineShape(t *testing.T, capture harnesslayout.Capture, artifact string) harnesslayout.ArtifactShape {
	t.Helper()
	for _, shape := range capture.Shapes {
		if shape.Artifact == artifact {
			return shape
		}
	}
	t.Fatalf("%s: no %s shape", capture.Session, artifact)
	return harnesslayout.ArtifactShape{}
}

func assertClineFields(t *testing.T, shape harnesslayout.ArtifactShape, paths ...string) {
	t.Helper()
	for _, want := range paths {
		if !slices.ContainsFunc(shape.Fields, func(f harnesslayout.FieldShape) bool { return f.Path == want }) {
			t.Errorf("%s: missing field path %s", shape.Artifact, want)
		}
	}
}

func clineTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestClineResolvesExtensionAndDataRoots(t *testing.T) {
	layout, _ := harnesslayout.Lookup(harnesslayout.ToolCline)
	darwin := harnesslayout.Env{GOOS: harnesslayout.OSDarwin, Home: "/Users/u", ConfigDir: "/Users/u/Library/Application Support"}
	roots := darwin.Resolve(layout)
	for _, want := range []string{
		"/Users/u/.cline/data",
		"/Users/u/Library/Application Support/Code/User/globalStorage/saoudrizwan.claude-dev",
		"/Users/u/Library/Application Support/Cursor/User/globalStorage/saoudrizwan.claude-dev",
	} {
		if !slices.Contains(roots, want) {
			t.Errorf("darwin roots %v miss %s", roots, want)
		}
	}
	if slices.Contains(roots, "/Users/u/.vscode-server/data/User/globalStorage/saoudrizwan.claude-dev") {
		t.Errorf("darwin roots %v include the Linux-only VS Code Server root", roots)
	}
	linux := harnesslayout.Env{GOOS: harnesslayout.OSLinux, Home: "/h", ConfigDir: "/h/.config"}
	if roots := linux.Resolve(layout); !slices.Contains(roots, "/h/.vscode-server/data/User/globalStorage/saoudrizwan.claude-dev") {
		t.Errorf("linux roots %v miss the VS Code Server root", roots)
	}
}

func TestClineCapturesLegacyExtensionTasks(t *testing.T) {
	report := runCline(t, clineGlobalStorageRoot)
	if len(report.Roots) != 1 || !report.Roots[0].Present || report.Roots[0].Sessions != 4 {
		t.Fatalf("roots = %+v", report.Roots)
	}
	if len(report.Captures) != 3 || len(report.Failures) != 1 || report.Failures[0].Session != "tasks/1767484800000" {
		t.Fatalf("captures = %d, failures = %+v", len(report.Captures), report.Failures)
	}

	native := clineCapture(t, report, "tasks/1767225600000")
	want := harnesslayout.Metadata{
		SessionID:     "1767225600000",
		Title:         "Add a health check endpoint to the sample service",
		ProjectPath:   "/projects/sample-app",
		StartedAt:     clineTime(t, "2026-01-01T00:00:00Z"),
		UpdatedAt:     clineTime(t, "2026-01-01T00:01:50Z"),
		Models:        []string{"claude-sonnet-4-5", "gpt-5"},
		UserTurns:     2,
		AssistantMsgs: 3,
		ToolCalls:     2,
	}
	if !reflect.DeepEqual(native.Metadata, want) {
		t.Fatalf("native task metadata = %+v, want %+v", native.Metadata, want)
	}
	api := clineShape(t, native, "api-conversation-history")
	if api.Records != 6 || api.Kinds["assistant:text+thinking+tool_use"] != 1 || api.Kinds["user:tool_result"] != 1 || api.Kinds["user:text"] != 2 {
		t.Fatalf("api history shape = %+v", api)
	}
	assertClineFields(t, api, "$[].role", "$[].content[].type", "$[].content[].input", "$[].ts", "$[].modelInfo.modelId", "$[].metrics.tokens.prompt", "$[].metrics.cost")
	ui := clineShape(t, native, "ui-messages")
	if ui.Records != 11 || ui.Kinds["say:task"] != 1 || ui.Kinds["say:user_feedback"] != 1 || ui.Kinds["say:api_req_started"] != 2 || ui.Kinds["ask:completion_result"] != 1 {
		t.Fatalf("ui messages shape = %+v", ui)
	}
	assertClineFields(t, ui, "$[].ts", "$[].type", "$[].say", "$[].ask", "$[].conversationHistoryIndex", "$[].lastCheckpointHash")
	assertClineFields(t, clineShape(t, native, "task-metadata"), "$.model_usage[].model_id", "$.environment_history[].cline_version", "$.files_in_context[].record_source")
	history := clineShape(t, native, "task-history")
	if history.Records != 1 || history.Kinds["history_item"] != 1 {
		t.Fatalf("task history shape = %+v", history)
	}
	assertClineFields(t, history, "$[].task", "$[].cwdOnTaskInitialization", "$[].tokensIn", "$[].totalCost", "$[].modelId")
	clineShape(t, native, "context-history")
	clineShape(t, native, "task-settings")

	xmlTools := clineCapture(t, report, "tasks/1767312000000").Metadata
	if xmlTools.Title != "Explain the build script" || xmlTools.ProjectPath != "/projects/tooling" || xmlTools.UserTurns != 1 || xmlTools.AssistantMsgs != 3 || xmlTools.ToolCalls != 2 || len(xmlTools.Models) != 0 {
		t.Fatalf("XML tool-call task metadata = %+v", xmlTools)
	}

	damaged := clineCapture(t, report, "tasks/1767398400000")
	if damaged.Metadata.Title != "Fix the flaky login test" || damaged.Metadata.UserTurns != 1 || damaged.Metadata.ProjectPath != "" {
		t.Fatalf("damaged task metadata = %+v", damaged.Metadata)
	}
	if shape := clineShape(t, damaged, "api-conversation-history"); shape.Malformed != 1 || shape.Records != 0 {
		t.Fatalf("malformed api history shape = %+v", shape)
	}
	if shape := clineShape(t, damaged, "task-history"); shape.Records != 0 {
		t.Fatalf("task without history entry = %+v", shape)
	}
}

func TestClineCapturesSDKSessionsAndStandaloneTasks(t *testing.T) {
	report := runCline(t, clineDataRoot)
	if len(report.Captures) != 3 || len(report.Failures) != 0 || report.Roots[0].Sessions != 3 {
		t.Fatalf("report = %+v", report)
	}

	lead := clineCapture(t, report, "sessions/1767657600000_k3x9q")
	want := harnesslayout.Metadata{
		SessionID:     "1767657600000_k3x9q",
		Title:         "Rename the config loader",
		ProjectPath:   "/projects/sample-api",
		StartedAt:     clineTime(t, "2026-01-06T00:00:00Z"),
		UpdatedAt:     clineTime(t, "2026-01-06T00:05:00Z"),
		Models:        []string{"anthropic/claude-sonnet-4.5"},
		UserTurns:     1,
		AssistantMsgs: 3,
		ToolCalls:     2,
	}
	if !reflect.DeepEqual(lead.Metadata, want) {
		t.Fatalf("SDK session metadata = %+v, want %+v", lead.Metadata, want)
	}
	manifest := clineShape(t, lead, "session-manifest")
	if manifest.Records != 1 || manifest.Kinds["source:vscode"] != 1 {
		t.Fatalf("manifest shape = %+v", manifest)
	}
	assertClineFields(t, manifest, "$.session_id", "$.status", "$.provider", "$.model", "$.cwd", "$.prompt", "$.started_at", "$.ended_at", "$.metadata.tokensIn", "$.messages_path")
	messages := clineShape(t, lead, "session-messages")
	if messages.Records != 6 || messages.Kinds["user:tool_result"] != 2 || messages.Kinds["assistant:thinking+tool_use"] != 1 {
		t.Fatalf("messages shape = %+v", messages)
	}
	assertClineFields(t, messages, "$.version", "$.agent", "$.origin.source", "$.system_prompt", "$.messages[].content[].type", "$.messages[].modelInfo.id", "$.messages[].metrics.inputTokens", "$.messages[].ts")
	subagent := clineShape(t, lead, "subagent-messages")
	if subagent.Records != 2 || subagent.Kinds["user:string"] != 1 {
		t.Fatalf("subagent shape = %+v", subagent)
	}
	assertClineFields(t, subagent, "$.taskType", "$.origin.parentThreadId")
	assertClineFields(t, clineShape(t, lead, "session-compaction"), "$.source_message_count", "$.messages[].role")

	orphan := clineCapture(t, report, "sessions/1767744000000_p7m2d")
	if orphan.Metadata.Title != "" || orphan.Metadata.UserTurns != 1 || !slices.Equal(orphan.Metadata.Models, []string{"gpt-5"}) || len(orphan.Shapes) != 1 {
		t.Fatalf("session without manifest = %+v", orphan)
	}

	standalone := clineCapture(t, report, "tasks/1767571200000")
	if standalone.Metadata.Title != "List the open TODO comments" || len(standalone.Shapes) != 1 {
		t.Fatalf("standalone legacy task = %+v", standalone)
	}
}

func TestClineReportsNeverCarryTranscriptText(t *testing.T) {
	report := runCline(t, clineGlobalStorageRoot, clineDataRoot)
	full, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(full), clineTranscriptMarker) {
		t.Fatalf("report contains transcript text: %s", full)
	}
	shapeOnly, err := json.Marshal(report.ShapeOnly())
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"health check", "/projects/", "1767225600000", "1767484800000", "2026-01-0", "Rename the config loader"} {
		if strings.Contains(string(shapeOnly), private) {
			t.Fatalf("shape-only report contains %q", private)
		}
	}
	if !strings.Contains(string(shapeOnly), "claude-sonnet-4-5") || !strings.Contains(string(shapeOnly), "say:user_feedback") {
		t.Fatal("shape-only report dropped models or kinds")
	}
}
