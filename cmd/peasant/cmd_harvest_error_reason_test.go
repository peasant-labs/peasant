package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
)

// TestHarvestHumanOutputNamesWhyASessionFailed proves the human summary says
// why a session failed instead of only "ERROR <harness> <id>".
func TestHarvestHumanOutputNamesWhyASessionFailed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(t.TempDir(), "projects")
	sid := "dddddddd-0000-4000-8000-000000000001"
	rec := func(uuid string) string {
		return `{"type":"user","sessionId":"` + sid + `","uuid":"` + uuid + `","cwd":"/tmp/qa-reason","timestamp":"2026-10-01T10:00:00Z","message":{"role":"user","content":"hello"}}` + "\n"
	}
	body := rec("u1") + "{\"type\":\"user\",\"broken\n" + rec("u3")
	if err := os.MkdirAll(filepath.Join(src, "-tmp-qa-reason"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "-tmp-qa-reason", sid+".jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := fmt.Sprintf("version: 1\nsources:\n  claude-code:\n    enabled: true\n    paths:\n      - %s\n  opencode: {enabled: false}\n  cursor: {enabled: false}\n  codex: {enabled: false}\n  strike: {enabled: false}\n  pi: {enabled: false}\noutput:\n  basePath: %s\n", src, filepath.Join(dir, "out"))
	if err := os.WriteFile(configPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--config=" + configPath}, {"--config=" + configPath, "--verbose"}} {
		output, err := executeHarvestCmd(t, dir, args)
		if err == nil {
			t.Fatalf("harvest %v should fail the malformed session:\n%s", args, output)
		}
		if !strings.Contains(output, "ERROR") || !strings.Contains(output, "capture JSONL record 2: malformed complete JSON") {
			t.Errorf("harvest %v does not say why the session failed:\n%s", args, output)
		}
	}
}

// TestVerboseSummaryPutsEachReasonUnderItsRow proves a failure reason prints
// directly under its own row in --verbose, before the subagent lines, and a
// failed subagent gets its own reason line.
func TestVerboseSummaryPutsEachReasonUnderItsRow(t *testing.T) {
	t.Parallel()
	root := ingest.SessionID("eeeeeeee-0000-4000-8000-000000000001")
	child := ingest.SessionID("eeeeeeee-0000-4000-8000-000000000002")
	result := &ingest.PipelineResult{Sessions: []ingest.SessionResult{
		{SessionID: root, Harness: ingest.HarnessClaudeCode, Error: errors.New("root reason")},
		{SessionID: child, Harness: ingest.HarnessClaudeCode, ParentUUID: &root, Error: errors.New("child reason")},
	}}
	var b strings.Builder
	printSummary(&b, result, true, false, "", "", nil, 0)
	var lines []string
	for _, l := range strings.Split(b.String(), "\n") {
		if strings.Contains(l, string(root)) || strings.Contains(l, string(child)) || strings.Contains(l, "reason") {
			lines = append(lines, strings.TrimSpace(l))
		}
	}
	want := []string{string(root), "root reason", string(child), "child reason"}
	if len(lines) != len(want) {
		t.Fatalf("summary lines %q, want rows and reasons in order %q:\n%s", lines, want, b.String())
	}
	for i, w := range want {
		if !strings.Contains(lines[i], w) {
			t.Fatalf("line %d = %q, want %q; order %q", i, lines[i], w, lines)
		}
	}
}

// TestDefaultSummaryNamesAFailedSubagentsReason proves a failed subagent, which
// has no row of its own in the default summary, still says why it failed.
func TestDefaultSummaryNamesAFailedSubagentsReason(t *testing.T) {
	t.Parallel()
	root := ingest.SessionID("eeeeeeee-0000-4000-8000-000000000003")
	child := ingest.SessionID("eeeeeeee-0000-4000-8000-000000000004")
	result := &ingest.PipelineResult{Sessions: []ingest.SessionResult{
		{SessionID: root, Harness: ingest.HarnessClaudeCode, Status: ingest.DiffNew},
		{SessionID: child, Harness: ingest.HarnessClaudeCode, ParentUUID: &root, Error: errors.New("child reason")},
	}}
	var b strings.Builder
	printSummary(&b, result, false, false, "", "", nil, 0)
	if !strings.Contains(b.String(), "subagent "+string(child)+": child reason") {
		t.Fatalf("default summary does not say why the subagent failed:\n%s", b.String())
	}
}
