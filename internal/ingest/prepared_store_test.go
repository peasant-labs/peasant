package ingest_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
)

// openPreparedStore opens the store at path without replaying the
// migration-state check, preparing the path first when it does not exist.
//
// A missing path is created as a copy of the pre-migrated golden database
// (parent directories included), so a skip-migrations open never lands on an
// empty, schema-less file. An existing path — one a pipeline run or a previous
// open already migrated — is reopened as is. The caller's options are passed
// through unchanged.
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
