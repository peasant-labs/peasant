package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// TestHarvestReportsSessionsExcludedBySelection proves sessions the saved
// selection leaves out are reported as excluded, with a hint, instead of being
// counted "unchanged" on a data dir that has never stored them.
func TestHarvestReportsSessionsExcludedBySelection(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(t.TempDir(), "projects")
	for i := 1; i <= 2; i++ {
		sid := fmt.Sprintf("bbbbbbbb-0000-4000-8000-00000000000%d", i)
		line := `{"type":"user","sessionId":"` + sid + `","uuid":"u1","cwd":"/tmp/qa-sel","timestamp":"2026-10-01T10:00:00Z","message":{"role":"user","content":"hello"}}` + "\n"
		if err := os.MkdirAll(filepath.Join(src, "-tmp-qa-sel"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(src, "-tmp-qa-sel", sid+".jsonl"), []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The selection kickstart saves on a machine with no projects yet.
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := fmt.Sprintf("version: 1\nsources:\n  claude-code:\n    enabled: true\n    paths:\n      - %s\n  opencode: {enabled: false}\n  cursor: {enabled: false}\n  codex: {enabled: false}\n  strike: {enabled: false}\n  pi: {enabled: false}\noutput:\n  basePath: %s\nselection:\n  mode: selected\n", src, filepath.Join(dir, "out"))
	if err := os.WriteFile(configPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--config=" + configPath, "--dry-run"}, {"--config=" + configPath}} {
		output, err := executeHarvestCmd(t, dir, args)
		if err != nil {
			t.Fatalf("harvest %v: %v\n%s", args, err, output)
		}
		if !strings.Contains(output, "   - 0     unchanged") {
			t.Errorf("harvest %v counted unselected sessions as unchanged:\n%s", args, output)
		}
		if !strings.Contains(output, "   - 2     excluded by selection") || !strings.Contains(output, "peasant config") {
			t.Errorf("harvest %v did not report the 2 sessions excluded by selection with a hint:\n%s", args, output)
		}
	}
}

// TestHarvestExcludedBySelectionAcrossRulesAndReaders covers the three ways a
// selection leaves a Claude session out (not on the allowlist, an exact
// session exclusion, a subagent whose parent was not selected) and the three
// readers of the run's counts: the --json summary and per-session rows, and
// the stored ingest log, which keeps its existing meaning (excluded sessions
// stay inside sessions_unchanged so a run's counts still add up).
func TestHarvestExcludedBySelectionAcrossRulesAndReaders(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(t.TempDir(), "projects")
	proj := filepath.Join(src, "-tmp-qa-sel")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	const kept, unlisted, denied = "cccccccc-0000-4000-8000-000000000001", "cccccccc-0000-4000-8000-000000000002", "cccccccc-0000-4000-8000-000000000003"
	write := func(path, sid, extra string) {
		t.Helper()
		line := `{"type":"user","sessionId":"` + sid + `",` + extra + `"uuid":"u-` + filepath.Base(path) + `","cwd":"/tmp/qa-sel","timestamp":"2026-10-01T10:00:00Z","message":{"role":"user","content":"hello"}}` + "\n"
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, sid := range []string{kept, unlisted, denied} {
		write(filepath.Join(proj, sid+".jsonl"), sid, "")
	}
	write(filepath.Join(proj, unlisted, "subagents", "agent-a1b2c3d.jsonl"), unlisted, `"isSidechain":true,"agentId":"a1b2c3d",`)

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := fmt.Sprintf("version: 1\nsources:\n  claude-code:\n    enabled: true\n    paths:\n      - %s\n  opencode: {enabled: false}\n  cursor: {enabled: false}\n  codex: {enabled: false}\n  strike: {enabled: false}\n  pi: {enabled: false}\noutput:\n  basePath: %s\n"+
		"selection:\n  mode: selected\n  harnesses:\n    claude-code:\n      sessions: [%s, %s]\n      exclusions:\n        sessions: [%s]\n",
		src, filepath.Join(dir, "out"), kept, denied, denied)
	if err := os.WriteFile(configPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := executeHarvestCmd(t, dir, []string{"--config=" + configPath, "--json"})
	if err != nil {
		t.Fatalf("harvest --json: %v\n%s", err, output)
	}
	var got struct {
		Summary  map[string]any `json:"summary"`
		Sessions []struct {
			SessionID           string `json:"sessionId"`
			Status              string `json:"status"`
			ExcludedBySelection *bool  `json:"excludedBySelection"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(output[strings.Index(output, "{"):]), &got); err != nil {
		t.Fatalf("decode --json output: %v\n%s", err, output)
	}
	if v, ok := got.Summary["ExcludedBySelection"]; !ok || v != float64(3) {
		t.Errorf("summary ExcludedBySelection = %v (present %v), want 3 (unlisted root, its subagent, exact exclusion)\n%s", v, ok, output)
	}
	// The kept session was just written, so it is debounced as Active.
	if got.Summary["Active"] != float64(1) || got.Summary["Unchanged"] != float64(0) {
		t.Errorf("summary Active/Unchanged = %v/%v, want 1/0", got.Summary["Active"], got.Summary["Unchanged"])
	}
	excluded := 0
	for _, s := range got.Sessions {
		if s.ExcludedBySelection == nil {
			t.Fatalf("session %s has no excludedBySelection key:\n%s", s.SessionID, output)
		}
		if *s.ExcludedBySelection {
			excluded++
			if s.SessionID == kept {
				t.Errorf("selected session %s marked excluded", kept)
			}
		}
	}
	if excluded != 3 {
		t.Errorf("per-session rows mark %d sessions excluded, want 3:\n%s", excluded, output)
	}

	conn, err := sqlite.OpenConn(string(defaults.ResolveDBFilePathWith(dir)), sqlite.OpenReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var unchanged int64 = -1
	if err := sqlitex.ExecuteTransient(conn, `SELECT sessions_unchanged FROM ingest_log ORDER BY id DESC LIMIT 1`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			unchanged = stmt.ColumnInt64(0)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if unchanged != 3 {
		t.Errorf("ingest_log sessions_unchanged = %d, want 3 (excluded stay in unchanged so the stored counts add up)", unchanged)
	}
}
