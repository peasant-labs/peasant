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

//go:embed testdata/migrations/v50_index_formats.yaml
var indexFormatMigrationYAML []byte

type indexFormatMigrationCase struct {
	Name           string `yaml:"name"`
	SessionID      string `yaml:"sessionID"`
	IndexerVersion int    `yaml:"indexerVersion"`
	IndexedAt      *int64 `yaml:"indexedAt"`
	Entries        bool   `yaml:"entries"`
	WantFormat     *int   `yaml:"wantFormat"`
}

func loadIndexFormatMigrationFixtures(t *testing.T) []indexFormatMigrationCase {
	t.Helper()
	var document struct {
		RequiredNames []string                   `yaml:"requiredNames"`
		Cases         []indexFormatMigrationCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(indexFormatMigrationYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		t.Fatal(err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("index format migration fixture must have one document: %v", err)
	}
	seen := make(map[string]bool)
	for _, row := range document.Cases {
		if row.Name == "" || row.SessionID == "" || seen[row.Name] {
			t.Fatalf("invalid or duplicate index format migration row: %+v", row)
		}
		seen[row.Name] = true
	}
	if len(document.RequiredNames) == 0 {
		t.Fatal("index format migration requires a named deletion-protection manifest")
	}
	for _, name := range document.RequiredNames {
		if !seen[name] {
			t.Fatalf("missing required index format migration case %q", name)
		}
	}
	return document.Cases
}

func TestMigrationV54PreservesIndexProducerHistory(t *testing.T) {
	t.Parallel()
	cases := loadIndexFormatMigrationFixtures(t)
	path := filepath.Join(t.TempDir(), "legacy.db")
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	frozen := sqlitemigration.Schema{Migrations: dbSchema.Migrations[:53], MigrationOptions: dbSchema.MigrationOptions[:53]}
	if err := sqlitemigration.Migrate(t.Context(), conn, frozen); err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteScript(conn, `
INSERT INTO projects (project_hash) VALUES ('format-project');
INSERT INTO host_slugs (opaque_id, host_slug) VALUES ('format-host', 'fixture-host');
`, nil); err != nil {
		t.Fatal(err)
	}
	for _, row := range cases {
		var indexedAt any
		if row.IndexedAt != nil {
			indexedAt = *row.IndexedAt
		}
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO sessions
(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format, index_version, indexed_at)
VALUES (?, 'claude-code', 'fixture-model', 'format-host', 'format-project', 1, 2, 3, '/fixture/source.jsonl', 'jsonl', ?, ?)`, &sqlitex.ExecOptions{Args: []any{row.SessionID, row.IndexerVersion, indexedAt}}); err != nil {
			t.Fatal(err)
		}
		if row.Entries {
			if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_entries
(session_id, entry_index, provider, entry_type, role, content_preview)
VALUES (?, 0, 'claude-code', 'text', 'user', 'preserve legacy projection')`, &sqlitex.ExecOptions{Args: []any{row.SessionID}}); err != nil {
				t.Fatal(err)
			}
		}
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO index_log
(session_id, provider, outcome, index_version, entries_count, reason, started_at, finished_at)
VALUES (?, 'claude-code', 'skipped', ?, 0, 'legacy attempt', 11, 12)`, &sqlitex.ExecOptions{Args: []any{row.SessionID, row.IndexerVersion}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(path, WithPoolSize(1))
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	conn, err = upgraded.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Pool().Put(conn)
	for _, row := range cases {
		t.Run(row.Name, func(t *testing.T) {
			found := false
			if err := sqlitex.ExecuteTransient(conn, `SELECT index_version, indexed_at, index_format_version,
(SELECT COUNT(*) FROM session_entries WHERE session_id = sessions.session_id)
FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
				Args: []any{row.SessionID}, ResultFunc: func(stmt *sqlite.Stmt) error {
					found = true
					var indexedAt *int64
					var format *int
					if stmt.ColumnType(1) != sqlite.TypeNull {
						value := stmt.ColumnInt64(1)
						indexedAt = &value
					}
					if stmt.ColumnType(2) != sqlite.TypeNull {
						value := stmt.ColumnInt(2)
						format = &value
					}
					if stmt.ColumnInt(0) != row.IndexerVersion || !reflect.DeepEqual(indexedAt, row.IndexedAt) || !reflect.DeepEqual(format, row.WantFormat) || (stmt.ColumnInt(3) > 0) != row.Entries {
						t.Errorf("migration altered producer evidence or misidentified format: parser=%d time=%v format=%v, want %+v", stmt.ColumnInt(0), indexedAt, format, row)
					}
					return nil
				},
			}); err != nil {
				t.Fatal(err)
			}
			if !found {
				t.Fatal("migration lost session")
			}
			if err := sqlitex.ExecuteTransient(conn, `SELECT index_version, index_format_version, reason, started_at, finished_at FROM index_log WHERE session_id = ?`, &sqlitex.ExecOptions{
				Args: []any{row.SessionID}, ResultFunc: func(stmt *sqlite.Stmt) error {
					if stmt.ColumnInt(0) != row.IndexerVersion || stmt.ColumnType(1) != sqlite.TypeNull || stmt.ColumnText(2) != "legacy attempt" || stmt.ColumnInt64(3) != 11 || stmt.ColumnInt64(4) != 12 {
						t.Error("migration changed a historic indexing attempt")
					}
					return nil
				},
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
