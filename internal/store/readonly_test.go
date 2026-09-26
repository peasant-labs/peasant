package store_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/store"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// OpenReadOnly must inspect a database whose writer still holds it open, without
// rewriting the database file. The previous implementation refused any live WAL
// and buffered the whole file in memory first.
func TestOpenReadOnlyReadsWithLiveWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peasant.db")
	writer, err := store.Open(path)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	t.Cleanup(func() { _ = writer.Close() })

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	read, err := store.OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly with a live writer: %v", err)
	}
	if got, want := read.InstallationSalt(), writer.InstallationSalt(); got != want {
		t.Fatalf("read-only salt = %v, want %v", got, want)
	}
	if err := read.Close(); err != nil {
		t.Fatalf("close read-only store: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("OpenReadOnly rewrote the database file")
	}
}

// A schema this build does not understand is refused without migrating.
func TestOpenReadOnlyRefusesOtherSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peasant.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	stale := store.CurrentSchemaVersion() - 1
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteTransient(conn, fmt.Sprintf("PRAGMA user_version = %d", stale), nil); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := store.OpenReadOnly(path); err == nil {
		t.Fatal("OpenReadOnly accepted a non-current schema")
	}

	conn, err = sqlite.OpenConn(path, sqlite.OpenReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var version int
	if err := sqlitex.ExecuteTransient(conn, "PRAGMA user_version", &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
		version = stmt.ColumnInt(0)
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	if version != stale {
		t.Fatalf("refused dry-run migrated the schema: user_version = %d, want %d", version, stale)
	}
}

// A missing database is reported, never created.
func TestOpenReadOnlyRefusesMissingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	if _, err := store.OpenReadOnly(path); err == nil {
		t.Fatal("OpenReadOnly accepted a missing database")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OpenReadOnly created the database: stat err = %v", err)
	}
}
