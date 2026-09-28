package harnesslayout_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/harnesslayout"
)

const kiroCrewFixture = "testdata/kirocrew"

func runKiroCrew(t *testing.T, roots ...string) harnesslayout.Report {
	t.Helper()
	layout, ok := harnesslayout.Lookup(harnesslayout.ToolKiroCrew)
	if !ok {
		t.Fatal("kirocrew layout is not registered")
	}
	report, err := harnesslayout.Run(context.Background(), layout, roots, harnesslayout.OSSource, 0)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func shapesByArtifact(capture harnesslayout.Capture) map[string][]harnesslayout.ArtifactShape {
	out := map[string][]harnesslayout.ArtifactShape{}
	for _, shape := range capture.Shapes {
		out[shape.Artifact] = append(out[shape.Artifact], shape)
	}
	return out
}

func hasField(shape harnesslayout.ArtifactShape, path string) bool {
	for _, field := range shape.Fields {
		if field.Path == path {
			return true
		}
	}
	return false
}

func TestKiroCrewLayoutResolvesDataHomeOnEveryOS(t *testing.T) {
	layout, _ := harnesslayout.Lookup(harnesslayout.ToolKiroCrew)
	for _, goos := range []harnesslayout.OS{harnesslayout.OSLinux, harnesslayout.OSDarwin, harnesslayout.OSWindows} {
		env := harnesslayout.Env{GOOS: goos, Home: "/h", ConfigDir: "/h/.config"}
		got := env.Resolve(layout)
		want := []string{filepath.FromSlash("/h/.kiro/crew"), filepath.FromSlash("/h/.kirocrew")}
		if !slices.Equal(got, want) {
			t.Errorf("%s roots = %v, want %v", goos, got, want)
		}
	}
}

func TestKiroCrewCaptureJoinsTranscriptsAndCrewLogs(t *testing.T) {
	report := runKiroCrew(t, kiroCrewFixture)
	if len(report.Roots) != 1 || !report.Roots[0].Present || report.Roots[0].Sessions != 3 || report.Roots[0].Error != "" {
		t.Fatalf("roots = %+v", report.Roots)
	}
	if len(report.Failures) != 0 {
		t.Fatalf("failures = %+v", report.Failures)
	}
	var ids []string
	for _, capture := range report.Captures {
		ids = append(ids, capture.Session)
	}
	if strings.Join(ids, ",") != "dashboard_fix-build,slack_1789000000.123,sub-7f3a" {
		t.Fatalf("sessions = %v", ids)
	}

	dashboard := report.Captures[0]
	meta := dashboard.Metadata
	if meta.SessionID != "dashboard_fix-build" || meta.Title != "Fix the flaky build" || meta.ProjectPath != "/work/example-app" {
		t.Fatalf("dashboard identity = %+v", meta)
	}
	if meta.UserTurns != 3 || meta.AssistantMsgs != 3 || meta.ToolCalls != 3 {
		t.Fatalf("dashboard counts = %+v, want transcript and archive rows and crew-log tool calls", meta)
	}
	if !slices.Equal(meta.Models, []string{"claude-sonnet-4.5", "claude-haiku-4.5"}) {
		t.Fatalf("dashboard models = %v, want the opening model before the fallback", meta.Models)
	}
	if !meta.StartedAt.Equal(time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)) || meta.UpdatedAt.UnixMilli() != 1789902020100 {
		t.Fatalf("dashboard time range = %v..%v", meta.StartedAt, meta.UpdatedAt)
	}

	shapes := shapesByArtifact(dashboard)
	for name, want := range map[string]int{"transcript": 1, "archive": 1, "threads": 1, "session-map": 1, "crew-log": 2} {
		if len(shapes[name]) != want {
			t.Fatalf("%s shapes = %d, want %d: %+v", name, len(shapes[name]), want, dashboard.Shapes)
		}
	}
	transcript := shapes["transcript"][0]
	if transcript.Records != 7 || transcript.Malformed != 1 || transcript.Kinds["metadata"] != 1 || transcript.Kinds["user"] != 2 ||
		transcript.Kinds["assistant"] != 2 || transcript.Kinds["tool"] != 1 || transcript.Kinds["system"] != 1 {
		t.Fatalf("transcript shape = %+v", transcript)
	}
	if !hasField(transcript, "$.meta.mid") || !hasField(transcript, "$.variants[].content") || !hasField(transcript, "$.turn_in_flight_prompt.content") {
		t.Fatalf("transcript fields = %+v", transcript.Fields)
	}
	if archive := shapes["archive"][0]; archive.Kinds["archive"] != 1 || archive.Records != 3 {
		t.Fatalf("archive shape = %+v", archive)
	}
	threads := shapes["threads"][0]
	if threads.Records != 2 || threads.Kinds["user"] != 1 || threads.Kinds["assistant"] != 1 || !hasField(threads, "$.threads.{mid}[].role") {
		t.Fatalf("threads shape = %+v", threads)
	}
	if sessionMap := shapes["session-map"][0]; sessionMap.Records != 1 || sessionMap.Kinds["acp"] != 1 || !hasField(sessionMap, "$.sid") {
		t.Fatalf("session-map shape = %+v", sessionMap)
	}
	crewKinds := map[string]int{}
	for _, shape := range shapes["crew-log"] {
		for kind, n := range shape.Kinds {
			crewKinds[kind] += n
		}
	}
	if crewKinds["session"] != 2 || crewKinds["tool/called"] != 3 || crewKinds["turn/completed"] != 2 || crewKinds["model/selected"] != 1 {
		t.Fatalf("crew-log kinds = %v", crewKinds)
	}

	slack := report.Captures[1].Metadata
	if slack.Title != "" || slack.ProjectPath != "/work/example-service" || slack.UserTurns != 2 || slack.AssistantMsgs != 2 || slack.ToolCalls != 3 {
		t.Fatalf("slack metadata = %+v, want map cwd and transcript tool names", slack)
	}
	if got := shapesByArtifact(report.Captures[1]); len(got["crew-log"]) != 0 || len(got["session-map"]) != 1 {
		t.Fatalf("slack shapes = %+v", report.Captures[1].Shapes)
	}

	subagent := report.Captures[2].Metadata
	if subagent.UserTurns != 1 || subagent.AssistantMsgs != 2 || subagent.ToolCalls != 1 || subagent.ProjectPath != "/work/example-app" ||
		!slices.Equal(subagent.Models, []string{"claude-haiku-4.5"}) {
		t.Fatalf("crew-log-only metadata = %+v", subagent)
	}
}

func TestKiroCrewReportNeverCarriesTranscriptText(t *testing.T) {
	report := runKiroCrew(t, kiroCrewFixture)
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"SENTINEL", "cache directory", "a1b2c3d4e5f60718293a4b5c6d7e8f90", "b2c3d4e5f60718293a4b5c6d7e8f90a1", "C0EXAMPLE"} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("report contains %q", private)
		}
	}
	shapeOnly, err := json.Marshal(report.ShapeOnly())
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"Fix the flaky build", "/work/example", "dashboard_fix-build", "sub-7f3a", kiroCrewFixture, "2026-09-2"} {
		if strings.Contains(string(shapeOnly), private) {
			t.Fatalf("shape-only report contains %q", private)
		}
	}
}

func TestKiroCrewToleratesMissingRootAndMalformedFiles(t *testing.T) {
	root := t.TempDir()
	write := func(name, data string) {
		t.Helper()
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("session_map.json", "{not json")
	write("sessions/dashboard_a.jsonl", "{\"_type\":\"metadata\"}\n{\"role\":\"user\",\"ts\":\"garbage\"}\n")
	write("sessions/.threads/dashboard_a.json", "[")
	write("crew-log/sessions/x/log.jsonl", "")

	report := runKiroCrew(t, filepath.Join(root, "absent"), root)
	if report.Roots[0].Present || report.Roots[0].Error != "" || !report.Roots[1].Present {
		t.Fatalf("roots = %+v", report.Roots)
	}
	if len(report.Failures) != 0 || len(report.Captures) != 1 {
		t.Fatalf("captures = %+v failures = %+v", report.Captures, report.Failures)
	}
	capture := report.Captures[0]
	if capture.Metadata.UserTurns != 1 || !capture.Metadata.StartedAt.IsZero() {
		t.Fatalf("metadata = %+v", capture.Metadata)
	}
	shapes := shapesByArtifact(capture)
	if len(shapes["session-map"]) != 1 || shapes["session-map"][0].Malformed != 1 || shapes["threads"][0].Malformed != 1 {
		t.Fatalf("shapes = %+v", capture.Shapes)
	}
}
