package store

import (
	_ "embed"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitemigration"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/source_availability.yaml
var sourceAvailabilityYAML []byte

type sourceAvailabilityCase struct {
	Name   string `yaml:"name"`
	Reason string `yaml:"reason"`
	Valid  bool   `yaml:"valid"`
}

func LoadSourceAvailabilityFixtures(t *testing.T) []sourceAvailabilityCase {
	t.Helper()
	var doc struct {
		RequiredNames []string                 `yaml:"requiredNames"`
		Cases         []sourceAvailabilityCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(sourceAvailabilityYAML, &doc); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, row := range doc.Cases {
		if row.Name == "" || names[row.Name] {
			t.Fatalf("invalid source availability fixture %q", row.Name)
		}
		names[row.Name] = true
	}
	if len(doc.RequiredNames) == 0 {
		t.Fatal("source availability requires named deletion protection")
	}
	for _, name := range doc.RequiredNames {
		if !names[name] {
			t.Fatalf("missing source availability fixture %q", name)
		}
	}
	return doc.Cases
}

func TestMigrationV63SourceAvailability(t *testing.T) {
	t.Parallel()
	conn, err := sqlite.OpenConn(filepath.Join(t.TempDir(), "upgrade.db"), sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	predecessor := sqlitemigration.Schema{Migrations: dbSchema.Migrations[:62], MigrationOptions: dbSchema.MigrationOptions[:62]}
	if err := sqlitemigration.Migrate(t.Context(), conn, predecessor); err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteScript(conn, `
INSERT INTO host_slugs(opaque_id, host_slug) VALUES('source-host','source-host');
INSERT INTO projects(project_hash) VALUES('source-project');
INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format)
VALUES('source-session','claude-code','fixture-model','source-host','source-project',1,2,3,'/synthetic/missing.jsonl','jsonl');
INSERT INTO session_entries(session_id, entry_index, provider, entry_type, role, content_preview)
VALUES('source-session',0,'claude-code','text','user','prior readable content');`, nil); err != nil {
		t.Fatal(err)
	}
	if err := sqlitemigration.Migrate(t.Context(), conn, dbSchema); err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteTransient(conn, `PRAGMA foreign_keys = ON`, nil); err != nil {
		t.Fatal(err)
	}
	for _, row := range LoadSourceAvailabilityFixtures(t) {
		t.Run(row.Name, func(t *testing.T) {
			_, validationErr := ingest.NewSourceUnavailabilityReason(row.Reason)
			if (validationErr == nil) != row.Valid {
				t.Fatalf("constructor: %v, valid=%t", validationErr, row.Valid)
			}
			err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_source_unavailability(session_id,reason) VALUES('source-session',?)`, &sqlitex.ExecOptions{Args: []any{row.Reason}})
			if (err == nil) != row.Valid {
				t.Fatalf("database constraint: %v, valid=%t", err, row.Valid)
			}
			if err := sqlitex.ExecuteTransient(conn, `DELETE FROM session_source_unavailability`, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_source_unavailability VALUES('absent-session',?)`, &sqlitex.ExecOptions{Args: []any{string(ingest.SourceUnavailableNoSavedCopy)}}); err == nil {
		t.Fatal("orphan availability accepted")
	}
	found := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT source_path, content_preview FROM sessions JOIN session_entries USING(session_id) WHERE session_id='source-session'`, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
		found = true
		if stmt.ColumnText(0) != "/synthetic/missing.jsonl" || stmt.ColumnText(1) != "prior readable content" {
			t.Fatal("migration changed prior stored content")
		}
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("migration lost prior readable content")
	}
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_source_unavailability VALUES('source-session',?)`, &sqlitex.ExecOptions{Args: []any{string(ingest.SourceUnavailableNoSavedCopy)}}); err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteScript(conn, `DELETE FROM session_entries WHERE session_id='source-session'; DELETE FROM sessions WHERE session_id='source-session';`, nil); err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteTransient(conn, `SELECT session_id FROM session_source_unavailability`, &sqlitex.ExecOptions{ResultFunc: func(*sqlite.Stmt) error { t.Fatal("session deletion left unavailable state"); return nil }}); err != nil {
		t.Fatal(err)
	}
}
