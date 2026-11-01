package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// TestVerifyContentFailsOnAMissingTable guards #682: `harvest verify
// --content` must run the table presence check that plain `verify` runs, so
// a dropped table fails it instead of reporting PASSED.
func TestVerifyContentFailsOnAMissingTable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := fmt.Sprintf("version: 1\nsources:\n  claude-code:\n    enabled: true\n    paths:\n      - %s\n  opencode: {enabled: false}\n  cursor: {enabled: false}\n  codex: {enabled: false}\n  strike: {enabled: false}\n  pi: {enabled: false}\noutput:\n  basePath: %s\n", t.TempDir(), filepath.Join(dir, "out"))
	if err := os.WriteFile(configPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := executeHarvestCmd(t, dir, []string{"--config=" + configPath}); err != nil {
		t.Fatalf("seed harvest: %v\n%s", err, out)
	}
	conn, err := sqlite.OpenConn(string(defaults.ResolveDBFilePathWith(dir)))
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteScript(conn, `DROP TABLE session_files;`, nil); err != nil {
		t.Fatal(err)
	}
	conn.Close()

	for _, args := range [][]string{{"verify"}, {"verify", "--content"}} {
		out, err := executeHarvestCmd(t, dir, append([]string{"--config=" + configPath}, args...))
		if err == nil || strings.Contains(out, "Status: PASSED") {
			t.Errorf("harvest %v with session_files dropped: err = %v, want a failure and no PASSED:\n%s", args, err, out)
		}
		if !strings.Contains(out, "session_files - MISSING") {
			t.Errorf("harvest %v did not name the missing table:\n%s", args, out)
		}
	}
}
