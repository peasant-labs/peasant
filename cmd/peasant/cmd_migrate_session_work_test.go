package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

const (
	migrateSessionWorkSQL    = "testdata/migrate-session-work.sql"
	migrateSessionWorkGolden = "testdata/migrate-session-work.golden.json"
)

func migrateBehindFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	path := string(defaults.ResolveDBFilePathWith(dir))
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	sql, err := os.ReadFile(migrateSessionWorkSQL)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteScript(conn, string(sql), nil); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	return dir, path
}

func TestMigrateDryRunSessionWork(t *testing.T) {
	dir, path := migrateBehindFixture(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text, _, err := executeMigrateCmd(t, dir, filepath.Join(dir, "owned"), []string{"--dry-run"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"session work: 1 session(s)", "unknown evidence adapter_version: 1", "lower bound:", "run the dry run again"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in %s", want, text)
		}
	}
	output, stderr, err := executeMigrateCmd(t, dir, filepath.Join(dir, "owned"), []string{"--dry-run", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	if stderr != "" {
		t.Fatalf("unexpected stderr: %s", stderr)
	}
	dec := json.NewDecoder(strings.NewReader(output))
	var doc map[string]json.RawMessage
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		t.Fatalf("extra JSON output: %v", err)
	}
	if _, ok := doc["schema_upgrade"]; !ok {
		t.Fatal("missing schema plan")
	}
	var formatted bytes.Buffer
	if err := json.Indent(&formatted, doc["session_work"], "", "  "); err != nil {
		t.Fatal(err)
	}
	formatted.WriteByte('\n')
	golden, err := os.ReadFile(migrateSessionWorkGolden)
	if err != nil {
		t.Fatal(err)
	}
	golden = bytes.ReplaceAll(golden, []byte("\r\n"), []byte("\n"))
	if !bytes.Equal(formatted.Bytes(), golden) {
		t.Fatalf("session work golden mismatch:\n%s", formatted.String())
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("dry run changed database")
	}
}

func TestMigrateDryRunSessionWorkEvidenceError(t *testing.T) {
	dir, path := migrateBehindFixture(t)
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteScript(conn, "ALTER TABLE sessions RENAME COLUMN model_harness TO missing_harness;", nil); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	out, _, err := executeMigrateCmd(t, dir, filepath.Join(dir, "owned"), []string{"--dry-run"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "session work: unavailable:") || !strings.Contains(out, "model_harness") || !strings.Contains(out, "schema") {
		t.Fatalf("missing schema or evidence error: %s", out)
	}
	out, _, err = executeMigrateCmd(t, dir, filepath.Join(dir, "owned"), []string{"--dry-run", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Schema json.RawMessage `json:"schema_upgrade"`
		Work   sessionWork     `json:"session_work"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Schema) == 0 || doc.Work.Available || !strings.Contains(doc.Work.Error, "model_harness") {
		t.Fatalf("missing error or schema: %s", out)
	}
}
