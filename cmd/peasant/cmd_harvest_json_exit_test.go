package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHarvestJSONExitsNonZeroWhenASessionFails proves --json reports a failed
// session through the exit code, the same as the human summary does.
func TestHarvestJSONExitsNonZeroWhenASessionFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(t.TempDir(), "projects")
	sid := "cccccccc-0000-4000-8000-000000000001"
	rec := func(uuid string) string {
		return `{"type":"user","sessionId":"` + sid + `","uuid":"` + uuid + `","cwd":"/tmp/qa-json","timestamp":"2026-10-01T10:00:00Z","message":{"role":"user","content":"hello"}}` + "\n"
	}
	// A malformed record in the middle of a new transcript fails the session.
	body := rec("u1") + "{\"type\":\"user\",\"broken\n" + rec("u3")
	if err := os.MkdirAll(filepath.Join(src, "-tmp-qa-json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "-tmp-qa-json", sid+".jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := fmt.Sprintf("version: 1\nsources:\n  claude-code:\n    enabled: true\n    paths:\n      - %s\n  opencode: {enabled: false}\n  cursor: {enabled: false}\n  codex: {enabled: false}\n  strike: {enabled: false}\n  pi: {enabled: false}\noutput:\n  basePath: %s\n", src, filepath.Join(dir, "out"))
	if err := os.WriteFile(configPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	human, humanErr := executeHarvestCmd(t, dir, []string{"--config=" + configPath})
	if humanErr == nil {
		t.Fatalf("precondition: human harvest should fail this session:\n%s", human)
	}
	output, err := executeHarvestCmd(t, dir, []string{"--config=" + configPath, "--json"})
	if !strings.Contains(output, `"Errors": 1`) {
		t.Fatalf("--json summary does not report the failed session:\n%s", output)
	}
	if err == nil {
		t.Fatalf("harvest --json returned success although its own summary says Errors: 1")
	}
}
