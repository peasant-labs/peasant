package ingest_test

import (
	"testing"

	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
)

// openPreparedStore opens the store at path at the head schema, preparing
// the path first when it does not exist.
//
// A missing path is created as a copy of the pre-migrated golden database
// (parent directories included), so the open never lands on an empty,
// schema-less file. An existing path — one a pipeline run or a previous
// open already migrated — is reopened as is. The caller's options are passed
// through unchanged. See storetest.OpenPrepared for the shared implementation.
func openPreparedStore(t *testing.T, path string, options ...store.OpenOption) (*store.Store, error) {
	t.Helper()
	return storetest.OpenPrepared(t, path, options...)
}
