package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// TestHarvestIngestsATranscriptThatStartsWithAUTF8BOM proves a byte order mark
// before the first record no longer fails the whole session on first ingest:
// the session is stored with every record, the first one included.
func TestHarvestIngestsATranscriptThatStartsWithAUTF8BOM(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(t.TempDir(), "projects")
	sid := "bbbbbbbb-0000-4000-8000-000000000679"
	body := "\xef\xbb\xbf"
	for i := 0; i < 3; i++ {
		body += fmt.Sprintf(`{"type":"user","sessionId":"%s","uuid":"u%d","cwd":"/tmp/qa-bom","timestamp":"2026-10-01T10:0%d:00Z","message":{"role":"user","content":"turn %d"}}`+"\n", sid, i, i, i)
	}
	if err := os.MkdirAll(filepath.Join(src, "-tmp-qa-bom"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "-tmp-qa-bom", sid+".jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := fmt.Sprintf("version: 1\nsources:\n  claude-code:\n    enabled: true\n    paths:\n      - %s\n  opencode: {enabled: false}\n  cursor: {enabled: false}\n  codex: {enabled: false}\n  strike: {enabled: false}\n  pi: {enabled: false}\noutput:\n  basePath: %s\n", src, filepath.Join(dir, "out"))
	if err := os.WriteFile(configPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := executeHarvestCmd(t, dir, []string{"--config=" + configPath})
	if err != nil {
		t.Fatalf("harvest failed a transcript whose only oddity is a leading BOM: %v\n%s", err, output)
	}
	conn, err := sqlite.OpenConn(string(defaults.ResolveDBFilePathWith(dir)), sqlite.OpenReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var sessions, entries, first int
	if err := sqlitex.ExecuteTransient(conn, `SELECT (SELECT COUNT(*) FROM sessions WHERE session_id = ?1), (SELECT COUNT(*) FROM session_entries WHERE session_id = ?1), (SELECT COUNT(*) FROM session_entries WHERE session_id = ?1 AND content_preview LIKE '%turn 0%')`, &sqlitex.ExecOptions{
		Args: []any{sid},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			sessions, entries, first = stmt.ColumnInt(0), stmt.ColumnInt(1), stmt.ColumnInt(2)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if sessions != 1 {
		t.Fatalf("session %s stored %d times, want once:\n%s", sid, sessions, output)
	}
	if entries < 3 {
		t.Fatalf("session %s stored %d entries, want all 3 records:\n%s", sid, entries, output)
	}
	if first == 0 {
		t.Fatalf("session %s did not store the record that follows the BOM:\n%s", sid, output)
	}
}
