package testfixture

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/opencode_native_mutations.yaml
var nativeMutationsYAML []byte

type nativeMutation struct {
	Name           string `yaml:"name"`
	SessionID      string `yaml:"session_id"`
	MessageID      string `yaml:"message_id"`
	Text           string `yaml:"text"`
	TimeCreated    int64  `yaml:"time_created"`
	FileModifiedMS int64  `yaml:"file_modified_ms"`
}

// PrepareNativeCLI relocates synthetic project attribution beneath the source's
// materializer-owned root, so the production Git resolver cannot inspect real
// projects. Both possible metadata authorities receive the same confined path.
func PrepareNativeCLI(t testing.TB, source MaterializedSource) string {
	t.Helper()
	conn := openNativeSetup(t, source)
	defer func() {
		closeNativeSetup(t, conn)
		// The synthetic source is quiescent before initial import; its creation
		// by this test must not accidentally exercise the active-session gate.
		setNativeFileTime(t, source, time.Unix(0, 0))
	}()
	project := filepath.Join(source.root, "project")
	if err := requireConfinedPath(source.root, project); err != nil {
		t.Fatalf("confine synthetic native project directory: %v", err)
	}
	if err := os.Mkdir(project, 0o700); err != nil {
		t.Fatalf("create synthetic native project directory: %v", err)
	}
	if nativeTableExists(t, conn, "session") {
		if err := sqlitex.Execute(conn, "UPDATE session SET directory = ?", &sqlitex.ExecOptions{Args: []any{project}}); err != nil {
			t.Fatalf("relocate synthetic legacy project attribution: %v", err)
		}
	}
	if nativeTableExists(t, conn, "session_v2") {
		if err := sqlitex.Execute(conn, "UPDATE session_v2 SET directory = ?", &sqlitex.ExecOptions{Args: []any{project}}); err != nil {
			t.Fatalf("relocate synthetic v2 project attribution: %v", err)
		}
	}
	return project
}

// ApplyNativeMutationByName changes only a named embedded native-user fixture.
// It accepts no SQL or filesystem path and rejects sources not created by the
// materializer before opening a write connection.
func ApplyNativeMutationByName(t testing.TB, source MaterializedSource, name string) {
	t.Helper()
	var corpus struct {
		Mutations []nativeMutation `yaml:"mutations"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(nativeMutationsYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&corpus); err != nil {
		t.Fatalf("decode named native mutations: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("decode named native mutations: expected one YAML document, got %v", err)
	}
	var selected *nativeMutation
	seen := make(map[string]bool)
	for i := range corpus.Mutations {
		m := &corpus.Mutations[i]
		if m.Name == "" || seen[m.Name] || m.SessionID == "" || m.MessageID == "" || m.Text == "" || m.TimeCreated <= 0 || m.FileModifiedMS <= 0 {
			t.Fatalf("invalid or duplicate named native mutation %q", m.Name)
		}
		seen[m.Name] = true
		if m.Name == name {
			selected = m
		}
	}
	if selected == nil {
		t.Fatalf("native mutation %q is not in the embedded corpus", name)
	}
	conn := openNativeSetup(t, source)
	defer func() {
		closeNativeSetup(t, conn)
		setNativeFileTime(t, source, time.UnixMilli(selected.FileModifiedMS))
	}()
	// Advance the native session clock beyond the completed import. Aging only
	// the file mtime makes this a quiescent update, so reindexing must follow
	// the logical source clock rather than --include-active forcing a pass.
	updatedMS := time.Now().UnixMilli()
	payload := struct {
		Text string `json:"text"`
		Time struct {
			Created int64 `json:"created"`
		} `json:"time"`
	}{Text: selected.Text}
	payload.Time.Created = selected.TimeCreated
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode named native user mutation: %v", err)
	}
	if err := sqlitex.Execute(conn, "UPDATE session_message SET data = ?, time_updated = ? WHERE id = ? AND session_id = ? AND type = 'user'", &sqlitex.ExecOptions{Args: []any{string(data), updatedMS, selected.MessageID, selected.SessionID}}); err != nil {
		t.Fatalf("apply named synthetic native message update: %v", err)
	}
	if conn.Changes() != 1 {
		t.Fatalf("native mutation %q did not update exactly its named user row", name)
	}
	if nativeTableExists(t, conn, "session") {
		if err := sqlitex.Execute(conn, "UPDATE session SET time_updated = ? WHERE id = ?", &sqlitex.ExecOptions{Args: []any{updatedMS, selected.SessionID}}); err != nil {
			t.Fatalf("advance synthetic legacy session clock: %v", err)
		}
	}
	if nativeTableExists(t, conn, "session_v2") {
		if err := sqlitex.Execute(conn, "UPDATE session_v2 SET time_updated = ? WHERE id = ?", &sqlitex.ExecOptions{Args: []any{updatedMS, selected.SessionID}}); err != nil {
			t.Fatalf("advance synthetic v2 session clock: %v", err)
		}
	}
}

func setNativeFileTime(t testing.TB, source MaterializedSource, modified time.Time) {
	t.Helper()
	if err := requireReadableConfinedPath(source.root, source.Path, false); err != nil {
		t.Fatalf("confine synthetic source timestamp update: %v", err)
	}
	if err := os.Chtimes(source.Path, modified, modified); err != nil {
		t.Fatalf("set quiescent synthetic source timestamp: %v", err)
	}
}

func openNativeSetup(t testing.TB, source MaterializedSource) *sqlite.Conn {
	t.Helper()
	// Snapshot validation covers the DB and both sidecars, including symlinks,
	// before SQLite can write to any of them.
	if _, err := snapshotSource(source); err != nil {
		t.Fatalf("refuse native fixture mutation outside materializer-owned regular files: %v", err)
	}
	conn, err := sqlite.OpenConn(source.Path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open confined synthetic source for named fixture preparation: %v", err)
	}
	return conn
}

func closeNativeSetup(t testing.TB, conn *sqlite.Conn) {
	t.Helper()
	if err := conn.Close(); err != nil {
		t.Errorf("close synthetic native source before production run: %v", err)
	}
}

func nativeTableExists(t testing.TB, conn *sqlite.Conn, table string) bool {
	t.Helper()
	found := false
	if err := sqlitex.Execute(conn, "SELECT 1 FROM sqlite_schema WHERE type = 'table' AND name = ?", &sqlitex.ExecOptions{Args: []any{table}, ResultFunc: func(*sqlite.Stmt) error { found = true; return nil }}); err != nil {
		t.Fatalf("inspect synthetic native session table: %v", err)
	}
	return found
}
