package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// TestOwnerCreatedAfterUpgradeFromV81 proves an explicit upgrade bootstraps the
// owner row and the permanence file, so an upgraded store never waits for a
// later writable open.
func TestOwnerCreatedAfterUpgradeFromV81(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "v81.db")
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateFrozenSchema(t, conn, 81); err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(LocalOwnerFilePath(path)); !os.IsNotExist(err) {
		t.Fatalf("the V81 store already has an owner file: %v", err)
	}
	if _, err := Upgrade(context.Background(), path, UpgradeOptions{}); err != nil {
		t.Fatalf("upgrade the V81 store: %v", err)
	}
	// Assert the owner before any Open: Open bootstraps an owner itself, so a
	// later read cannot prove Upgrade created one.
	upgraded, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open the upgraded database directly: %v", err)
	}
	var ownerRows int
	if err := sqlitex.ExecuteTransient(upgraded, `SELECT COUNT(*) FROM local_owner`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error { ownerRows = int(stmt.ColumnInt64(0)); return nil },
	}); err != nil {
		_ = upgraded.Close()
		t.Fatalf("count local_owner after the upgrade: %v", err)
	}
	if err := upgraded.Close(); err != nil {
		t.Fatal(err)
	}
	if ownerRows != 1 {
		t.Fatalf("local_owner rows after Upgrade = %d, want 1 before any Open", ownerRows)
	}
	if _, err := os.Stat(LocalOwnerFilePath(path)); err != nil {
		t.Fatalf("owner file after the upgrade, before any Open: %v", err)
	}
}

// TestReadOnlyOpenCreatesNoOwner proves a read-only or immutable open never
// creates the owner row or the permanence file.
func TestReadOnlyOpenCreatesNoOwner(t *testing.T) {
	t.Parallel()
	for _, immutable := range []bool{false, true} {
		immutable := immutable
		name := "read-only"
		if immutable {
			name = "immutable-read-only"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "no-owner.db")
			writable, err := Open(path, WithSkipLocalOwnerBootstrap())
			if err != nil {
				t.Fatal(err)
			}
			if err := writable.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(LocalOwnerFilePath(path)); !os.IsNotExist(err) {
				t.Fatalf("the fixture already has an owner file: %v", err)
			}
			options := []OpenOption{}
			if immutable {
				options = append(options, WithImmutableReadOnly())
			}
			read, err := OpenReadOnlyWithOptions(path, options...)
			if err != nil {
				t.Fatalf("read-only open: %v", err)
			}
			defer read.Close()
			owner, err := read.LocalOwner(context.Background())
			if err != nil || owner != nil {
				t.Fatalf("read-only owner = %+v, %v; want nil and no write", owner, err)
			}
			if _, err := os.Stat(LocalOwnerFilePath(path)); !os.IsNotExist(err) {
				t.Fatalf("a read-only open created the owner file: %v", err)
			}
		})
	}
}
