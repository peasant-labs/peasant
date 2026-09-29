package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
)

// openPreparedStore opens the store at path the way a test seeds or inspects
// the database a command uses, without replaying the migration-state check.
//
// A missing path is first prepared as a copy of the pre-migrated golden
// database (parent directories included), so the command later finds zero
// pending migrations. An existing path — one a command already created and
// migrated through the production open, or one a previous seed prepared — is
// reopened as is. Either way the file is already at head, so the open skips
// migrations; the caller's options (index formats, pool size, generation
// artifacts) are passed through unchanged, and the process-wide pool override
// from TestMain stays in force.
//
// Fresh-install coverage is unaffected: a test that runs a command against an
// empty root before calling this still exercises the production migrating
// open inside the command.
func openPreparedStore(t *testing.T, path string, options ...store.OpenOption) (*store.Store, error) {
	t.Helper()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(path), defaults.PrivateDirPerm); err != nil {
			t.Fatalf("prepare the data directory for %s: %v", path, err)
		}
		storetest.CopyGoldenTo(t, path)
	} else if err != nil {
		t.Fatalf("stat the test database %s: %v", path, err)
	}
	return store.Open(path, append([]store.OpenOption{store.WithSkipMigrations()}, options...)...)
}
