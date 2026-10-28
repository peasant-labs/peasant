package harnesslayout_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/harnesslayout"
)

const (
	grokSessionA = "019a0000-0000-7000-8000-00000000000a"
	grokSessionB = "019a0000-0000-7000-8000-00000000000b"
	grokSessionC = "019a0000-0000-7000-8000-00000000000c"
)

func runGrokBuildFixture(t *testing.T) harnesslayout.Report {
	t.Helper()
	layout, ok := harnesslayout.Lookup(harnesslayout.ToolGrokBuild)
	if !ok {
		t.Fatal("grok-build layout is not registered")
	}
	root := filepath.Join("testdata", "grok-build")
	report, err := harnesslayout.Run(context.Background(), layout, []string{root}, harnesslayout.OSSource, 0)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func grokCapture(t *testing.T, report harnesslayout.Report, session string) harnesslayout.Capture {
	t.Helper()
	for _, capture := range report.Captures {
		if capture.Session == session {
			return capture
		}
	}
	t.Fatalf("no capture for %s in %+v", session, report.Captures)
	return harnesslayout.Capture{}
}

func grokShape(t *testing.T, capture harnesslayout.Capture, artifact string) harnesslayout.ArtifactShape {
	t.Helper()
	for _, shape := range capture.Shapes {
		if shape.Artifact == artifact {
			return shape
		}
	}
	t.Fatalf("no %s shape in %s", artifact, capture.Session)
	return harnesslayout.ArtifactShape{}
}

func TestGrokBuildLayoutResolvesGrokHomeOnEveryOS(t *testing.T) {
	layout, _ := harnesslayout.Lookup(harnesslayout.ToolGrokBuild)
	for _, env := range []harnesslayout.Env{
		{GOOS: harnesslayout.OSLinux, Home: "/h", ConfigDir: "/h/.config"},
		{GOOS: harnesslayout.OSDarwin, Home: "/h", ConfigDir: "/h/Library/Application Support"},
		{GOOS: harnesslayout.OSWindows, Home: "/h", ConfigDir: "/h/AppData/Roaming"},
	} {
		got := env.Resolve(layout)
		if len(got) != 1 || filepath.ToSlash(got[0]) != "/h/.grok" {
			t.Errorf("%s roots = %v, want [/h/.grok]", env.GOOS, got)
		}
	}
}

func TestGrokBuildCapturesSessionsFromEveryWorkingDirectoryGroup(t *testing.T) {
	report := runGrokBuildFixture(t)
	if len(report.Roots) != 1 || !report.Roots[0].Present || report.Roots[0].Sessions != 3 {
		t.Fatalf("roots = %+v", report.Roots)
	}
	if len(report.Failures) != 0 {
		t.Fatalf("failures = %+v", report.Failures)
	}
	var sessions []string
	for _, capture := range report.Captures {
		sessions = append(sessions, capture.Session)
		if capture.Tool != harnesslayout.ToolGrokBuild {
			t.Errorf("%s tool = %q", capture.Session, capture.Tool)
		}
	}
	if want := []string{grokSessionA, grokSessionB, grokSessionC}; !slices.Equal(sessions, want) {
		t.Fatalf("sessions = %v, want %v", sessions, want)
	}
}

func TestGrokBuildMetadataPrefersSummaryAndChatHistory(t *testing.T) {
	a := grokCapture(t, runGrokBuildFixture(t), grokSessionA)
	meta := a.Metadata
	if meta.SessionID != grokSessionA || meta.Title != "Add a greeting endpoint" || meta.ProjectPath != "/work/demo-app" {
		t.Fatalf("identity = %+v", meta)
	}
	if !meta.StartedAt.Equal(time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)) || !meta.UpdatedAt.Equal(time.Date(2026, 5, 1, 10, 5, 0, 0, time.UTC)) {
		t.Fatalf("time range = %v..%v", meta.StartedAt, meta.UpdatedAt)
	}
	if !slices.Equal(meta.Models, []string{"grok-build-demo", "grok-build-demo-fast"}) {
		t.Fatalf("models = %v", meta.Models)
	}
	if meta.UserTurns != 2 || meta.AssistantMsgs != 3 || meta.ToolCalls != 2 {
		t.Fatalf("counts = user %d assistant %d tools %d, want 2 3 2", meta.UserTurns, meta.AssistantMsgs, meta.ToolCalls)
	}

	chat := grokShape(t, a, "chat-history")
	if chat.Records != 10 || chat.Malformed != 1 || chat.Kinds["user"] != 3 || chat.Kinds["assistant"] != 3 || chat.Kinds["backend_tool_call"] != 1 {
		t.Fatalf("chat-history shape = %+v", chat)
	}
	updates := grokShape(t, a, "updates")
	if updates.Records != 10 || updates.Kinds["agent_message_chunk"] != 3 || updates.Kinds["tool_call"] != 1 || updates.Kinds["subagent_spawned"] != 1 {
		t.Fatalf("updates shape = %+v", updates)
	}
	fields := map[string]harnesslayout.FieldShape{}
	for _, field := range updates.Fields {
		fields[field.Path] = field
	}
	for _, want := range []string{"$.timestamp", "$.method", "$.params.sessionId", "$.params.update.sessionUpdate", "$.params.update.content.text", "$.params._meta.eventId"} {
		if _, ok := fields[want]; !ok {
			t.Errorf("updates shape lacks %s", want)
		}
	}
	subagent := grokShape(t, a, "subagent-meta")
	if subagent.Kinds["completed"] != 1 {
		t.Fatalf("subagent-meta shape = %+v", subagent)
	}
	for _, artifact := range []string{"summary", "signals", "plan", "rewind-points"} {
		grokShape(t, a, artifact)
	}
}

func TestGrokBuildToleratesMalformedSummaryAndMissingChatHistory(t *testing.T) {
	report := runGrokBuildFixture(t)

	b := grokCapture(t, report, grokSessionB)
	if summary := grokShape(t, b, "summary"); summary.Malformed != 1 || summary.Records != 0 {
		t.Fatalf("malformed summary shape = %+v", summary)
	}
	if b.Metadata.ProjectPath != "/work/demo-app" || b.Metadata.UserTurns != 1 {
		t.Fatalf("session b metadata = %+v", b.Metadata)
	}

	c := grokCapture(t, report, grokSessionC)
	meta := c.Metadata
	if meta.ProjectPath != "/work/a-very-long-project-path" {
		t.Fatalf("project from .cwd marker = %q", meta.ProjectPath)
	}
	if meta.UserTurns != 1 || meta.AssistantMsgs != 1 || meta.ToolCalls != 1 {
		t.Fatalf("update-stream counts = %+v", meta)
	}
	if updates := grokShape(t, c, "updates"); updates.Records != 4 || updates.Malformed != 1 {
		t.Fatalf("updates with a partial final line = %+v", updates)
	}
	if !meta.UpdatedAt.Equal(time.Date(2026, 5, 1, 12, 2, 0, 0, time.UTC)) {
		t.Fatalf("updated at = %v", meta.UpdatedAt)
	}
}

func TestGrokBuildReportNeverContainsTranscriptText(t *testing.T) {
	report := runGrokBuildFixture(t)
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"SECRET-PROMPT-7f3a", "Now add a test.", "package main", "go http handler", "write handler", "reminder text"} {
		if strings.Contains(string(encoded), text) {
			t.Fatalf("report contains transcript text %q", text)
		}
	}
	shapeOnly, err := json.Marshal(report.ShapeOnly())
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{grokSessionA, "/work/demo-app", "Add a greeting endpoint", "2026-05-01"} {
		if strings.Contains(string(shapeOnly), private) {
			t.Fatalf("shape-only report contains %q", private)
		}
	}
	if !strings.Contains(string(shapeOnly), "grok-build-demo") || !strings.Contains(string(shapeOnly), "$.params.update.sessionUpdate") {
		t.Fatalf("shape-only report dropped models or shapes: %s", shapeOnly)
	}
}
