package store_test

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// OpenReadOnly must inspect a database whose writer still holds it open, without
// rewriting the database file. The previous implementation refused any live WAL
// and buffered the whole file in memory first.
func TestOpenReadOnlyReadsWithLiveWriter(t *testing.T) {
	path := storetest.CopyGoldenDB(t)
	writer, err := store.Open(path, store.WithSkipMigrations())
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
	path := storetest.CopyGoldenDB(t)
	s, err := store.Open(path, store.WithSkipMigrations())
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

// A dry-run store opened with the managed-generation reader reports the same
// capabilities a writable store would, so the run resolves the native harness
// targets instead of the retained baseline, and it still rewrites nothing.
func TestOpenReadOnlyWithGenerationOptionsAdvertisesManagedGenerationSupport(t *testing.T) {
	path := storetest.CopyGoldenDB(t)
	root := t.TempDir()

	artifacts, err := store.NewOSGenerationArtifactStoreExisting(root)
	if err != nil {
		t.Fatalf("open existing artifact root: %v", err)
	}
	locker, err := store.NewFileSessionLocker(root)
	if err != nil {
		t.Fatalf("open owned root for locks: %v", err)
	}

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	read, err := store.OpenReadOnlyWithOptions(path,
		store.WithIndexFormats(store.V2IndexFormat()),
		store.WithGenerationArtifacts(artifacts, locker),
	)
	if err != nil {
		t.Fatalf("OpenReadOnlyWithOptions with generation options: %v", err)
	}
	if !read.SupportsIndexFormat(2) {
		t.Fatal("read-only store with generation options does not report format 2 support")
	}
	if !read.GenerationSnapshotsSupported() {
		t.Fatal("read-only store with generation options does not report managed-generation support")
	}
	if err := read.Close(); err != nil {
		t.Fatalf("close read-only store: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("read-only store with generation options rewrote the database file")
	}
}

// The read-only artifact constructor never creates the owned root: a dry run
// against a store without one must resolve the retained baseline, not leave a
// new directory behind.
func TestNewOSGenerationArtifactStoreExistingDoesNotCreateRoot(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	if _, err := store.NewOSGenerationArtifactStoreExisting(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("constructor error = %v, want fs.ErrNotExist", err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("constructor created the owned root: stat err = %v", err)
	}
}
