package store

import (
	"context"
	"path/filepath"
	"testing"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitemigration"
	"zombiezen.com/go/sqlite/sqlitex"
)

func TestDebugLegacyAlterTable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	pool15, err := sqlitex.NewPool(dbPath, sqlitex.PoolOptions{PrepareConn: preparePragmas})
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	conn15, err := pool15.Take(ctx)
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	schemaV15 := sqlitemigration.Schema{Migrations: dbSchema.Migrations[:15]}
	if err := sqlitemigration.Migrate(ctx, conn15, schemaV15); err != nil {
		t.Fatalf("V1-V15: %v", err)
	}
	pool15.Put(conn15)
	pool15.Close()

	pool, err := sqlitex.NewPool(dbPath, sqlitex.PoolOptions{PrepareConn: preparePragmas})
	if err != nil {
		t.Fatalf("pool2: %v", err)
	}
	conn, err := pool.Take(ctx)
	if err != nil {
		t.Fatalf("take2: %v", err)
	}
	defer func() { pool.Put(conn); pool.Close() }()

	// Check current legacy_alter_table value
	if err := sqlitex.ExecuteTransient(conn, "PRAGMA legacy_alter_table", &sqlitex.ExecOptions{
		ResultFunc: func(s *sqlite.Stmt) error {
			t.Logf("legacy_alter_table (before) = %d", s.ColumnInt(0))
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Set it ON outside a transaction
	if err := sqlitex.ExecuteTransient(conn, "PRAGMA legacy_alter_table = ON", nil); err != nil {
		t.Fatalf("set legacy_alter_table: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn, "PRAGMA legacy_alter_table", &sqlitex.ExecOptions{
		ResultFunc: func(s *sqlite.Stmt) error {
			t.Logf("legacy_alter_table (after SET, before txn) = %d", s.ColumnInt(0))
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Start a savepoint
	if err := sqlitex.ExecuteTransient(conn, "SAVEPOINT test", nil); err != nil {
		t.Fatalf("savepoint: %v", err)
	}
	defer func() {
		_ = sqlitex.ExecuteTransient(conn, "ROLLBACK TO SAVEPOINT test", nil)
		_ = sqlitex.ExecuteTransient(conn, "RELEASE SAVEPOINT test", nil)
	}()

	if err := sqlitex.ExecuteTransient(conn, "PRAGMA legacy_alter_table", &sqlitex.ExecOptions{
		ResultFunc: func(s *sqlite.Stmt) error {
			t.Logf("legacy_alter_table (inside txn) = %d", s.ColumnInt(0))
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Try rename inside savepoint with legacy_alter_table ON
	if err := sqlitex.ExecuteTransient(conn, "ALTER TABLE annotation_classes RENAME TO _old_annotation_classes", nil); err != nil {
		t.Fatalf("rename: %v", err)
	}

	// Check FK in annotation_families after rename
	if err := sqlitex.ExecuteTransient(conn, "PRAGMA foreign_key_list(annotation_families)", &sqlitex.ExecOptions{
		ResultFunc: func(s *sqlite.Stmt) error {
			t.Logf("annotation_families FK after rename: table=%s from=%s", s.ColumnText(2), s.ColumnText(3))
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
}
