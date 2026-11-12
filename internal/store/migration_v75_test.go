package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitemigration"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// TestMigrationV75AdmitsImageAttachments proves a v74 database keeps its
// entries, search index, and foreign keys, accepts an image entry, rejects the
// old closed set, and gains session_attachments.
func TestMigrationV75AdmitsImageAttachments(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "v74.db")
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitemigration.Migrate(t.Context(), conn, frozenSchema(74)); err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteScript(conn, `
INSERT INTO host_slugs(opaque_id, host_slug) VALUES('image-host', 'image-host');
INSERT INTO projects(project_hash) VALUES('image-project');
INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format)
VALUES('11111111-1111-4111-8111-111111111111', 'claude-code', 'fixture-model', 'image-host', 'image-project', 1, 2, 3, '/synthetic/image.jsonl', 'jsonl');
INSERT INTO session_entries(session_id, entry_index, provider, entry_type, role, depth, content_preview)
VALUES('11111111-1111-4111-8111-111111111111', 0, 'claude-code', 'text', 'user', 0, 'keep me');
`, nil); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := openUpgraded(path, WithPoolSize(1))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	conn, err = s.Pool().Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Pool().Put(conn)

	var preview string
	if err := sqlitex.ExecuteTransient(conn, `SELECT content_preview FROM session_entries WHERE session_id = ? AND entry_index = 0`, &sqlitex.ExecOptions{
		Args: []any{"11111111-1111-4111-8111-111111111111"},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			preview = stmt.ColumnText(0)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if preview != "keep me" {
		t.Fatalf("migrated preview = %q, want the v74 text preserved", preview)
	}
	if err := sqlitex.ExecuteTransient(conn, `
INSERT INTO session_entries(session_id, entry_index, provider, entry_type, role, depth)
VALUES('11111111-1111-4111-8111-111111111111', 1, 'claude-code', 'image', 'user', 1)`, nil); err != nil {
		t.Fatalf("entry_type image was rejected after v75: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn, `
INSERT INTO session_entries(session_id, entry_index, provider, entry_type, role, depth)
VALUES('11111111-1111-4111-8111-111111111111', 2, 'claude-code', 'not-a-type', 'user', 1)`, nil); err == nil {
		t.Fatal("an entry type outside the closed set was accepted")
	}
	var table string
	if err := sqlitex.ExecuteTransient(conn, `SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'session_attachments'`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			table = stmt.ColumnText(0)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if table != "session_attachments" {
		t.Fatal("session_attachments was not created")
	}
	var fts string
	if err := sqlitex.ExecuteTransient(conn, `SELECT name FROM sqlite_master WHERE name = 'session_search_fts'`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			fts = stmt.ColumnText(0)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if fts == "" {
		t.Fatal("the search index was not recreated")
	}
	if err := sqlitex.ExecuteTransient(conn, `
INSERT INTO session_attachments(session_id, source_entry_ref, partition_id)
VALUES('missing-session', 'img-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'main')`, nil); err == nil {
		t.Fatal("session_attachments accepted a row whose session does not exist")
	}
}
