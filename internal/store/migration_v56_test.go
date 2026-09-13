package store

import (
	"bytes"
	_ "embed"
	"io"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitemigration"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/migrations/v52_index_inputs.yaml
var indexedInputMigrationYAML []byte

func TestMigrationV56PreservesHistoryWithoutInputProof(t *testing.T) {
	t.Parallel()
	// Reuse the populated, empty and absent legacy-index histories. Their
	// independently nullable producer/format/time values must remain unchanged.
	histories := loadIndexFormatMigrationFixtures(t)
	conn, err := sqlite.OpenConn(filepath.Join(t.TempDir(), "index-input.db"), sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	frozen := sqlitemigration.Schema{Migrations: dbSchema.Migrations[:55], MigrationOptions: dbSchema.MigrationOptions[:55]}
	if err := sqlitemigration.Migrate(t.Context(), conn, frozen); err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteScript(conn, "INSERT INTO projects (project_hash) VALUES ('input-project'); INSERT INTO host_slugs (opaque_id, host_slug) VALUES ('input-host', 'fixture-host');", nil); err != nil {
		t.Fatal(err)
	}
	for _, row := range histories {
		var indexedAt, format any
		if row.IndexedAt != nil {
			indexedAt = *row.IndexedAt
		}
		if row.WantFormat != nil {
			format = *row.WantFormat
		}
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO sessions
(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format, index_version, indexed_at, index_format_version)
VALUES (?, 'claude-code', 'fixture-model', 'input-host', 'input-project', 1, 2, 3, '/fixture/source.jsonl', 'jsonl', ?, ?, ?)`, &sqlitex.ExecOptions{Args: []any{row.SessionID, row.IndexerVersion, indexedAt, format}}); err != nil {
			t.Fatal(err)
		}
		if row.Entries {
			if err := sqlitex.ExecuteTransient(conn, "INSERT INTO session_entries (session_id, entry_index, provider, entry_type, role, content_preview) VALUES (?, 0, 'claude-code', 'text', 'user', 'preserved input')", &sqlitex.ExecOptions{Args: []any{row.SessionID}}); err != nil {
				t.Fatal(err)
			}
		}
		if err := sqlitex.ExecuteTransient(conn, "INSERT INTO index_log (session_id, provider, outcome, index_version, entries_count, started_at) VALUES (?, 'claude-code', 'indexed', ?, 0, 5)", &sqlitex.ExecOptions{Args: []any{row.SessionID, row.IndexerVersion}}); err != nil {
			t.Fatal(err)
		}
	}
	readHistory := func() [][]string {
		var rows [][]string
		if err := sqlitex.ExecuteTransient(conn, `SELECT s.session_id, s.index_version, s.indexed_at, s.index_format_version,
s.artifact_hash, s.session_entries_hash, e.content_preview, l.index_version, l.index_format_version
FROM sessions s LEFT JOIN session_entries e USING(session_id) LEFT JOIN index_log l USING(session_id) ORDER BY s.session_id`, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
			values := make([]string, stmt.ColumnCount())
			for i := range values {
				values[i] = stmt.ColumnType(i).String() + ":" + stmt.ColumnText(i)
			}
			rows = append(rows, values)
			return nil
		}}); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	before := readHistory()
	if err := sqlitemigration.Migrate(t.Context(), conn, dbSchema); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, readHistory()) {
		t.Fatal("migration changed stored representation or producer history")
	}
	if err := sqlitex.ExecuteTransient(conn, "SELECT indexed_input_hash FROM sessions", &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
		if stmt.ColumnType(0) != sqlite.TypeNull {
			t.Error("migration invented an indexed input proof")
		}
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name    string  `yaml:"name"`
			Hash    *string `yaml:"hash"`
			Refused bool    `yaml:"refused"`
		} `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(indexedInputMigrationYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatal("migration fixtures require one document")
	}
	seen := make(map[string]bool)
	for _, row := range fixture.Cases {
		if row.Name == "" || seen[row.Name] {
			t.Fatal("migration fixture has duplicate or missing name")
		}
		seen[row.Name] = true
		t.Run(row.Name, func(t *testing.T) {
			var hash any
			if row.Hash != nil {
				hash = *row.Hash
			}
			err := sqlitex.ExecuteTransient(conn, "UPDATE sessions SET indexed_input_hash = ? WHERE session_id = ?", &sqlitex.ExecOptions{Args: []any{hash, histories[0].SessionID}})
			if (err != nil) != row.Refused {
				t.Fatalf("input hash acceptance mismatch: err=%v refused=%t", err, row.Refused)
			}
		})
	}
	if len(fixture.RequiredNames) == 0 {
		t.Fatal("missing required-name manifest")
	}
	for _, name := range fixture.RequiredNames {
		if !seen[name] {
			t.Fatalf("missing required migration case %q", name)
		}
	}
}
