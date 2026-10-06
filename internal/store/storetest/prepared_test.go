package storetest

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

func TestOpenPreparedPreparesMissingPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "prepared.db")
	db, err := OpenPrepared(t, path)
	if err != nil {
		t.Fatalf("OpenPrepared on a missing path: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("prepared path was not created: %v", err)
	}
	if version, err := store.SchemaVersionAt(path); err != nil || version != store.CurrentSchemaVersion() {
		t.Fatalf("prepared copy version = %d, err = %v; want %d", version, err, store.CurrentSchemaVersion())
	}
}

func TestCopyGoldenToIfAbsentKeepsExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keep.db")
	db, err := OpenPrepared(t, path)
	if err != nil {
		t.Fatalf("first OpenPrepared: %v", err)
	}
	// Make the file observably differ from the golden template: a marker table
	// the template does not carry. An always-overwrite implementation loses it.
	ctx := context.Background()
	conn, err := db.Pool().Take(ctx)
	if err != nil {
		t.Fatalf("take a connection: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn, `CREATE TABLE keep_marker (x INTEGER);`, nil); err != nil {
		db.Pool().Put(conn)
		t.Fatalf("create the marker table: %v", err)
	}
	db.Pool().Put(conn)
	if err := db.Close(); err != nil {
		t.Fatalf("close the prepared store: %v", err)
	}

	CopyGoldenToIfAbsent(t, path)

	db, err = store.Open(path, store.WithSkipMigrations())
	if err != nil {
		t.Fatalf("reopen the prepared file: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	conn, err = db.Pool().Take(ctx)
	if err != nil {
		t.Fatalf("take a connection after the second call: %v", err)
	}
	defer db.Pool().Put(conn)
	found := false
	err = sqlitex.ExecuteTransient(conn, `SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'keep_marker'`, &sqlitex.ExecOptions{
		ResultFunc: func(*sqlite.Stmt) error { found = true; return nil },
	})
	if err != nil {
		t.Fatalf("query the marker table: %v", err)
	}
	if !found {
		t.Fatal("an existing prepared path was overwritten")
	}
}
