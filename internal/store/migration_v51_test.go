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

//go:embed testdata/migrations/v51_artifact_inputs.yaml
var artifactInputMigrationYAML []byte

func TestMigrationV51DoesNotInventRetainedEvidence(t *testing.T) {
	t.Parallel()
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name           string `yaml:"name"`
			SessionID      string `yaml:"sessionID"`
			ComputeVersion int    `yaml:"computeVersion"`
			ComputedAt     *int64 `yaml:"computedAt"`
			InputTokens    int    `yaml:"inputTokens"`
		} `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(artifactInputMigrationYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatal("artifact migration fixture requires one YAML document")
	}
	required := []string{"computed-history", "uncomputed-history"}
	if !reflect.DeepEqual(fixture.RequiredNames, required) {
		t.Fatal("artifact migration required-name manifest changed")
	}
	seen := make(map[string]bool)
	path := filepath.Join(t.TempDir(), "legacy.db")
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	frozen := sqlitemigration.Schema{Migrations: dbSchema.Migrations[:50], MigrationOptions: dbSchema.MigrationOptions[:50]}
	if err := sqlitemigration.Migrate(t.Context(), conn, frozen); err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteScript(conn, `INSERT INTO projects (project_hash) VALUES ('artifact-project');
INSERT INTO host_slugs (opaque_id, host_slug) VALUES ('artifact-host', 'fixture-host');`, nil); err != nil {
		t.Fatal(err)
	}
	for _, row := range fixture.Cases {
		if row.Name == "" || seen[row.Name] {
			t.Fatalf("invalid migration fixture %q", row.Name)
		}
		seen[row.Name] = true
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO sessions
(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format, index_version, indexed_at)
VALUES (?, 'claude-code', 'fixture-model', 'artifact-host', 'artifact-project', 1, 2, 3, '/fixture/source.jsonl', 'jsonl', 15, 4)`, &sqlitex.ExecOptions{Args: []any{row.SessionID}}); err != nil {
			t.Fatal(err)
		}
		var computedAt any
		if row.ComputedAt != nil {
			computedAt = *row.ComputedAt
		}
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_metrics (session_id, compute_version, computed_at, input_tokens) VALUES (?, ?, ?, ?)`, &sqlitex.ExecOptions{Args: []any{row.SessionID, row.ComputeVersion, computedAt, row.InputTokens}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range required {
		if !seen[name] {
			t.Fatalf("missing migration fixture %q", name)
		}
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(path, WithPoolSize(1))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err = db.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool().Put(conn)
	for _, row := range fixture.Cases {
		t.Run(row.Name, func(t *testing.T) {
			found := false
			if err := sqlitex.ExecuteTransient(conn, `SELECT s.adapter_version, s.artifact_hash, s.metric_seed_json, s.index_version, m.compute_version, m.computed_at, m.input_tokens
FROM sessions s JOIN session_metrics m USING(session_id) WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{row.SessionID}, ResultFunc: func(stmt *sqlite.Stmt) error {
				found = true
				if stmt.ColumnType(0) != sqlite.TypeNull || stmt.ColumnType(1) != sqlite.TypeNull || stmt.ColumnType(2) != sqlite.TypeNull {
					t.Error("migration invented retained artifact/adapter/seed evidence")
				}
				var computedAt *int64
				if stmt.ColumnType(5) != sqlite.TypeNull {
					value := stmt.ColumnInt64(5)
					computedAt = &value
				}
				if stmt.ColumnInt(3) != 15 || stmt.ColumnInt(4) != row.ComputeVersion || !reflect.DeepEqual(computedAt, row.ComputedAt) || stmt.ColumnInt(6) != row.InputTokens {
					t.Error("migration changed historical producer/metrics state")
				}
				return nil
			}}); err != nil {
				t.Fatal(err)
			}
			if !found {
				t.Fatal("migration lost session")
			}
		})
	}
}
