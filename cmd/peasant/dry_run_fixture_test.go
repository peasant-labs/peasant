package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
)

// seedClosedStore prepares the ONE database state a dry run may inspect: an
// existing, migrated, checkpointed file with no live sidecars, under the data
// directory the command is given.
//
// A dry run does not create or migrate state and does not touch a journal, so a
// test that expects a successful forecast has to arrange that state itself. The
// tests that instead prove the refusals — a missing database, or one with a live
// write-ahead log — must NOT call this.
//
// It asserts the sidecars are gone after Close, because a dry run that found one
// would refuse, and the refusal would then look like a product defect rather than
// a fixture that never checkpointed.
func seedClosedStore(t testing.TB, dir string) string {
	t.Helper()
	path := string(defaults.ResolveDBFilePathWith(dir))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("prepare the data directory for a dry-run fixture: %v", err)
	}
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open the database a dry run will inspect: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close the database a dry run will inspect: %v", err)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(path + suffix); err == nil {
			t.Fatalf("the dry-run fixture left %s behind; a dry run refuses a database that is not checkpointed, so this fixture would measure the refusal instead of the forecast", filepath.Base(path+suffix))
		} else if !os.IsNotExist(err) {
			t.Fatalf("check the dry-run fixture's sidecars: %v", err)
		}
	}
	return path
}
