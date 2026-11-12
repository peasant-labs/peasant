package storetest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
)

// CopyGoldenToIfAbsent prepares path for a skip-migrations open: a missing
// file is created as a copy of the pre-migrated golden database (parent
// directories included); an existing file is left untouched, so a reopen
// keeps the rows a previous open or seed wrote.
func CopyGoldenToIfAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(path), defaults.PrivateDirPerm); err != nil {
			t.Fatalf("prepare the data directory for %s: %v", path, err)
		}
		CopyGoldenTo(t, path)
	} else if err != nil {
		t.Fatalf("stat the test database %s: %v", path, err)
	}
}

// OpenPrepared opens the store at path without replaying the migration-state
// check, preparing the path first when it does not exist.
//
// A missing path is created as a copy of the golden database at the head
// schema, so the open never builds a store from the baseline. An existing
// path — one a pipeline run or a previous open already created — is reopened
// as is. The caller's options are passed through unchanged.
func OpenPrepared(t *testing.T, path string, options ...store.OpenOption) (*store.Store, error) {
	t.Helper()
	CopyGoldenToIfAbsent(t, path)
	return store.Open(path, options...)
}
