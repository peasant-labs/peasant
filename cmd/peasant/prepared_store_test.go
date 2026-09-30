package main

import (
	"testing"

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
// from TestMain stays in force. See storetest.OpenPrepared for the shared
// implementation.
//
// Fresh-install coverage is unaffected: a test that runs a command against an
// empty root before calling this still exercises the production migrating
// open inside the command.
func openPreparedStore(t *testing.T, path string, options ...store.OpenOption) (*store.Store, error) {
	t.Helper()
	return storetest.OpenPrepared(t, path, options...)
}
